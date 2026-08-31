package apply

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janpuc/t3-code-operator/internal/render"
	"sigs.k8s.io/yaml"
)

func TestWorkstationFilesPreserveUserStateAndKeepSecretsPrivate(t *testing.T) {
	dataRoot := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestFile(t, filepath.Join(dataRoot, "home", ".gitconfig"), "[alias]\n\tco = checkout\n")
	userHostsPath := filepath.Join(dataRoot, "home", ".config", "gh", "hosts.yml")
	writeTestFile(t, userHostsPath, "enterprise.example.test:\n  user: preserved\n")
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "project", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "groups", "nested", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	credential := render.SecretReference{Namespace: "agents", Name: "git-token", Key: "token"}
	privateKey := render.SecretReference{Namespace: "agents", Name: "git-signing", Key: "private"}
	publicKey := render.SecretReference{Namespace: "agents", Name: "git-signing", Key: "public"}
	privateBody := base64.StdEncoding.EncodeToString(append([]byte("openssh-key-v1\x00"), make([]byte, 160)...))
	privateValue := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + privateBody + "\n-----END OPENSSH PRIVATE KEY-----"
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		credential: {Value: "github-secret-canary", Version: "1"},
		privateKey: {Value: privateValue, Version: "1"},
		publicKey:  {Value: "ssh-ed25519 AAAA-public-canary agent@example.test\n", Version: "1"},
	}}
	applier, err := New(Config{
		DataRoot:      dataRoot,
		WorkspaceRoot: workspaceRoot,
		Secrets:       secrets,
		Upstream:      &fakeUpstream{},
		Activity:      &fakeActivity{},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := workstationFileManifest(t, credential, privateKey, publicKey)
	report, err := applier.Apply(context.Background(), manifest)
	if err != nil || report.State != ApplyStateProgrammed {
		t.Fatalf("apply Workstation files: report=%#v err=%v", report, err)
	}

	gitConfig := readTestFile(t, filepath.Join(dataRoot, "home", ".gitconfig"))
	for _, required := range []string{
		"co = checkout",
		managedBlockStart,
		"allowedSignersFile = /data/home/.ssh/allowed_signers",
		`directory = "` + filepath.Join(workspaceRoot, "project") + `"`,
		`directory = "` + filepath.Join(workspaceRoot, "groups", "nested") + `"`,
	} {
		if !strings.Contains(gitConfig, required) {
			t.Fatalf("Git config lacks %q:\n%s", required, gitConfig)
		}
	}

	managedGitHubDirectory := filepath.Join(dataRoot, "t3-coded", "gh")
	if config := readTestFile(t, filepath.Join(managedGitHubDirectory, "config.yml")); config != "version: 1\n" {
		t.Fatalf("unexpected GitHub CLI config: %q", config)
	}
	hostsRaw := readTestFile(t, filepath.Join(managedGitHubDirectory, "hosts.yml"))
	hosts := map[string]any{}
	if err := yaml.Unmarshal([]byte(hostsRaw), &hosts); err != nil {
		t.Fatal(err)
	}
	github := hosts["github.com"].(map[string]any)
	users := github["users"].(map[string]any)
	managedUser := users["agent-user"].(map[string]any)
	if github["user"] != "agent-user" || github["oauth_token"] != "github-secret-canary" ||
		managedUser["oauth_token"] != "github-secret-canary" {
		t.Fatalf("GitHub hosts state is wrong: %#v", hosts)
	}
	if userHosts := readTestFile(t, userHostsPath); userHosts != "enterprise.example.test:\n  user: preserved\n" {
		t.Fatalf("user-owned GitHub CLI state changed: %q", userHosts)
	}
	assertPinnedGitHubCLIContract(t, managedGitHubDirectory, "agent-user", "github-secret-canary")
	managedHostsPath := filepath.Join(managedGitHubDirectory, "hosts.yml")
	privateKeyPath := filepath.Join(dataRoot, "home", ".ssh", "id_signing")
	for _, path := range []string{managedHostsPath, privateKeyPath} {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	driftReport, needsApply, err := applier.Refresh(context.Background())
	if err != nil || !needsApply || driftReport.Reason != "DriftDetected" {
		t.Fatalf("permissive file modes were not detected: report=%#v needsApply=%v err=%v", driftReport, needsApply, err)
	}
	if _, err := applier.Apply(context.Background(), manifest); err != nil {
		t.Fatalf("repair managed file modes: %v", err)
	}
	for _, path := range []string{managedHostsPath, privateKeyPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("managed file %s has mode %04o", path, info.Mode().Perm())
		}
	}

	normalizedPrivate := readTestFile(t, privateKeyPath)
	for _, line := range strings.Split(normalizedPrivate, "\n") {
		if !strings.HasPrefix(line, "-----") && len(line) > 70 {
			t.Fatalf("private key line is not rewrapped: %d", len(line))
		}
	}
	allowed := readTestFile(t, filepath.Join(dataRoot, "home", ".ssh", "allowed_signers"))
	if allowed != "agent@example.test ssh-ed25519 AAAA-public-canary agent@example.test\n" {
		t.Fatalf("unexpected allowed_signers: %q", allowed)
	}
	if machine := readTestFile(t, filepath.Join(dataRoot, "t3-coded", "machine-info")); machine != "PRETTY_HOSTNAME=\"Cluster\"\n" {
		t.Fatalf("unexpected machine-info: %q", machine)
	}
	state := readTestFile(t, filepath.Join(dataRoot, "t3-coded", "state.json"))
	for _, secret := range []string{"github-secret-canary", privateBody, "AAAA-public-canary"} {
		if strings.Contains(state, secret) {
			t.Fatalf("persisted state contains secret content")
		}
	}
}

func TestRepositoryScanRefreshDetectsNewSafeDirectory(t *testing.T) {
	dataRoot := t.TempDir()
	workspaceRoot := t.TempDir()
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{}}
	applier, err := New(Config{
		DataRoot:               dataRoot,
		WorkspaceRoot:          workspaceRoot,
		RepositoryScanInterval: time.Minute,
		Secrets:                secrets,
		Upstream:               &fakeUpstream{},
		Activity:               &fakeActivity{},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	applier.now = func() time.Time { return now }
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Git:       &render.GitConfiguration{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Apply(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	newRepository := filepath.Join(workspaceRoot, "new-repository")
	if err := os.MkdirAll(filepath.Join(newRepository, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	report, needsApply, err := applier.Refresh(context.Background())
	if err != nil || !needsApply || report.Reason != "DriftDetected" {
		t.Fatalf("new repository was not detected: report=%#v needsApply=%v err=%v", report, needsApply, err)
	}
}

func workstationFileManifest(
	t *testing.T,
	credential render.SecretReference,
	privateKey render.SecretReference,
	publicKey render.SecretReference,
) render.Manifest {
	t.Helper()
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace:   "agents",
		Name:        "primary",
		UID:         "workstation-uid",
		MachineInfo: &render.MachineInfo{PrettyHostname: "Cluster"},
		Git: &render.GitConfiguration{
			UserName:            "Agent User",
			UserEmail:           "agent@example.test",
			GitHubUser:          "agent-user",
			CredentialSecretRef: &credential,
			SigningKeySecretRef: &render.GitSigningKeyReference{PrivateKey: &privateKey, PublicKey: &publicKey},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func assertPinnedGitHubCLIContract(t *testing.T, configDirectory, expectedUser, expectedToken string) {
	t.Helper()
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		t.Log("skip pinned GitHub CLI contract: gh is unavailable")
		return
	}
	version, err := exec.Command(ghPath, "--version").Output()
	if err != nil || !strings.Contains(string(version), "gh version 2.98.0") {
		t.Log("skip pinned GitHub CLI contract: gh 2.98.0 is unavailable")
		return
	}
	environment := environmentWithout(os.Environ(), "GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN")
	environment = append(environment, "GH_CONFIG_DIR="+configDirectory)
	tokenCommand := exec.Command(ghPath, "auth", "token", "--hostname", "github.com")
	tokenCommand.Env = environment
	token, err := tokenCommand.Output()
	if err != nil || strings.TrimSpace(string(token)) != expectedToken {
		t.Fatalf("pinned GitHub CLI did not read the managed active token: %v", err)
	}
	credentialCommand := exec.Command(ghPath, "auth", "git-credential", "get")
	credentialCommand.Env = environment
	credentialCommand.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	credential, err := credentialCommand.Output()
	if err != nil {
		t.Fatalf("pinned GitHub CLI credential helper failed: %v", err)
	}
	for _, required := range []string{"username=" + expectedUser, "password=" + expectedToken} {
		if !strings.Contains(string(credential), required) {
			sanitized := strings.ReplaceAll(string(credential), expectedToken, "<redacted>")
			t.Fatalf("pinned GitHub CLI credential helper returned an unexpected shape: %q", sanitized)
		}
	}
}

func environmentWithout(environment []string, names ...string) []string {
	prefixes := make([]string, 0, len(names))
	for _, name := range names {
		prefixes = append(prefixes, name+"=")
	}
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		remove := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(item, prefix) {
				remove = true
				break
			}
		}
		if !remove {
			result = append(result, item)
		}
	}
	return result
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
