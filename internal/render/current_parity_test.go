package render

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCurrentDeploymentParity(t *testing.T) {
	litellm := SecretReference{Namespace: "agents", Name: "t3-code-config", Key: "LITELLM_API_KEY"}
	memini := SecretReference{Namespace: "agents", Name: "t3-code-config", Key: "MEMINI_API_KEY"}
	github := SecretReference{Namespace: "agents", Name: "t3-code-config", Key: "GH_TOKEN"}
	meminiHome := "personal/jan"
	baseURL := "http://litellm.ai.svc.cluster.local:4000/v1"

	servers := []MCPServer{
		{
			Name:      "koment",
			Transport: "stdio",
			Config:    json.RawMessage(`{"command":"koment","args":["mcp","--write"]}`),
		},
		{
			Name:      "memini",
			Transport: "http",
			Config:    json.RawMessage(`{"url":"http://memini.ai.svc.cluster.local:8080/mcp","enabled":true}`),
			Headers: []Header{
				{Name: "Authorization", Prefix: "Bearer ", ValueFrom: &memini},
				{Name: "X-Memini-Home", Value: &meminiHome},
			},
		},
	}
	for name, endpoint := range map[string]string{
		"kubectl":        "kubectl",
		"prometheus":     "prometheus",
		"victoria-logs":  "victoria_logs",
		"github":         "github",
		"context7":       "context7",
		"home-assistant": "ha",
		"sidero":         "sidero",
	} {
		servers = append(servers, MCPServer{
			Name:      name,
			Transport: "http",
			Config: json.RawMessage(
				`{"url":"http://litellm.ai.svc.cluster.local:4000/mcp/` + endpoint + `/mcp","enabled":true}`,
			),
			Headers: []Header{{Name: "Authorization", Prefix: "Bearer ", ValueFrom: &litellm}},
		})
	}

	agentKit := Extension{
		Name: "agent-kit",
		Source: ExtensionSource{
			Type: ExtensionSourceGit,
			Git: &GitExtensionSource{
				URL:                 "https://github.com/janpuc/agent-kit.git",
				Commit:              strings.Repeat("a", 40),
				Path:                "skills",
				CredentialSecretRef: &github,
			},
			Include: []string{"research", "tdd"},
		},
	}
	meminiPlugin := Extension{
		Name: "memini",
		Source: ExtensionSource{
			Type: ExtensionSourceMarketplace,
			Marketplace: &MarketplaceExtensionSource{
				Marketplace:         "memini",
				Extension:           "memini",
				RepositoryURL:       "https://github.com/eleboucher/memini.git",
				Commit:              strings.Repeat("b", 40),
				CredentialSecretRef: &github,
			},
		},
	}
	komentMarketplace := Extension{
		Name: "koment",
		Source: ExtensionSource{
			Type: ExtensionSourceMarketplace,
			Marketplace: &MarketplaceExtensionSource{
				Marketplace:         "koment-dev",
				Extension:           "koment",
				RepositoryURL:       "https://github.com/koment-dev/koment.git",
				Commit:              strings.Repeat("c", 40),
				CredentialSecretRef: &github,
			},
		},
	}
	komentRelease := Extension{
		Name: "koment",
		Source: ExtensionSource{
			Type: ExtensionSourceGitHubRelease,
			GitHubRelease: &GitHubReleaseExtensionSource{
				Repository:          "koment-dev/koment",
				Tag:                 "v3.2.0",
				Asset:               "koment-plugin-codex_v3.2.0.tar.gz",
				SHA256:              strings.Repeat("d", 64),
				CredentialSecretRef: &github,
			},
		},
	}

	commonEnvironment := []EnvironmentVariable{
		{Name: "LITELLM_API_KEY", ValueFrom: &litellm},
		{Name: "MEMINI_API_KEY", ValueFrom: &memini},
		{Name: "MEMINI_HOME", Value: &meminiHome},
	}
	claudeFile := `{
		"binaryPath":"claude",
		"customModels":["claude-opus-4-1"],
		"file":{
			"effortLevel":"xhigh"
		}
	}`
	opencodeFile := `{
		"binaryPath":"opencode",
		"customModels":["litellm/minimax/MiniMax-M3"],
		"file":{
			"provider":{"litellm":{"npm":"@ai-sdk/openai-compatible","options":{"baseURL":"` + baseURL + `","apiKey":"{env:LITELLM_API_KEY}"}}},
			"model":"litellm/minimax/MiniMax-M3",
			"small_model":"litellm/opencode-go/hy3",
			"plugin":["@eleboucher/opencode-memini","@koment/opencode-koment"]
		}
	}`

	manifest, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{
			{
				InstanceID:  "codex",
				Driver:      "codex",
				DisplayName: "Codex",
				Enabled:     true,
				Environment: commonEnvironment,
				Config:      json.RawMessage(`{"binaryPath":"codex","customModels":["gpt-5.6-sol"]}`),
				MCPServers:  servers,
				Extensions:  []Extension{agentKit, meminiPlugin, komentRelease},
			},
			{
				InstanceID:  "claude",
				Driver:      "claudeAgent",
				DisplayName: "Claude",
				Enabled:     true,
				Environment: commonEnvironment,
				Config:      json.RawMessage(claudeFile),
				MCPServers:  servers,
				Extensions:  []Extension{agentKit, meminiPlugin, komentMarketplace},
			},
			{
				InstanceID:  "claudel",
				Driver:      "claudeAgent",
				DisplayName: "Claude via LiteLLM",
				Enabled:     true,
				Environment: append(append([]EnvironmentVariable(nil), commonEnvironment...),
					EnvironmentVariable{Name: "ANTHROPIC_AUTH_TOKEN", ValueFrom: &litellm},
					EnvironmentVariable{Name: "ANTHROPIC_BASE_URL", Value: &baseURL},
				),
				Config:     json.RawMessage(`{"binaryPath":"claude","customModels":["claude-opus-4-1"]}`),
				MCPServers: servers,
				Extensions: []Extension{agentKit},
			},
			{
				InstanceID:  "opencode",
				Driver:      "opencode",
				DisplayName: "OpenCode",
				Enabled:     true,
				Environment: commonEnvironment,
				Config:      json.RawMessage(opencodeFile),
				MCPServers:  servers,
				Extensions:  []Extension{agentKit},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.ProviderInstances) != 4 || len(manifest.Files) != 5 || len(manifest.Extensions) != 8 {
		t.Fatalf("parity shape changed: providers=%d files=%d extensions=%d", len(manifest.ProviderInstances), len(manifest.Files), len(manifest.Extensions))
	}
	if len(manifest.Warnings) != 0 {
		t.Fatalf("supported parity input produced warnings: %#v", manifest.Warnings)
	}

	for _, instanceID := range []string{"codex", "claude", "claudel", "opencode"} {
		file := findFile(t, manifest, instanceID)
		for _, serverName := range []string{"kubectl", "prometheus", "victoria-logs", "github", "context7", "home-assistant", "sidero", "memini", "koment"} {
			if !strings.Contains(file.Content, serverName) {
				t.Fatalf("%s did not render MCP server %s: %s", instanceID, serverName, file.Content)
			}
		}
	}
	if !strings.Contains(findFile(t, manifest, "codex").Content, "bearer_token_env_var") ||
		!strings.Contains(findFile(t, manifest, "claude").Content, "${T3CODE_MCP_") ||
		!strings.Contains(findFile(t, manifest, "opencode").Content, "{env:T3CODE_MCP_") {
		t.Fatal("one supported MCP Secret dialect lost environment indirection")
	}
	claudeSettings := findFileAtPath(t, manifest, "/data/harnesses/claude/claude/settings.json")
	if claudeSettings.Content != `{"effortLevel":"xhigh"}` {
		t.Fatalf("Claude settings were rendered into the wrong file: %#v", claudeSettings)
	}
	assertManifestSecretSafe(t, manifest)
}

func assertManifestSecretSafe(t *testing.T, manifest Manifest) {
	t.Helper()
	for instanceID, instance := range manifest.ProviderInstances {
		for _, variable := range instance.Environment {
			if variable.Sensitive && (variable.Value != "" || variable.ValueFrom == nil) {
				t.Fatalf("sensitive environment contains a value for %s: %#v", instanceID, variable)
			}
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"raw-secret-canary", `"serverPassword"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("rendered manifest contains forbidden Secret material %q", forbidden)
		}
	}
}
