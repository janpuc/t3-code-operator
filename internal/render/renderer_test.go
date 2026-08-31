package render

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderUsesUpstreamProviderInstancesAndSecretReferences(t *testing.T) {
	secret := SecretReference{
		Namespace: "agents",
		Name:      "t3-code-config",
		Key:       "LITELLM_API_KEY",
	}
	remoteMCP := MCPServer{
		Name:      "kubectl",
		Transport: "http",
		Config:    json.RawMessage(`{"url":"http://litellm.ai.svc.cluster.local:4000/mcp/kubectl/mcp"}`),
		Headers: []Header{{
			Name:      "Authorization",
			Prefix:    "Bearer ",
			ValueFrom: &secret,
		}},
	}

	input := ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{
			{InstanceID: "codex", Driver: "codex", Enabled: true, MCPServers: []MCPServer{remoteMCP}},
			{InstanceID: "claude", Driver: "claudeAgent", Enabled: true, MCPServers: []MCPServer{remoteMCP}},
			{InstanceID: "cursor", Driver: "cursor", Enabled: true, MCPServers: []MCPServer{remoteMCP}},
			{InstanceID: "grok", Driver: "grok", Enabled: true, MCPServers: []MCPServer{remoteMCP}},
			{InstanceID: "opencode", Driver: "opencode", Enabled: true, MCPServers: []MCPServer{remoteMCP}},
		},
	}

	manifest, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.APIVersion != ProtocolVersion {
		t.Fatalf("unexpected protocol version %q", manifest.APIVersion)
	}
	if !strings.HasPrefix(manifest.DesiredRevision, "sha256:") {
		t.Fatalf("unexpected desired revision %q", manifest.DesiredRevision)
	}
	if manifest.ServerSettings.EnableProviderUpdateChecks {
		t.Fatal("renderer enabled upstream provider update checks")
	}
	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawManifest), `"serverSettings":{"enableProviderUpdateChecks":false}`) {
		t.Fatalf("rendered manifest omitted managed server settings: %s", rawManifest)
	}
	if len(manifest.ProviderInstances) != 5 {
		t.Fatalf("expected five provider instances, got %d", len(manifest.ProviderInstances))
	}
	if got := string(manifest.ProviderInstances["codex"].Config); !strings.Contains(got, `"homePath":"/data/harnesses/codex/codex"`) {
		t.Fatalf("Codex home path is not managed: %s", got)
	}
	if got := string(manifest.ProviderInstances["claude"].Config); !strings.Contains(got, `"homePath":"/data/harnesses/claude/claude"`) {
		t.Fatalf("Claude home path is not managed: %s", got)
	}

	claudeFile := findFile(t, manifest, "claude")
	if claudeFile.Path != "/data/harnesses/claude/claude/.claude.json" || claudeFile.Mode != WriteModeMerge {
		t.Fatalf("unexpected Claude target: %#v", claudeFile)
	}
	if !strings.Contains(claudeFile.Content, `Bearer ${T3CODE_MCP_`) {
		t.Fatalf("Claude MCP does not use environment indirection: %s", claudeFile.Content)
	}

	codexFile := findFile(t, manifest, "codex")
	if codexFile.Path != "/data/harnesses/codex/codex/config.toml" || codexFile.Mode != WriteModeMerge {
		t.Fatalf("unexpected Codex target: %#v", codexFile)
	}
	if !strings.Contains(codexFile.Content, `bearer_token_env_var = "T3CODE_MCP_`) {
		t.Fatalf("Codex MCP does not use bearer_token_env_var: %s", codexFile.Content)
	}

	opencodeFile := findFile(t, manifest, "opencode")
	if opencodeFile.Path != "/data/harnesses/opencode/opencode/opencode.jsonc" || opencodeFile.Mode != WriteModeMerge {
		t.Fatalf("unexpected OpenCode target: %#v", opencodeFile)
	}
	if !strings.Contains(opencodeFile.Content, `Bearer {env:T3CODE_MCP_`) {
		t.Fatalf("OpenCode MCP does not use environment indirection: %s", opencodeFile.Content)
	}

	if findFileByInstance(manifest, "cursor") != nil || findFileByInstance(manifest, "grok") != nil {
		t.Fatal("alpha drivers claimed an unverified MCP file dialect")
	}
	assertWarning(t, manifest, "cursor", IssueAlphaDialect)
	assertWarning(t, manifest, "grok", IssueAlphaDialect)

	for _, instanceID := range []string{"codex", "claude", "opencode"} {
		instance := manifest.ProviderInstances[instanceID]
		binding := findSensitiveBinding(t, instance)
		if binding.Value != "" || binding.ValueFrom == nil || *binding.ValueFrom != secret || !binding.Sensitive {
			t.Fatalf("invalid Secret binding for %s: %#v", instanceID, binding)
		}
	}
}

func TestRenderRejectsKnownInlineSecretFields(t *testing.T) {
	_, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{{
			InstanceID: "opencode",
			Driver:     "opencode",
			Enabled:    true,
			Config:     json.RawMessage(`{"serverPassword":"raw-secret-canary"}`),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "serverPassword") {
		t.Fatalf("expected a serverPassword validation error, got %v", err)
	}
}

func TestRenderIsDeterministicAcrossInputOrder(t *testing.T) {
	first, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{
			{InstanceID: "zulu", Driver: "codex", Enabled: true},
			{InstanceID: "alpha", Driver: "claudeAgent", Enabled: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []Harness{
			{InstanceID: "alpha", Driver: "claudeAgent", Enabled: true},
			{InstanceID: "zulu", Driver: "codex", Enabled: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.DesiredRevision != second.DesiredRevision {
		t.Fatalf("input order changed the revision: %s != %s", first.DesiredRevision, second.DesiredRevision)
	}
}

func TestVerifyManifestRejectsEnabledProviderUpdateChecks(t *testing.T) {
	manifest, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest.ServerSettings.EnableProviderUpdateChecks = true
	manifest.DesiredRevision, err = manifestRevision(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifest(manifest); err == nil ||
		!strings.Contains(err.Error(), "serverSettings.enableProviderUpdateChecks") {
		t.Fatalf("enabled provider update checks passed validation: %v", err)
	}
}

func findFile(t *testing.T, manifest Manifest, instanceID string) FileTarget {
	t.Helper()
	file := findFileByInstance(manifest, instanceID)
	if file == nil {
		t.Fatalf("file target for %q is missing", instanceID)
	}
	return *file
}

func findFileByInstance(manifest Manifest, instanceID string) *FileTarget {
	for index := range manifest.Files {
		if manifest.Files[index].InstanceID == instanceID {
			return &manifest.Files[index]
		}
	}
	return nil
}

func assertWarning(t *testing.T, manifest Manifest, instanceID string, code IssueCode) {
	t.Helper()
	for _, warning := range manifest.Warnings {
		if warning.InstanceID == instanceID && warning.Code == code {
			return
		}
	}
	t.Fatalf("warning %q for %q is missing: %#v", code, instanceID, manifest.Warnings)
}

func findSensitiveBinding(t *testing.T, instance ProviderInstance) ProviderEnvironment {
	t.Helper()
	for _, environment := range instance.Environment {
		if environment.Sensitive {
			return environment
		}
	}
	t.Fatalf("sensitive binding is missing: %#v", instance.Environment)
	return ProviderEnvironment{}
}
