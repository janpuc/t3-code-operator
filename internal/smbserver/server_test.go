package smbserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareBuildsEncryptedShareWithoutClientLeases(t *testing.T) {
	state := t.TempDir()
	server := testServer(t, Config{
		Username:        "developer",
		ShareName:       "projects",
		ServerIdentity:  "agents/nvme-workstation",
		PasswordFile:    filepath.Join(state, "password"),
		WorkspacePath:   "/workspace",
		StateDirectory:  filepath.Join(state, "state"),
		UnixUser:        "node",
		Port:            1445,
		ReadOnly:        true,
		PollInterval:    time.Second,
		SMBDBinary:      "/usr/sbin/smbd",
		SMBPasswdBinary: "/usr/bin/smbpasswd",
		NetBinary:       "/usr/bin/net",
	})
	configurationPath, err := server.prepare()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	configuration := string(raw)
	for _, required := range []string{
		"server min protocol = SMB3",
		"netbios name = " + server.netBIOSName(),
		"server smb encrypt = required",
		"smb2 leases = no",
		"oplocks = no",
		"wide links = no",
		"[projects]",
		"path = /workspace",
		"valid users = node",
		"force user = node",
		"read only = yes",
	} {
		if !strings.Contains(configuration, required) {
			t.Fatalf("configuration lacks %q:\n%s", required, configuration)
		}
	}
	usernameMap, err := os.ReadFile(filepath.Join(state, "state", "username.map"))
	if err != nil {
		t.Fatal(err)
	}
	if string(usernameMap) != "node = developer\n" {
		t.Fatalf("unexpected username map %q", usernameMap)
	}
}

func TestUpdatePasswordUsesStandardInputOnly(t *testing.T) {
	directory := t.TempDir()
	inputPath := filepath.Join(directory, "stdin")
	argumentsPath := filepath.Join(directory, "arguments")
	binary := filepath.Join(directory, "smbpasswd")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\" >\"$T3_TEST_ARGUMENTS\"\ncat >\"$T3_TEST_STDIN\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("T3_TEST_ARGUMENTS", argumentsPath)
	t.Setenv("T3_TEST_STDIN", inputPath)
	server := testServer(t, Config{
		Username:        "t3",
		ShareName:       "workspace",
		ServerIdentity:  "agents/nvme-workstation",
		PasswordFile:    filepath.Join(directory, "password"),
		WorkspacePath:   "/workspace",
		StateDirectory:  filepath.Join(directory, "state"),
		UnixUser:        "node",
		Port:            1445,
		PollInterval:    time.Second,
		SMBDBinary:      "/usr/sbin/smbd",
		SMBPasswdBinary: binary,
		NetBinary:       "/usr/bin/net",
	})
	if err := server.updatePassword(context.Background(), "/state/smb.conf", []byte("correct horse")); err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(input) != "correct horse\ncorrect horse\n" {
		t.Fatalf("unexpected password input %q", input)
	}
	arguments, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(arguments), "correct horse") {
		t.Fatalf("password appeared in arguments: %q", arguments)
	}
	if string(arguments) != "-s\n-a\n-c\n/state/smb.conf\nnode\n" {
		t.Fatalf("unexpected smbpasswd arguments %q", arguments)
	}
}

func TestSetLocalSIDUsesStableHashedIdentity(t *testing.T) {
	directory := t.TempDir()
	argumentsPath := filepath.Join(directory, "arguments")
	binary := filepath.Join(directory, "net")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\" >\"$T3_TEST_ARGUMENTS\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("T3_TEST_ARGUMENTS", argumentsPath)
	server := testServer(t, Config{
		Username:        "t3",
		ShareName:       "workspace",
		ServerIdentity:  "agents/nvme-workstation",
		PasswordFile:    filepath.Join(directory, "password"),
		WorkspacePath:   "/workspace",
		StateDirectory:  filepath.Join(directory, "state"),
		UnixUser:        "node",
		Port:            1445,
		PollInterval:    time.Second,
		SMBDBinary:      "/usr/sbin/smbd",
		SMBPasswdBinary: "/usr/bin/smbpasswd",
		NetBinary:       binary,
	})
	configurationPath, err := server.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.setLocalSID(context.Background(), configurationPath); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	expected := "--configfile=" + configurationPath + "\nsetlocalsid\n" + localSID("agents/nvme-workstation") + "\n"
	if string(arguments) != expected {
		t.Fatalf("unexpected net arguments %q", arguments)
	}
	if localSID("agents/nvme-workstation") == localSID("agents/other-workstation") {
		t.Fatal("distinct server identities produced the same test SID")
	}
}

