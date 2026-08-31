package render

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublicValidationKeepsDialectKnowledgeInRender(t *testing.T) {
	support, err := ValidateHarness("agents", Harness{InstanceID: "cursor", Driver: "cursor", Enabled: true})
	if err != nil || support != SupportLevelAlpha {
		t.Fatalf("Cursor support mismatch: support=%q err=%v", support, err)
	}
	if _, err := ValidateHarness("agents", Harness{InstanceID: "future", Driver: "future", Enabled: true}); err == nil {
		t.Fatal("unknown driver passed adapter validation")
	}
	if err := ValidateExtension("agents", Extension{Name: "skill", Source: ExtensionSource{
		Type: ExtensionSourceGit,
		Git:  &GitExtensionSource{URL: "https://example.test/skill.git", Commit: strings.Repeat("a", 40)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMCPServer("agents", MCPServer{
		Name: "remote", Transport: "http", Config: json.RawMessage(`{"url":"https://mcp.example.test"}`),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicDialectCapabilitiesMatchRenderedOutput(t *testing.T) {
	for _, driver := range []string{"codex", "claudeAgent", "opencode"} {
		if !ProgramsMCPServers(driver) {
			t.Errorf("%s did not report MCP server capability", driver)
		}
	}
	for _, driver := range []string{"cursor", "grok", "future"} {
		if ProgramsMCPServers(driver) {
			t.Errorf("%s reported unavailable MCP server capability", driver)
		}
	}

	tests := []struct {
		driver     string
		sourceType ExtensionSourceType
		want       bool
	}{
		{driver: "codex", sourceType: ExtensionSourceGit, want: true},
		{driver: "codex", sourceType: ExtensionSourceOCI, want: true},
		{driver: "codex", sourceType: ExtensionSourceMarketplace, want: true},
		{driver: "codex", sourceType: ExtensionSourceGitHubRelease, want: true},
		{driver: "claudeAgent", sourceType: ExtensionSourceGit, want: true},
		{driver: "claudeAgent", sourceType: ExtensionSourceOCI, want: true},
		{driver: "claudeAgent", sourceType: ExtensionSourceMarketplace, want: true},
		{driver: "claudeAgent", sourceType: ExtensionSourceGitHubRelease, want: true},
		{driver: "opencode", sourceType: ExtensionSourceGit, want: true},
		{driver: "opencode", sourceType: ExtensionSourceOCI, want: true},
		{driver: "opencode", sourceType: ExtensionSourceMarketplace, want: false},
		{driver: "opencode", sourceType: ExtensionSourceGitHubRelease, want: false},
		{driver: "cursor", sourceType: ExtensionSourceGit, want: false},
		{driver: "grok", sourceType: ExtensionSourceOCI, want: false},
		{driver: "future", sourceType: ExtensionSourceGit, want: false},
		{driver: "codex", sourceType: "Future", want: false},
	}
	for _, test := range tests {
		if got := ProgramsExtensionSource(test.driver, test.sourceType); got != test.want {
			t.Errorf("ProgramsExtensionSource(%q, %q) = %t, want %t", test.driver, test.sourceType, got, test.want)
		}
	}
}
