package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/janpuc/t3-code-operator/internal/render"
)

const (
	miseOutputLimit           = 1 << 20
	miseErrorLimit            = 32 << 10
	defaultMiseCommandTimeout = 15 * time.Minute
)

type MiseRuntimeConfig struct {
	Binary         string
	DataRoot       string
	CommandTimeout time.Duration
}

type MiseRuntime struct {
	binary         string
	dataRoot       string
	cacheRoot      string
	stateRoot      string
	configRoot     string
	systemRoot     string
	globalConfig   string
	commandTimeout time.Duration
}

func NewMiseRuntime(config MiseRuntimeConfig) (*MiseRuntime, error) {
	if config.Binary == "" {
		return nil, errors.New("mise binary is required")
	}
	if config.DataRoot == "" || !filepath.IsAbs(config.DataRoot) {
		return nil, errors.New("data root must be an absolute path")
	}
	root := filepath.Join(filepath.Clean(config.DataRoot), "t3-coded", "mise")
	commandTimeout := config.CommandTimeout
	if commandTimeout <= 0 {
		commandTimeout = defaultMiseCommandTimeout
	}
	runtime := &MiseRuntime{
		binary:         config.Binary,
		dataRoot:       root,
		cacheRoot:      filepath.Join(root, "cache"),
		stateRoot:      filepath.Join(root, "state"),
		configRoot:     filepath.Join(root, "config"),
		systemRoot:     filepath.Join(root, "system"),
		globalConfig:   filepath.Join(root, "config", "global.toml"),
		commandTimeout: commandTimeout,
	}
	for _, directory := range []string{runtime.dataRoot, runtime.cacheRoot, runtime.stateRoot, runtime.configRoot, runtime.systemRoot} {
		if err := rejectSymlinkComponents(filepath.Clean(config.DataRoot), directory); err != nil {
			return nil, fmt.Errorf("validate mise runtime directory: %w", err)
		}
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create mise runtime directory: %w", err)
		}
		if err := rejectSymlinkComponents(filepath.Clean(config.DataRoot), directory); err != nil {
			return nil, fmt.Errorf("validate created mise runtime directory: %w", err)
		}
	}
	global := []byte("[settings]\nnot_found_auto_install = false\nnot_found_system_fallback = false\n")
	if err := atomicWriteFile(runtime.globalConfig, global, 0o600); err != nil {
		return nil, fmt.Errorf("write isolated mise global configuration: %w", err)
	}
	return runtime, nil
}

func (runtime *MiseRuntime) Prepare(ctx context.Context, directory, toolDataRoot string) ([]MiseExecutable, error) {
	if !filepath.IsAbs(directory) || !filepath.IsAbs(toolDataRoot) {
		return nil, errors.New("mise working and data directories must be absolute")
	}
	if _, err := runtime.run(ctx, directory, toolDataRoot, miseErrorLimit, "--locked", "--no-hooks", "--yes", "-C", directory, "install", "--jobs", "1"); err != nil {
		return nil, err
	}
	raw, err := runtime.run(ctx, directory, toolDataRoot, miseOutputLimit, "--no-hooks", "-C", directory, "bin-paths", "--json")
	if err != nil {
		return nil, err
	}
	var executables []MiseExecutable
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&executables); err != nil {
		return nil, fmt.Errorf("parse mise executable inventory: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("mise executable inventory contains trailing data")
	}
	return executables, nil
}

func (runtime *MiseRuntime) run(
	ctx context.Context,
	directory string,
	toolDataRoot string,
	outputLimit int,
	arguments ...string,
) ([]byte, error) {
	commandContext, cancel := context.WithTimeout(ctx, runtime.commandTimeout)
	defer cancel()
	command := exec.CommandContext(commandContext, runtime.binary, arguments...)
	command.Dir = directory
	command.Env = runtime.environment(directory, toolDataRoot)
	command.Stdin = strings.NewReader("")
	standardOutput := &limitedMiseBuffer{limit: outputLimit}
	standardError := &limitedMiseBuffer{limit: miseErrorLimit}
	command.Stdout = standardOutput
	command.Stderr = standardError
	if err := command.Run(); err != nil {
		if ctxErr := commandContext.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		message := strings.TrimSpace(standardError.String())
		if message == "" {
			message = strings.TrimSpace(standardOutput.String())
		}
		if message == "" {
			return nil, fmt.Errorf("mise command failed: %w", err)
		}
		return nil, fmt.Errorf("mise command failed: %w: %s", err, message)
	}
	if standardOutput.truncated {
		return nil, errors.New("mise command output exceeded its size limit")
	}
	return append([]byte(nil), standardOutput.Bytes()...), nil
}