func TestReadPasswordRejectsAmbiguousInput(t *testing.T) {
	for name, value := range map[string]string{
		"empty":   "",
		"newline": "first\nsecond",
		"nul":     "first\x00second",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "password")
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readPassword(path); err == nil {
				t.Fatal("invalid password was accepted")
			}
		})
	}
}

func TestConfigRejectsUnsafeNamesAndPrivilegedPorts(t *testing.T) {
	baseline := Config{
		Username:        "t3",
		ShareName:       "workspace",
		ServerIdentity:  "agents/nvme-workstation",
		PasswordFile:    "/password",
		WorkspacePath:   "/workspace",
		StateDirectory:  "/state",
		UnixUser:        "node",
		Port:            1445,
		PollInterval:    time.Second,
		SMBDBinary:      "/usr/sbin/smbd",
		SMBPasswdBinary: "/usr/bin/smbpasswd",
		NetBinary:       "/usr/bin/net",
	}
	for name, mutate := range map[string]func(*Config){
		"username": func(value *Config) { value.Username = "t3 admin" },
		"share":    func(value *Config) { value.ShareName = "workspace]" },
		"port":     func(value *Config) { value.Port = 445 },
		"path":     func(value *Config) { value.WorkspacePath = "/workspace\n[other]" },
		"identity": func(value *Config) { value.ServerIdentity = "agents/nvme\n[other]" },
	} {
		t.Run(name, func(t *testing.T) {
			value := baseline
			mutate(&value)
			if _, err := New(value); err == nil {
				t.Fatal("invalid SMB configuration was accepted")
			}
		})
	}
}

func TestRunAppliesSecretRotationWithoutRestartingSMBD(t *testing.T) {
	directory := t.TempDir()
	passwordPath := filepath.Join(directory, "password")
	passwordLog := filepath.Join(directory, "password-log")
	readyPath := filepath.Join(directory, "smbd-ready")
	if err := os.WriteFile(passwordPath, []byte("first-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	smbpasswd := filepath.Join(directory, "smbpasswd")
	if err := os.WriteFile(smbpasswd, []byte("#!/bin/sh\nset -eu\ncat >>\"$T3_TEST_PASSWORD_LOG\"\nprintf '%s\\n' --end-- >>\"$T3_TEST_PASSWORD_LOG\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	smbd := filepath.Join(directory, "smbd")
	if err := os.WriteFile(smbd, []byte("#!/bin/sh\nset -eu\ntouch \"$T3_TEST_SMBD_READY\"\ntrap 'exit 0' TERM INT\nwhile :; do sleep 0.1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	netBinary := filepath.Join(directory, "net")
	if err := os.WriteFile(netBinary, []byte("#!/bin/sh\nset -eu\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("T3_TEST_PASSWORD_LOG", passwordLog)
	t.Setenv("T3_TEST_SMBD_READY", readyPath)
	server := testServer(t, Config{
		Username:        "t3",
		ShareName:       "workspace",
		ServerIdentity:  "agents/nvme-workstation",
		PasswordFile:    passwordPath,
		WorkspacePath:   "/workspace",
		StateDirectory:  filepath.Join(directory, "state"),
		UnixUser:        "node",
		Port:            1445,
		PollInterval:    10 * time.Millisecond,
		SMBDBinary:      smbd,
		SMBPasswdBinary: smbpasswd,
		NetBinary:       netBinary,
		Logf:            func(string, ...any) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx)
	}()
	waitForFileContent(t, readyPath, "", 2*time.Second)
	waitForFileContent(t, passwordLog, "first-password\nfirst-password\n--end--", 2*time.Second)
	if err := os.WriteFile(passwordPath, []byte("second-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForFileContent(t, passwordLog, "second-password\nsecond-password\n--end--", 2*time.Second)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SMB server did not stop")
	}
}

func waitForFileContent(t *testing.T, path, expected string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(raw), expected) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, _ := os.ReadFile(path)
	t.Fatalf("file %s did not contain %q: %q", path, expected, raw)
}

func testServer(t *testing.T, config Config) *Server {
	t.Helper()
	server, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return server
}
