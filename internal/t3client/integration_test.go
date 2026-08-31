package t3client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/janpuc/t3-code-operator/internal/apply"
)

func TestRealT3ControlClient(t *testing.T) {
	if os.Getenv("T3_CLIENT_INTEGRATION") != "1" {
		t.Skip("set T3_CLIENT_INTEGRATION=1 to run the real t3 control test")
	}
	binary := os.Getenv("T3_CLIENT_T3_BINARY")
	if binary == "" {
		t.Fatal("T3_CLIENT_T3_BINARY is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	version, err := DetectVersion(ctx, binary)
	if err != nil || version != "0.0.34" {
		t.Fatalf("unexpected t3 version %q: %v", version, err)
	}

	port := reserveIntegrationPort(t)
	baseDirectory := t.TempDir()
	workspace := t.TempDir()
	server := startIntegrationT3(t, binary, baseDirectory, workspace, port)
	t.Cleanup(func() { server.stop(t) })
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	waitForIntegrationT3(t, ctx, baseURL, server)

	auth, err := NewAuthTokenManager(AuthConfig{
		BaseURL:        baseURL,
		BaseDirectory:  baseDirectory,
		ClientID:       "integration-workstation",
		T3Binary:       binary,
		RequestTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := auth.Close(closeContext); err != nil {
			t.Error(err)
		}
	})
	client, err := New(Config{
		BaseURL:        baseURL,
		Tokens:         auth,
		RequestTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	desired := map[string]apply.ProviderInstance{
		"Codex_Integration": {
			Driver:      "codex",
			DisplayName: "Integration probe",
			Enabled:     false,
			Environment: []apply.ProviderEnvironment{{Name: "PROBE_VALUE", Value: "present"}},
			Config:      json.RawMessage(`{"future":{"preserved":true}}`),
		},
	}
	settings := apply.ManagedSettings{
		EnableProviderUpdateChecks: false,
		ProviderInstances:          desired,
	}
	if err := client.ApplyManagedSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	matches, err := client.ManagedSettingsMatch(ctx, settings)
	if err != nil || !matches {
		t.Fatalf("real settings did not match: matches=%v err=%v", matches, err)
	}
	active, err := client.ActiveInstances(ctx, nil)
	if err != nil || len(active) != 0 {
		t.Fatalf("isolated disabled provider was active: active=%#v err=%v", active, err)
	}
}

type integrationT3Server struct {
	command *exec.Cmd
	done    chan struct{}
	waitErr error
}

func reserveIntegrationPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func startIntegrationT3(t *testing.T, binary, baseDirectory, workspace string, port int) *integrationT3Server {
	t.Helper()
	command := exec.Command(
		binary,
		"start",
		"--mode", "web",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--base-dir", baseDirectory,
		"--no-browser",
		workspace,
	)
	command.Stdin = strings.NewReader("")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.Env = append(os.Environ(), "NO_COLOR=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	server := &integrationT3Server{command: command, done: make(chan struct{})}
	go func() {
		server.waitErr = command.Wait()
		close(server.done)
	}()
	return server
}

func waitForIntegrationT3(t *testing.T, ctx context.Context, baseURL string, server *integrationT3Server) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/.well-known/t3/environment", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, requestErr := client.Do(request)
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-server.done:
			t.Fatalf("isolated t3 exited before readiness: %v", server.waitErr)
		case <-ctx.Done():
			t.Fatal("isolated t3 did not become ready")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (server *integrationT3Server) stop(t *testing.T) {
	t.Helper()
	if err := server.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Error(err)
		return
	}
	select {
	case <-server.done:
		return
	case <-time.After(20 * time.Second):
	}
	if err := server.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Error(err)
	}
	select {
	case <-server.done:
	case <-time.After(5 * time.Second):
		t.Error("isolated t3 did not exit")
	}
}
