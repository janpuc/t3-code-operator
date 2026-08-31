package render

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderWorkstationConfigurationUsesSecretReferences(t *testing.T) {
	credential := &SecretReference{Namespace: "agents", Name: "git-token", Key: "token"}
	privateKey := &SecretReference{Namespace: "agents", Name: "git-signing", Key: "private"}
	publicKey := &SecretReference{Namespace: "agents", Name: "git-signing", Key: "public"}
	manifest, err := Render(ResolvedWorkstation{
		Namespace:   "agents",
		Name:        "primary",
		UID:         "workstation-uid",
		MachineInfo: &MachineInfo{PrettyHostname: `Cluster "One"`},
		Git: &GitConfiguration{
			UserName:            "Agent User",
			UserEmail:           "agent@example.test",
			GitHubUser:          "agent-user",
			CredentialSecretRef: credential,
			SigningKeySecretRef: &GitSigningKeyReference{PrivateKey: privateKey, PublicKey: publicKey},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 7 {
		t.Fatalf("unexpected Workstation file count: %d", len(manifest.Files))
	}
	gitConfig := findRenderedFile(t, manifest.Files, gitConfigPath)
	if gitConfig.Scope != FileScopeWorkstation || gitConfig.Mode != WriteModeManagedBlock ||
		!gitConfig.DiscoverGitSafeDirectories || !strings.Contains(gitConfig.Content, "allowedSignersFile") {
		t.Fatalf("unexpected Git config target: %#v", gitConfig)
	}
	githubConfig := findRenderedFile(t, manifest.Files, githubConfigPath)
	if githubConfig.Mode != WriteModeReplace || githubConfig.Content != "version: 1\n" {
		t.Fatalf("unexpected GitHub CLI config target: %#v", githubConfig)
	}
	hosts := findRenderedFile(t, manifest.Files, githubHostsPath)
	if hosts.Mode != WriteModeReplace || len(hosts.Values) != 2 ||
		hosts.Values[0].ValueFrom != *credential || hosts.Values[1].ValueFrom != *credential ||
		!strings.Contains(hosts.Content, `"user":"agent-user"`) || strings.Contains(hosts.Content, "secret-canary") {
		t.Fatalf("unexpected GitHub credential target: %#v", hosts)
	}
	private := findRenderedFile(t, manifest.Files, gitPrivateKeyPath)
	if private.SecretContent == nil || private.SecretContent.ValueFrom != *privateKey || private.Content != "" {
		t.Fatalf("unexpected private key target: %#v", private)
	}
	machine := findRenderedFile(t, manifest.Files, machineInfoPath)
	if machine.Content != "PRETTY_HOSTNAME=\"Cluster \\\"One\\\"\"\n" {
		t.Fatalf("unexpected machine-info content: %q", machine.Content)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-canary", "OPENSSH PRIVATE KEY"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("rendered manifest contains secret content %q", forbidden)
		}
	}
	if err := VerifyManifest(manifest); err != nil {
		t.Fatal(err)
	}
}

func TestRenderWorkstationConfigurationRejectsUnsafeIdentity(t *testing.T) {
	_, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Git:       &GitConfiguration{UserName: "unsafe\nname"},
	})
	if err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("expected unsafe Git identity rejection, got %v", err)
	}

	_, err = Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Git: &GitConfiguration{
			SigningKeySecretRef: &GitSigningKeyReference{
				PrivateKey: &SecretReference{Namespace: "agents", Name: "key", Key: "private"},
				PublicKey:  &SecretReference{Namespace: "agents", Name: "key", Key: "public"},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "userEmail") {
		t.Fatalf("expected signing email rejection, got %v", err)
	}

	credential := &SecretReference{Namespace: "agents", Name: "git-token", Key: "token"}
	_, err = Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Git:       &GitConfiguration{CredentialSecretRef: credential},
	})
	if err == nil || !strings.Contains(err.Error(), "githubUser") {
		t.Fatalf("expected missing GitHub user rejection, got %v", err)
	}

	_, err = Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Git:       &GitConfiguration{GitHubUser: "agent-user"},
	})
	if err == nil || !strings.Contains(err.Error(), "credentialSecretRef") {
		t.Fatalf("expected unused GitHub user rejection, got %v", err)
	}
}

func findRenderedFile(t *testing.T, files []FileTarget, path string) FileTarget {
	t.Helper()
	for _, file := range files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("render lacks file %s", path)
	return FileTarget{}
}
