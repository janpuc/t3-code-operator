package render

import (
	"strings"
	"testing"
)

func TestRenderActivatesPinnedSkillsInEachSupportedRoot(t *testing.T) {
	credential := SecretReference{Namespace: "agents", Name: "github-read", Key: "token"}
	extension := Extension{
		Name: "agent-kit",
		Source: ExtensionSource{
			Type: ExtensionSourceGit,
			Git: &GitExtensionSource{
				URL:                 "https://github.com/example/agent-kit.git",
				Commit:              strings.Repeat("a", 40),
				Path:                "skills",
				CredentialSecretRef: &credential,
			},
			Include: []string{"tdd", "research"},
		},
	}
	manifest, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{
			{InstanceID: "codex", Driver: "codex", Enabled: true, Extensions: []Extension{extension}},
			{InstanceID: "claude", Driver: "claudeAgent", Enabled: true, Extensions: []Extension{extension}},
			{InstanceID: "opencode", Driver: "opencode", Enabled: true, Extensions: []Extension{extension}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Extensions) != 3 {
		t.Fatalf("expected three activations, got %#v", manifest.Extensions)
	}

	tests := map[string]string{
		"codex":    "/data/harnesses/codex/codex/skills/research",
		"claude":   "/data/harnesses/claude/claude/skills/research",
		"opencode": "/data/harnesses/opencode/opencode/skills/research",
	}
	var cacheKey string
	for instanceID, expectedPath := range tests {
		activation := findActivation(t, manifest, instanceID, "agent-kit")
		if !strings.HasPrefix(activation.CacheKey, "sha256:") {
			t.Fatalf("invalid cache key for %s: %s", instanceID, activation.CacheKey)
		}
		if cacheKey == "" {
			cacheKey = activation.CacheKey
		} else if activation.CacheKey != cacheKey {
			t.Fatalf("source cache key changed by target: %s != %s", activation.CacheKey, cacheKey)
		}
		if len(activation.Destinations) != 2 || activation.Destinations[0].Path != expectedPath {
			t.Fatalf("unexpected destinations for %s: %#v", instanceID, activation.Destinations)
		}
		if activation.Destinations[0].SourcePath != "skills/research" || activation.Destinations[0].Mode != WriteModeReplace {
			t.Fatalf("unexpected activation target for %s: %#v", instanceID, activation.Destinations[0])
		}
		if activation.Source.Git == nil || activation.Source.Git.CredentialSecretRef == nil ||
			*activation.Source.Git.CredentialSecretRef != credential {
			t.Fatalf("credential reference was not preserved for %s: %#v", instanceID, activation.Source)
		}
	}
	if value := findEnvironmentValue(manifest.ProviderInstances["opencode"], "XDG_CONFIG_HOME"); value != "/data/harnesses/opencode" {
		t.Fatalf("OpenCode skill discovery root is not managed: %q", value)
	}
}

func findEnvironmentValue(instance ProviderInstance, name string) string {
	for _, variable := range instance.Environment {
		if variable.Name == name {
			return variable.Value
		}
	}
	return ""
}

func TestRenderSelectsMarketplaceInstallerByDriver(t *testing.T) {
	extension := Extension{
		Name: "memini",
		Source: ExtensionSource{
			Type: ExtensionSourceMarketplace,
			Marketplace: &MarketplaceExtensionSource{
				Marketplace:   "memini",
				Extension:     "memini",
				RepositoryURL: "https://github.com/eleboucher/memini.git",
				Commit:        strings.Repeat("b", 40),
			},
		},
	}
	manifest, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{
			{InstanceID: "codex", Driver: "codex", Enabled: true, Extensions: []Extension{extension}},
			{InstanceID: "claude", Driver: "claudeAgent", Enabled: true, Extensions: []Extension{extension}},
			{InstanceID: "opencode", Driver: "opencode", Enabled: true, Extensions: []Extension{extension}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]InstallerKind{
		"codex":  InstallerCodexMarketplace,
		"claude": InstallerClaudeMarketplace,
	}
	for instanceID, kind := range want {
		installer := findActivation(t, manifest, instanceID, "memini").Installer
		if installer == nil || installer.Kind != kind || installer.Extension != "memini" {
			t.Fatalf("unexpected installer for %s: %#v", instanceID, installer)
		}
	}
	if findActivationOrNil(manifest, "opencode", "memini") != nil {
		t.Fatal("OpenCode must not claim an unpinned package installation")
	}
	assertWarning(t, manifest, "opencode", IssueUnsupportedExtensionSource)
}

func findActivationOrNil(manifest Manifest, instanceID, name string) *ExtensionActivation {
	for index := range manifest.Extensions {
		activation := &manifest.Extensions[index]
		if activation.InstanceID == instanceID && activation.Name == name {
			return activation
		}
	}
	return nil
}

func TestRenderRejectsExtensionDestinationCollision(t *testing.T) {
	makeExtension := func(name, commit string) Extension {
		return Extension{
			Name: name,
			Source: ExtensionSource{
				Type:    ExtensionSourceGit,
				Git:     &GitExtensionSource{URL: "https://example.test/" + name + ".git", Commit: commit},
				Include: []string{"tdd"},
			},
		}
	}
	_, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{{
			InstanceID: "codex",
			Driver:     "codex",
			Enabled:    true,
			Extensions: []Extension{
				makeExtension("first", strings.Repeat("c", 40)),
				makeExtension("second", strings.Repeat("d", 40)),
			},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("expected a destination collision, got %v", err)
	}
}

func TestRenderRejectsUnsafeMarketplacePluginIdentifiers(t *testing.T) {
	for name, marketplace := range map[string]MarketplaceExtensionSource{
		"marketplace option": {
			Marketplace:   "--help",
			Extension:     "plugin",
			RepositoryURL: "https://example.test/plugins.git",
			Commit:        strings.Repeat("a", 40),
		},
		"plugin option": {
			Marketplace:   "plugins",
			Extension:     "--help",
			RepositoryURL: "https://example.test/plugins.git",
			Commit:        strings.Repeat("a", 40),
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Render(ResolvedWorkstation{
				Namespace: "agents",
				Name:      "primary",
				UID:       "workstation-uid",
				Harnesses: []Harness{{
					InstanceID: "codex",
					Driver:     "codex",
					Enabled:    true,
					Extensions: []Extension{{
						Name:   "plugin",
						Source: ExtensionSource{Type: ExtensionSourceMarketplace, Marketplace: &marketplace},
					}},
				}},
			})
			if err == nil || !strings.Contains(err.Error(), "safe plugin identifiers") {
				t.Fatalf("expected a safe identifier error, got %v", err)
			}
		})
	}
}

func TestRepositoryURLAllowsAnSSHUsernameButNoPassword(t *testing.T) {
	if err := validateRepositoryURL("ssh://git@example.test/skills.git", "source"); err != nil {
		t.Fatalf("SSH username was rejected: %v", err)
	}
	for _, rawURL := range []string{
		"ssh://git:secret@example.test/skills.git",
		"https://git@example.test/skills.git",
		"https://example.test/skills.git?token=secret",
	} {
		if err := validateRepositoryURL(rawURL, "source"); err == nil {
			t.Fatalf("unsafe repository URL passed validation: %s", rawURL)
		}
	}
}

func findActivation(t *testing.T, manifest Manifest, instanceID, name string) ExtensionActivation {
	t.Helper()
	for _, activation := range manifest.Extensions {
		if activation.InstanceID == instanceID && activation.Name == name {
			return activation
		}
	}
	t.Fatalf("activation %s/%s is missing: %#v", instanceID, name, manifest.Extensions)
	return ExtensionActivation{}
}
