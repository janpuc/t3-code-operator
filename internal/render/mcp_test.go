package render

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderStdioMCPUsesEachSupportedDialect(t *testing.T) {
	secret := SecretReference{Namespace: "agents", Name: "memini", Key: "api-key"}
	literal := "project"
	server := MCPServer{
		Name:      "memini",
		Transport: "stdio",
		Config:    json.RawMessage(`{"command":"memini","args":["serve"],"cwd":"/workspace"}`),
		Environment: []EnvironmentVariable{
			{Name: "MEMINI_API_KEY", ValueFrom: &secret},
			{Name: "MEMINI_SCOPE", Value: &literal},
		},
	}

	manifest, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{
			{InstanceID: "codex", Driver: "codex", Enabled: true, MCPServers: []MCPServer{server}},
			{InstanceID: "claude", Driver: "claudeAgent", Enabled: true, MCPServers: []MCPServer{server}},
			{InstanceID: "opencode", Driver: "opencode", Enabled: true, MCPServers: []MCPServer{server}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	codex := findFile(t, manifest, "codex").Content
	if !strings.Contains(codex, `command = "memini"`) || !strings.Contains(codex, `env_vars = ["MEMINI_API_KEY"]`) {
		t.Fatalf("unexpected Codex stdio MCP config: %s", codex)
	}
	if !strings.Contains(codex, "[mcp_servers.memini.env]") || !strings.Contains(codex, `MEMINI_SCOPE = "project"`) {
		t.Fatalf("Codex literal MCP environment is missing: %s", codex)
	}
	if binding := findSensitiveBinding(t, manifest.ProviderInstances["codex"]); binding.Name != "MEMINI_API_KEY" {
		t.Fatalf("Codex must forward the exact stdio variable name: %#v", binding)
	}

	claude := findFile(t, manifest, "claude").Content
	if !strings.Contains(claude, `"command":"memini"`) || !strings.Contains(claude, `"MEMINI_API_KEY":"${T3CODE_MCP_`) {
		t.Fatalf("unexpected Claude stdio MCP config: %s", claude)
	}
	if binding := findSensitiveBinding(t, manifest.ProviderInstances["claude"]); !strings.HasPrefix(binding.Name, "T3CODE_MCP_") {
		t.Fatalf("Claude must use an internal stdio variable name: %#v", binding)
	}

	opencode := findFile(t, manifest, "opencode").Content
	if !strings.Contains(opencode, `"command":["memini","serve"]`) || !strings.Contains(opencode, `"MEMINI_API_KEY":"{env:T3CODE_MCP_`) {
		t.Fatalf("unexpected OpenCode stdio MCP config: %s", opencode)
	}
}

func TestRenderDeduplicatesEquivalentCodexStdioSecretBindings(t *testing.T) {
	secret := SecretReference{Namespace: "agents", Name: "shared", Key: "token"}
	server := func(name string) MCPServer {
		return MCPServer{
			Name:        name,
			Transport:   "stdio",
			Config:      json.RawMessage(`{"command":"server"}`),
			Environment: []EnvironmentVariable{{Name: "SHARED_TOKEN", ValueFrom: &secret}},
		}
	}
	manifest, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{{
			InstanceID: "codex",
			Driver:     "codex",
			Enabled:    true,
			MCPServers: []MCPServer{server("first"), server("second")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bindings := manifest.ProviderInstances["codex"].Environment
	if len(bindings) != 1 || bindings[0].Name != "SHARED_TOKEN" {
		t.Fatalf("equivalent bindings were not deduplicated: %#v", bindings)
	}
}

func TestRenderRejectsInlineCredentials(t *testing.T) {
	inline := "raw-secret-canary"
	tests := map[string]Harness{
		"sensitive environment": {
			InstanceID:  "codex",
			Driver:      "codex",
			Enabled:     true,
			Environment: []EnvironmentVariable{{Name: "OPENAI_API_KEY", Value: &inline}},
		},
		"sensitive header": {
			InstanceID: "codex",
			Driver:     "codex",
			Enabled:    true,
			MCPServers: []MCPServer{{
				Name:      "remote",
				Transport: "http",
				Config:    json.RawMessage(`{"url":"https://example.test/mcp"}`),
				Headers:   []Header{{Name: "Authorization", Value: &inline}},
			}},
		},
		"nested config credential": {
			InstanceID: "codex",
			Driver:     "codex",
			Enabled:    true,
			Config:     json.RawMessage(`{"custom":{"apiKey":"raw-secret-canary"}}`),
		},
	}
	for name, harness := range tests {
		t.Run(name, func(t *testing.T) {
			manifest, err := Render(ResolvedWorkstation{
				Namespace: "agents",
				Name:      "primary",
				UID:       "workstation-uid",
				Harnesses: []Harness{harness},
			})
			if err == nil {
				raw, _ := json.Marshal(manifest)
				t.Fatalf("expected inline credential rejection, got %s", raw)
			}
			if strings.Contains(err.Error(), inline) {
				t.Fatalf("validation error disclosed the credential: %v", err)
			}
		})
	}
}

func TestRenderRejectsMultilineHTTPHeaderContent(t *testing.T) {
	secret := SecretReference{Namespace: "agents", Name: "mcp-token", Key: "token"}
	for name, header := range map[string]Header{
		"prefix": {Name: "Authorization", Prefix: "Bearer\nInjected: ", ValueFrom: &secret},
		"value":  {Name: "X-Label", Value: mcpStringPointer("first\r\nInjected: second")},
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
					MCPServers: []MCPServer{{
						Name:      "remote",
						Transport: "http",
						Config:    json.RawMessage(`{"url":"https://example.test/mcp"}`),
						Headers:   []Header{header},
					}},
				}},
			})
			if err == nil || !strings.Contains(err.Error(), "single-line") {
				t.Fatalf("multiline header passed validation: %v", err)
			}
		})
	}
}

