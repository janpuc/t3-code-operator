package smbserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	DefaultPort         = 1445
	DefaultPollInterval = 5 * time.Second
	passwordLimit       = 1024
	stopTimeout         = 15 * time.Second
)

var (
	usernamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	shareNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
)

type Config struct {
	Username        string
	ShareName       string
	ServerIdentity  string
	PasswordFile    string
	WorkspacePath   string
	StateDirectory  string
	UnixUser        string
	Port            int
	ReadOnly        bool
	PollInterval    time.Duration
	SMBDBinary      string
	SMBPasswdBinary string
	NetBinary       string
	Logf            func(string, ...any)
}

type Server struct {
	config Config
}

func New(config Config) (*Server, error) {
	if config.Port == 0 {
		config.Port = DefaultPort
	}
	if config.PollInterval == 0 {
		config.PollInterval = DefaultPollInterval
	}
	if config.Logf == nil {
		config.Logf = log.Printf
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Server{config: config}, nil
}

func validateConfig(config Config) error {
	if !usernamePattern.MatchString(config.Username) {
		return errors.New("SMB username is invalid")
	}
	if !shareNamePattern.MatchString(config.ShareName) {
		return errors.New("SMB share name is invalid")
	}
	if config.ServerIdentity == "" || len(config.ServerIdentity) > 512 || strings.ContainsAny(config.ServerIdentity, "\x00\r\n") {
		return errors.New("SMB server identity is invalid")
	}
	if config.PasswordFile == "" || config.WorkspacePath == "" || config.StateDirectory == "" {
		return errors.New("SMB password, workspace, and state paths are required")
	}
	paths := []struct {
		name string
		path string
	}{
		{name: "password", path: config.PasswordFile},
		{name: "workspace", path: config.WorkspacePath},
		{name: "state", path: config.StateDirectory},
	}
	for _, value := range paths {
		if !validPath(value.path) || value.name == "state" && value.path == "/" {
			return fmt.Errorf("SMB %s path is invalid", value.name)
		}
	}
	if !usernamePattern.MatchString(config.UnixUser) {
		return errors.New("SMB UNIX user is invalid")
	}
	if config.Port < 1024 || config.Port > 65535 {
		return errors.New("SMB container port must be between 1024 and 65535")
	}
	if config.PollInterval <= 0 {
		return errors.New("SMB password poll interval must be positive")
	}
	if config.SMBDBinary == "" || config.SMBPasswdBinary == "" || config.NetBinary == "" {
		return errors.New("SMB server, password, and administration binaries are required")
	}
	return nil
}

func validPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}

func (server *Server) Run(ctx context.Context) error {
	configurationPath, err := server.prepare()
	if err != nil {
		return err
	}
	if err := server.setLocalSID(ctx, configurationPath); err != nil {
		return err
	}
	password, err := readPassword(server.config.PasswordFile)
	if err != nil {
		return err
	}
	if err := server.updatePassword(ctx, configurationPath, password); err != nil {
		return err
	}

	command := exec.Command(
		server.config.SMBDBinary,
		"--foreground",
		"--no-process-group",
		"--debug-stdout",
		"--configfile="+configurationPath,
		fmt.Sprintf("--port=%d", server.config.Port),
	)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start SMB server: %w", err)
	}

	wait := make(chan error, 1)
	go func() {
		wait <- command.Wait()
	}()
	ticker := time.NewTicker(server.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-wait:
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				return errors.New("SMB server exited")
			}
			return fmt.Errorf("SMB server exited: %w", err)
		case <-ticker.C:
			candidate, readErr := readPassword(server.config.PasswordFile)
			if readErr != nil {
				server.config.Logf("SMB password update is pending: %v", readErr)
				continue
			}
			if bytes.Equal(password, candidate) {
				continue
			}
			if updateErr := server.updatePassword(ctx, configurationPath, candidate); updateErr != nil {
				server.config.Logf("SMB password update is pending: %v", updateErr)
				continue
			}
			password = candidate
			server.config.Logf("SMB password updated")
		case <-ctx.Done():
			return stop(command, wait)
		}
	}
}

