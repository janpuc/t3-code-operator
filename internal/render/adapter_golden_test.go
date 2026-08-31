package render

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuiltInAdapterGoldens(t *testing.T) {
	secret := SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	literal := "personal"
	servers := []MCPServer{
		{
			Name:        "local",
			Transport:   "stdio",
			Config:      json.RawMessage(`{"command":"local-mcp","args":["serve"]}`),
			Environment: []EnvironmentVariable{{Name: "SCOPE", Value: &literal}, {Name: "SERVICE_TOKEN", ValueFrom: &secret}},
		},
		{
			Name:      "remote",
			Transport: "http",
			Config:    json.RawMessage(`{"url":"https://example.test/mcp","enabled":true}`),
			Headers:   []Header{{Name: "Authorization", Prefix: "Bearer ", ValueFrom: &secret}},
		},
	}
	remoteEnvironment := internalMCPEnvironmentName("agents", "instance", "remote", "header-authorization")
	claudeLocalEnvironment := internalMCPEnvironmentName("agents", "instance", "local", "env-SERVICE_TOKEN")

	tests := []struct {
		name          string
		driver        string
		config        json.RawMessage
		support       SupportLevel
		filePath      string
		fileContent   string
		warning       IssueCode
		sensitiveName string
	}{
		{
			name:     "codex",
			driver:   "codex",
			config:   json.RawMessage(`{"binaryPath":"codex"}`),
			support:  SupportLevelSupported,
			filePath: "/data/harnesses/instance/codex/config.toml",
			fileContent: "[mcp_servers.local]\n" +
				"args = [\"serve\"]\n" +
				"command = \"local-mcp\"\n" +
				"env_vars = [\"SERVICE_TOKEN\"]\n\n" +
				"[mcp_servers.local.env]\n" +
				"SCOPE = \"personal\"\n\n" +
				"[mcp_servers.remote]\n" +
				"enabled = true\n" +
				"url = \"https://example.test/mcp\"\n" +
				"bearer_token_env_var = \"" + remoteEnvironment + "\"\n",
			sensitiveName: "SERVICE_TOKEN",
		},
		{
			name:          "claude",
			driver:        "claudeAgent",
			config:        json.RawMessage(`{"binaryPath":"claude"}`),
			support:       SupportLevelSupported,
			filePath:      "/data/harnesses/instance/claude/.claude.json",
			fileContent:   `{"mcpServers":{"local":{"args":["serve"],"command":"local-mcp","env":{"SCOPE":"personal","SERVICE_TOKEN":"${` + claudeLocalEnvironment + `}"},"type":"stdio"},"remote":{"enabled":true,"headers":{"Authorization":"Bearer ${` + remoteEnvironment + `}"},"type":"http","url":"https://example.test/mcp"}}}`,
			sensitiveName: claudeLocalEnvironment,
		},
		{
			name:    "cursor-alpha",
			driver:  "cursor",
			config:  json.RawMessage(`{"binaryPath":"cursor-agent"}`),
			support: SupportLevelAlpha,
			warning: IssueAlphaDialect,
		},
		{
			name:    "grok-alpha",
			driver:  "grok",
			config:  json.RawMessage(`{"binaryPath":"grok"}`),
			support: SupportLevelAlpha,
			warning: IssueAlphaDialect,
		},
		{
			name:          "opencode",
			driver:        "opencode",
			config:        json.RawMessage(`{"binaryPath":"opencode"}`),
			support:       SupportLevelSupported,
			filePath:      "/data/harnesses/instance/opencode/opencode.jsonc",
			fileContent:   `{"mcp":{"local":{"command":["local-mcp","serve"],"environment":{"SCOPE":"personal","SERVICE_TOKEN":"{env:` + claudeLocalEnvironment + `}"},"type":"local"},"remote":{"enabled":true,"headers":{"Authorization":"Bearer {env:` + remoteEnvironment + `}"},"type":"remote","url":"https://example.test/mcp"}}}`,
			sensitiveName: claudeLocalEnvironment,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest, err := Render(ResolvedWorkstation{
				Namespace: "agents",
				Name:      "primary",
				UID:       "workstation-uid",
				Harnesses: []Harness{{
					InstanceID: "instance",
					Driver:     test.driver,
					Enabled:    true,
					Config:     test.config,
					MCPServers: servers,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			provider := manifest.ProviderInstances["instance"]
			if provider.SupportLevel != test.support {
				t.Fatalf("support level: got %q, want %q", provider.SupportLevel, test.support)
			}
			file := findFileByInstance(manifest, "instance")
			if test.filePath == "" {
				if file != nil {
					t.Fatalf("alpha adapter rendered an unverified file: %#v", file)
				}
				assertWarning(t, manifest, "instance", test.warning)
				return
			}
			if file == nil || file.Path != test.filePath || file.Content != test.fileContent {
				t.Fatalf("adapter golden mismatch\ngot:  %#v\nwant: path=%s content=%q", file, test.filePath, test.fileContent)
			}
			if !hasSensitiveEnvironment(provider, test.sensitiveName, secret) {
				t.Fatalf("expected sensitive environment %q: %#v", test.sensitiveName, provider.Environment)
			}
		})
	}
}

func hasSensitiveEnvironment(instance ProviderInstance, name string, reference SecretReference) bool {
	for _, variable := range instance.Environment {
		if variable.Name == name && variable.Sensitive && variable.ValueFrom != nil && *variable.ValueFrom == reference {
			return true
		}
	}
	return false
}

func TestInternalMCPEnvironmentNameGolden(t *testing.T) {
	got := internalMCPEnvironmentName("agents", "instance", "remote", "header-authorization")
	const want = "T3CODE_MCP_REMOTE_"
	if !strings.HasPrefix(got, want) || len(got) != len(want)+16 {
		t.Fatalf("internal environment name changed shape: %s", got)
	}
	if got != internalMCPEnvironmentName("agents", "instance", "remote", "header-authorization") {
		t.Fatal("internal environment name is not deterministic")
	}
	if got == internalMCPEnvironmentName("agents", "instance", "other", "header-authorization") {
		t.Fatal("different MCP servers produced the same environment name")
	}
}
