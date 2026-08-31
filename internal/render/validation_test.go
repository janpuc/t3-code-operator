package render

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderCanonicalizesEquivalentJSONNumbers(t *testing.T) {
	render := func(config string) Manifest {
		manifest, err := Render(ResolvedWorkstation{
			Namespace: "agents",
			Name:      "primary",
			UID:       "workstation-uid",
			Harnesses: []Harness{{
				InstanceID: "codex",
				Driver:     "codex",
				Enabled:    true,
				Config:     json.RawMessage(config),
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	first := render(`{"threshold":1}`)
	second := render(`{"threshold":1.0}`)
	if first.DesiredRevision != second.DesiredRevision {
		t.Fatalf("equivalent JSON numbers changed the revision: %s != %s", first.DesiredRevision, second.DesiredRevision)
	}
}

func TestRenderRejectsOversizedManifest(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"file": map[string]any{"notes": strings.Repeat("x", 900*1024)},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{{InstanceID: "codex", Driver: "codex", Enabled: true, Config: raw}},
	})
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("expected a rendered size error, got %v", err)
	}
}

func TestRenderRejectsManagedPathEscape(t *testing.T) {
	_, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{{
			InstanceID: "codex",
			Driver:     "codex",
			Enabled:    true,
			Config:     json.RawMessage(`{"shadowHomePath":"/data/harnesses/codex/../other"}`),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "shadowHomePath") {
		t.Fatalf("expected a managed path error, got %v", err)
	}
}

func TestRenderRejectsUnsupportedAndUnisolatedDrivers(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		_, err := Render(ResolvedWorkstation{
			Namespace: "agents",
			Name:      "primary",
			UID:       "workstation-uid",
			Harnesses: []Harness{{InstanceID: "future", Driver: "futureDriver", Enabled: true}},
		})
		if err == nil || !strings.Contains(err.Error(), "no renderer adapter") {
			t.Fatalf("expected an unsupported driver error, got %v", err)
		}
	})

	t.Run("unisolated repeated driver", func(t *testing.T) {
		_, err := Render(ResolvedWorkstation{
			Namespace: "agents",
			Name:      "primary",
			UID:       "workstation-uid",
			Harnesses: []Harness{
				{InstanceID: "first", Driver: "opencode", Enabled: true},
				{InstanceID: "second", Driver: "opencode", Enabled: true},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "state isolation") {
			t.Fatalf("expected a state isolation error, got %v", err)
		}
	})
}

func TestVerifyManifestRevalidatesRenderedSemantics(t *testing.T) {
	secret := SecretReference{Namespace: "agents", Name: "provider-token", Key: "token"}
	manifest, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{{
			InstanceID: "codex",
			Driver:     "codex",
			Enabled:    true,
			Environment: []EnvironmentVariable{{
				Name:      "OPENAI_API_KEY",
				ValueFrom: &secret,
			}},
			MCPServers: []MCPServer{{
				Name:      "remote",
				Transport: "http",
				Config:    json.RawMessage(`{"url":"https://example.test/mcp"}`),
			}},
			Extensions: []Extension{{
				Name: "research",
				Source: ExtensionSource{
					Type: ExtensionSourceGit,
					Git: &GitExtensionSource{
						URL:    "https://example.test/research.git",
						Commit: strings.Repeat("a", 40),
					},
				},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		mutate  func(*Manifest)
		expects string
	}{
		"provider policy": {
			mutate: func(candidate *Manifest) {
				provider := candidate.ProviderInstances["codex"]
				provider.Apply.Mechanism = ReloadNextSession
				candidate.ProviderInstances["codex"] = provider
			},
			expects: "providerInstances.codex.apply",
		},
		"inline provider Secret": {
			mutate: func(candidate *Manifest) {
				provider := candidate.ProviderInstances["codex"]
				provider.Environment[0].ValueFrom = nil
				provider.Environment[0].Value = "inline-secret"
				provider.Environment[0].Sensitive = false
				candidate.ProviderInstances["codex"] = provider
			},
			expects: "sensitive environment variable",
		},
		"NUL provider environment": {
			mutate: func(candidate *Manifest) {
				provider := candidate.ProviderInstances["codex"]
				provider.Environment[0].Name = "SAFE_LABEL"
				provider.Environment[0].ValueFrom = nil
				provider.Environment[0].Value = "unsafe\x00value"
				provider.Environment[0].Sensitive = false
				candidate.ProviderInstances["codex"] = provider
			},
			expects: "NUL",
		},
		"credentialed source URL": {
			mutate: func(candidate *Manifest) {
				candidate.Extensions[0].Source.Git.URL = "https://user:password@example.test/research.git"
			},
			expects: "must not contain credentials",
		},
		"file policy": {
			mutate: func(candidate *Manifest) {
				candidate.Files[0].Apply.When = ApplyWhenImmediate
			},
			expects: "files[0].apply",
		},
		"file path escape": {
			mutate: func(candidate *Manifest) {
				candidate.Files[0].Path = "/data/harnesses/codex/../other/config.toml"
			},
			expects: "clean absolute path",
		},
		"invalid owned path": {
			mutate: func(candidate *Manifest) {
				candidate.Files[0].OwnedPaths = []string{"/invalid~2path"}
			},
			expects: "invalid escape",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			var candidate Manifest
			if err := json.Unmarshal(raw, &candidate); err != nil {
				t.Fatal(err)
			}
			test.mutate(&candidate)
			candidate.DesiredRevision, err = manifestRevision(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyManifest(candidate); err == nil || !strings.Contains(err.Error(), test.expects) {
				t.Fatalf("invalid rendered semantics passed verification: %v", err)
			}
		})
	}
}