func (server *Server) prepare() (string, error) {
	directories := []string{
		server.config.StateDirectory,
		filepath.Join(server.config.StateDirectory, "cache"),
		filepath.Join(server.config.StateDirectory, "lock"),
		filepath.Join(server.config.StateDirectory, "pid"),
		filepath.Join(server.config.StateDirectory, "private"),
		filepath.Join(server.config.StateDirectory, "state"),
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return "", fmt.Errorf("create SMB state directory: %w", err)
		}
	}
	mapPath := filepath.Join(server.config.StateDirectory, "username.map")
	if err := writeFile(mapPath, []byte(server.config.UnixUser+" = "+server.config.Username+"\n")); err != nil {
		return "", fmt.Errorf("write SMB username map: %w", err)
	}
	configurationPath := filepath.Join(server.config.StateDirectory, "smb.conf")
	if err := writeFile(configurationPath, server.configuration(mapPath)); err != nil {
		return "", fmt.Errorf("write SMB configuration: %w", err)
	}
	return configurationPath, nil
}

func (server *Server) configuration(usernameMapPath string) []byte {
	readOnly := "no"
	if server.config.ReadOnly {
		readOnly = "yes"
	}
	state := server.config.StateDirectory
	return []byte(fmt.Sprintf(`[global]
server role = standalone server
netbios name = %s
security = user
map to guest = Never
server min protocol = SMB3
server smb encrypt = required
ntlm auth = ntlmv2-only
smb ports = %d
disable netbios = yes
dns proxy = no
load printers = no
printing = bsd
printcap name = /dev/null
logging = stdout
max log size = 0
passdb backend = tdbsam:%s
username map = %s
private dir = %s
state directory = %s
cache directory = %s
lock directory = %s
pid directory = %s
smb2 leases = no
oplocks = no
level2 oplocks = no
wide links = no

[%s]
path = %s
browseable = yes
guest ok = no
valid users = %s
force user = %s
force group = %s
read only = %s
create mask = 0664
directory mask = 0775
map archive = no
map hidden = no
map system = no
map readonly = no
`,
		server.netBIOSName(),
		server.config.Port,
		filepath.Join(state, "private", "passdb.tdb"),
		usernameMapPath,
		filepath.Join(state, "private"),
		filepath.Join(state, "state"),
		filepath.Join(state, "cache"),
		filepath.Join(state, "lock"),
		filepath.Join(state, "pid"),
		server.config.ShareName,
		server.config.WorkspacePath,
		server.config.UnixUser,
		server.config.UnixUser,
		server.config.UnixUser,
		readOnly,
	))
}

func (server *Server) setLocalSID(ctx context.Context, configurationPath string) error {
	identityContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(
		identityContext,
		server.config.NetBinary,
		"--configfile="+configurationPath,
		"setlocalsid",
		localSID(server.config.ServerIdentity),
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("configure SMB server identity: %w: %s", err, strings.TrimSpace(output.String()))
	}
	return nil
}

func (server *Server) netBIOSName() string {
	digest := sha256.Sum256([]byte(server.config.ServerIdentity))
	return fmt.Sprintf("T3%X", digest[:6])
}

func localSID(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	parts := [3]uint32{}
	for index := range parts {
		parts[index] = binary.BigEndian.Uint32(digest[index*4 : index*4+4])
		if parts[index] == 0 {
			parts[index] = 1
		}
	}
	return fmt.Sprintf("S-1-5-21-%d-%d-%d", parts[0], parts[1], parts[2])
}

func (server *Server) updatePassword(ctx context.Context, configurationPath string, password []byte) error {
	passwordContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(
		passwordContext,
		server.config.SMBPasswdBinary,
		"-s",
		"-a",
		"-c",
		configurationPath,
		server.config.UnixUser,
	)
	command.Stdin = io.MultiReader(bytes.NewReader(password), strings.NewReader("\n"), bytes.NewReader(password), strings.NewReader("\n"))
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("configure SMB password: %w: %s", err, strings.TrimSpace(output.String()))
	}
	return nil
}

func readPassword(path string) ([]byte, error) {
	password, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SMB password: %w", err)
	}
	if len(password) == 0 {
		return nil, errors.New("SMB password is empty")
	}
	if len(password) > passwordLimit {
		return nil, errors.New("SMB password exceeds its size limit")
	}
	if bytes.IndexAny(password, "\x00\r\n") >= 0 {
		return nil, errors.New("SMB password contains an unsupported control character")
	}
	return password, nil
}

func writeFile(path string, content []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".t3-smb-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func stop(command *exec.Cmd, wait <-chan error) error {
	if command.Process == nil {
		return nil
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop SMB server: %w", err)
	}
	timer := time.NewTimer(stopTimeout)
	defer timer.Stop()
	select {
	case <-wait:
		return nil
	case <-timer.C:
		if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("kill SMB server: %w", err)
		}
		<-wait
		return nil
	}
}