func (runtime *MiseRuntime) environment(directory, toolDataRoot string) []string {
	result := make([]string, 0, 24)
	for _, name := range []string{
		"PATH", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR", "TMPDIR",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	} {
		if value, exists := os.LookupEnv(name); exists {
			result = append(result, name+"="+value)
		}
	}
	return append(result,
		"MISE_DATA_DIR="+toolDataRoot,
		"MISE_CACHE_DIR="+runtime.cacheRoot,
		"MISE_STATE_DIR="+runtime.stateRoot,
		"MISE_CONFIG_DIR="+runtime.configRoot,
		"MISE_SYSTEM_CONFIG_DIR="+runtime.systemRoot,
		"MISE_GLOBAL_CONFIG_FILE="+runtime.globalConfig,
		"MISE_TRUSTED_CONFIG_PATHS="+directory,
		"MISE_SAFE=1",
		"MISE_LOCKED=1",
		"MISE_NO_HOOKS=1",
		"MISE_NOT_FOUND_AUTO_INSTALL=false",
		"MISE_NOT_FOUND_SYSTEM_FALLBACK=false",
		"MISE_JOBS=1",
		"MISE_YES=1",
		"CI=1",
		"NO_COLOR=1",
		"TERM=dumb",
	)
}

type limitedMiseBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *limitedMiseBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := buffer.limit - buffer.Len()
	if len(value) > remaining {
		buffer.truncated = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	_, _ = buffer.Buffer.Write(value)
	return originalLength, nil
}

func renderMiseFiles(tools []render.ToolActivation) ([]byte, []byte, error) {
	tools = normalizedToolActivations(tools)
	if err := render.ValidateToolActivations(tools); err != nil {
		return nil, nil, err
	}
	var configuration strings.Builder
	configuration.WriteString("[tool_config]\nlocked = true\n\n")
	configuration.WriteString("[settings]\nnot_found_auto_install = false\nnot_found_system_fallback = false\n\n")
	for _, tool := range tools {
		configuration.WriteString("[tools.")
		configuration.WriteString(tomlQuote(tool.Backend))
		configuration.WriteString("]\nversion = ")
		configuration.WriteString(tomlQuote(tool.Version))
		configuration.WriteByte('\n')
		optionNames := sortedStringKeys(tool.Options)
		for _, name := range optionNames {
			if name == "version" || name == "platforms" {
				return nil, nil, fmt.Errorf("tool %s uses reserved mise option %s", tool.Name, name)
			}
			configuration.WriteString(name)
			configuration.WriteString(" = ")
			configuration.WriteString(tomlQuote(tool.Options[name]))
			configuration.WriteByte('\n')
		}
		if strings.HasPrefix(tool.Backend, "http:") {
			configuration.WriteString("\n[tools.")
			configuration.WriteString(tomlQuote(tool.Backend))
			configuration.WriteString(".platforms]\n")
			for _, artifact := range tool.Artifacts {
				configuration.WriteString(tomlQuote(artifact.Platform))
				configuration.WriteString(" = { url = ")
				configuration.WriteString(tomlQuote(artifact.URL))
				configuration.WriteString(", checksum = ")
				configuration.WriteString(tomlQuote(artifact.SHA256))
				if artifact.Size > 0 {
					configuration.WriteString(", size = ")
					configuration.WriteString(strconv.FormatInt(artifact.Size, 10))
				}
				configuration.WriteString(" }\n")
			}
		}
		configuration.WriteByte('\n')
	}

	var lockfile strings.Builder
	lockfile.WriteString("lockfile_version = 1\n\n")
	for _, tool := range tools {
		lockfile.WriteString("[[tools.")
		lockfile.WriteString(tomlQuote(tool.Backend))
		lockfile.WriteString("]]\nversion = ")
		lockfile.WriteString(tomlQuote(tool.Version))
		lockfile.WriteString("\nbackend = ")
		lockfile.WriteString(tomlQuote(tool.Backend))
		lockfile.WriteString("\nspecifiers = [")
		lockfile.WriteString(tomlQuote(tool.Version))
		lockfile.WriteString("]\n")
		if len(tool.Options) != 0 {
			lockfile.WriteString("options = { ")
			for index, name := range sortedStringKeys(tool.Options) {
				if index != 0 {
					lockfile.WriteString(", ")
				}
				lockfile.WriteString(name)
				lockfile.WriteString(" = ")
				lockfile.WriteString(tomlQuote(tool.Options[name]))
			}
			lockfile.WriteString(" }\n")
		}
		for _, artifact := range tool.Artifacts {
			lockfile.WriteString("\n[tools.")
			lockfile.WriteString(tomlQuote(tool.Backend))
			lockfile.WriteByte('.')
			lockfile.WriteString(tomlQuote("platforms." + artifact.Platform))
			lockfile.WriteString("]\nchecksum = ")
			lockfile.WriteString(tomlQuote(artifact.SHA256))
			if artifact.Size > 0 {
				lockfile.WriteString("\nsize = ")
				lockfile.WriteString(strconv.FormatInt(artifact.Size, 10))
			}
			lockfile.WriteString("\nurl = ")
			lockfile.WriteString(tomlQuote(artifact.URL))
			lockfile.WriteByte('\n')
		}
		lockfile.WriteByte('\n')
	}
	return []byte(configuration.String()), []byte(lockfile.String()), nil
}

func tomlQuote(value string) string {
	return strconv.Quote(value)
}

func sortedStringKeys(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