func TestRenderRejectsSensitiveMCPURLQueryParameters(t *testing.T) {
	for _, rawURL := range []string{
		"https://example.test/mcp?api_key=raw-secret-canary",
		"https://example.test/mcp#not-sent",
	} {
		config, err := json.Marshal(map[string]string{"url": rawURL})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Render(ResolvedWorkstation{
			Namespace: "agents",
			Name:      "primary",
			UID:       "workstation-uid",
			Harnesses: []Harness{{
				InstanceID: "codex",
				Driver:     "codex",
				Enabled:    true,
				MCPServers: []MCPServer{{
					Name:      "remote",
					Transport: "http",
					Config:    config,
				}},
			}},
		})
		if err == nil || strings.Contains(err.Error(), "raw-secret-canary") {
			t.Fatalf("unsafe MCP URL passed validation or leaked its value: %v", err)
		}
	}
}

func mcpStringPointer(value string) *string {
	return &value
}

func TestRenderCanonicalizesOpaqueJSONAndMCPOrder(t *testing.T) {
	secret := SecretReference{Namespace: "agents", Name: "shared", Key: "token"}
	makeInput := func(config string, servers []MCPServer) ResolvedWorkstation {
		return ResolvedWorkstation{
			Namespace: "agents",
			Name:      "primary",
			UID:       "workstation-uid",
			Harnesses: []Harness{{
				InstanceID: "codex",
				Driver:     "codex",
				Enabled:    true,
				Config:     json.RawMessage(config),
				MCPServers: servers,
			}},
		}
	}
	firstServer := MCPServer{
		Name:      "first",
		Transport: "http",
		Config:    json.RawMessage(`{"url":"https://first.example/mcp","enabled":true}`),
		Headers:   []Header{{Name: "Authorization", Prefix: "Bearer ", ValueFrom: &secret}},
	}
	secondServer := MCPServer{
		Name:      "second",
		Transport: "http",
		Config:    json.RawMessage(`{"url":"https://second.example/mcp"}`),
	}

	first, err := Render(makeInput(`{"customModels":["one"],"launchArgs":["serve"]}`, []MCPServer{secondServer, firstServer}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Render(makeInput(`{ "launchArgs" : ["serve"], "customModels" : ["one"] }`, []MCPServer{firstServer, secondServer}))
	if err != nil {
		t.Fatal(err)
	}
	if first.DesiredRevision != second.DesiredRevision {
		t.Fatalf("semantic input order changed the revision: %s != %s", first.DesiredRevision, second.DesiredRevision)
	}
}

func TestRenderKeepsOpenCodeFileConfigOutOfUpstreamSettings(t *testing.T) {
	manifest, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{{
			InstanceID: "opencode",
			Driver:     "opencode",
			Enabled:    true,
			Config: json.RawMessage(`{
				"binaryPath":"opencode",
				"file":{
					"provider":{"litellm":{"options":{"apiKey":"{env:LITELLM_API_KEY}"}}},
					"model":"litellm/minimax/MiniMax-M3",
					"plugin":["@eleboucher/opencode-memini"]
				}
			}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	providerConfig := string(manifest.ProviderInstances["opencode"].Config)
	if strings.Contains(providerConfig, `"file"`) || strings.Contains(providerConfig, `"apiKey"`) {
		t.Fatalf("file dialect leaked into upstream t3 settings: %s", providerConfig)
	}
	if !strings.Contains(providerConfig, `"binaryPath":"opencode"`) {
		t.Fatalf("upstream provider config was lost: %s", providerConfig)
	}
	file := findFile(t, manifest, "opencode")
	if !strings.Contains(file.Content, `"apiKey":"{env:LITELLM_API_KEY}"`) ||
		!strings.Contains(file.Content, `"plugin":["@eleboucher/opencode-memini"]`) {
		t.Fatalf("OpenCode file config was not rendered: %s", file.Content)
	}
	for _, owned := range []string{"/model", "/plugin", "/provider"} {
		if !containsString(file.OwnedPaths, owned) {
			t.Fatalf("owned path %q is missing: %#v", owned, file.OwnedPaths)
		}
	}
}

func TestRenderKeepsClaudeFileConfigOutOfUpstreamSettings(t *testing.T) {
	manifest, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{{
			InstanceID: "claude",
			Driver:     "claudeAgent",
			Enabled:    true,
			Config: json.RawMessage(`{
				"binaryPath":"claude",
				"file":{
					"effortLevel":"xhigh"
				}
			}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	providerConfig := string(manifest.ProviderInstances["claude"].Config)
	if strings.Contains(providerConfig, `"file"`) || !strings.Contains(providerConfig, `"binaryPath":"claude"`) {
		t.Fatalf("invalid upstream Claude provider config: %s", providerConfig)
	}
	file := findFileAtPath(t, manifest, "/data/harnesses/claude/claude/settings.json")
	if file.Content != `{"effortLevel":"xhigh"}` || !containsString(file.OwnedPaths, "/effortLevel") {
		t.Fatalf("Claude file config was not rendered: %#v", file)
	}
	if findFileAtPathOrNil(manifest, "/data/harnesses/claude/claude/.claude.json") != nil {
		t.Fatal("Claude settings without MCP servers must not create .claude.json")
	}
}

func TestRenderRejectsInstallerOwnedFileConfig(t *testing.T) {
	tests := []struct {
		name   string
		driver string
		key    string
	}{
		{name: "Claude enabled plugins", driver: "claudeAgent", key: "enabledPlugins"},
		{name: "Claude known marketplaces", driver: "claudeAgent", key: "extraKnownMarketplaces"},
		{name: "Codex marketplaces", driver: "codex", key: "marketplaces"},
		{name: "Codex plugins", driver: "codex", key: "plugins"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Render(ResolvedWorkstation{
				Namespace: "agents",
				Name:      "primary",
				UID:       "workstation-uid",
				Harnesses: []Harness{{
					InstanceID: "instance",
					Driver:     test.driver,
					Enabled:    true,
					Config:     json.RawMessage(`{"file":{"` + test.key + `":{}}}`),
				}},
			})
			if err == nil || !strings.Contains(err.Error(), test.key) || !strings.Contains(err.Error(), "Extension attachments") {
				t.Fatalf("expected installer ownership error for %s, got %v", test.key, err)
			}
		})
	}
}

func findFileAtPath(t *testing.T, manifest Manifest, path string) FileTarget {
	t.Helper()
	file := findFileAtPathOrNil(manifest, path)
	if file == nil {
		t.Fatalf("file target %q is missing: %#v", path, manifest.Files)
	}
	return *file
}

func findFileAtPathOrNil(manifest Manifest, path string) *FileTarget {
	for index := range manifest.Files {
		if manifest.Files[index].Path == path {
			return &manifest.Files[index]
		}
	}
	return nil
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
