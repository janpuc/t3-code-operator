package apply

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janpuc/t3-code-operator/internal/render"
	"github.com/pelletier/go-toml/v2"
)

func TestTOMLMergePreservesUnknownStateAndReplacesOwnedRoot(t *testing.T) {
	dataRoot := t.TempDir()
	target := filepath.Join(dataRoot, "harnesses", "codex", "codex", "config.toml")
	writeTestFile(t, target, `
model = "user-model"

[auth]
access_token = "harness-owned"

[mcp_servers.user]
command = "user-mcp"
`)
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	applier := newTestApplier(t, dataRoot, secrets, &fakeUpstream{}, &fakeActivity{})
	manifest := driverManifest(t, "codex", reference, true)

	if _, err := applier.Apply(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var current map[string]any
	if err := toml.Unmarshal(raw, &current); err != nil {
		t.Fatal(err)
	}
	if current["model"] != "user-model" || current["auth"].(map[string]any)["access_token"] != "harness-owned" {
		t.Fatalf("unknown TOML state was lost: %#v", current)
	}
	servers := current["mcp_servers"].(map[string]any)
	if _, exists := servers["user"]; exists {
		t.Fatalf("user edit survived inside an operator-owned root: %#v", servers)
	}
	if _, exists := servers["remote"]; !exists {
		t.Fatalf("desired MCP server is missing: %#v", servers)
	}
	if strings.Contains(string(raw), "resolved-secret") {
		t.Fatal("resolved Secret entered TOML")
	}
}

func TestJSONCMergeAcceptsCommentsAndTrailingCommas(t *testing.T) {
	dataRoot := t.TempDir()
	target := filepath.Join(dataRoot, "harnesses", "opencode", "opencode", "opencode.jsonc")
	writeTestFile(t, target, `{
  // harness-owned setting
  "theme": "dark",
  "user": {"keep": true},
}`)
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	applier := newTestApplier(t, dataRoot, secrets, &fakeUpstream{}, &fakeActivity{})
	manifest := driverManifest(t, "opencode", reference, true)

	if _, err := applier.Apply(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	merged, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	current, err := decodeManagedObject(render.FileFormatJSON, merged)
	if err != nil {
		t.Fatal(err)
	}
	if current["theme"] != "dark" || current["user"].(map[string]any)["keep"] != true {
		t.Fatalf("unknown JSONC state was lost: %#v", current)
	}
	if _, exists := current["mcp"]; !exists {
		t.Fatalf("desired OpenCode MCP state is missing: %#v", current)
	}
	if !strings.Contains(string(merged), "// harness-owned setting") {
		t.Fatalf("JSONC merge removed an unowned comment: %s", merged)
	}
}

func TestMergeRemovalDeletesOnlyPreviouslyOwnedFields(t *testing.T) {
	dataRoot := t.TempDir()
	target := filepath.Join(dataRoot, "harnesses", "claude", "claude", ".claude.json")
	writeTestFile(t, target, `{"oauth":{"accessToken":"harness-owned"},"theme":"dark"}`)
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	applier := newTestApplier(t, dataRoot, secrets, &fakeUpstream{}, &fakeActivity{})
	if _, err := applier.Apply(context.Background(), driverManifest(t, "claudeAgent", reference, true)); err != nil {
		t.Fatal(err)
	}
	withoutMCP := driverManifest(t, "claudeAgent", reference, false)
	if _, err := applier.Apply(context.Background(), withoutMCP); err != nil {
		t.Fatal(err)
	}
	var current map[string]any
	readJSONFile(t, target, &current)
	if _, exists := current["mcpServers"]; exists {
		t.Fatalf("removed owned root remains: %#v", current)
	}
	if current["theme"] != "dark" || current["oauth"].(map[string]any)["accessToken"] != "harness-owned" {
		t.Fatalf("removal deleted harness-owned state: %#v", current)
	}
}

func TestMergeRevertsUserEditInsideOwnedRoot(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	applier := newTestApplier(t, dataRoot, secrets, &fakeUpstream{}, &fakeActivity{})
	manifest := driverManifest(t, "claudeAgent", reference, true)
	if _, err := applier.Apply(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dataRoot, "harnesses", "claude", "claude", ".claude.json")
	writeTestFile(t, target, `{"mcpServers":{"changed-in-ui":{"type":"http","url":"https://drift.example/mcp"}},"theme":"dark"}`)

	if _, err := applier.Apply(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	var current map[string]any
	readJSONFile(t, target, &current)
	servers := current["mcpServers"].(map[string]any)
	if _, exists := servers["changed-in-ui"]; exists {
		t.Fatalf("owned drift was not reverted: %#v", servers)
	}
	if _, exists := servers["remote"]; !exists || current["theme"] != "dark" {
		t.Fatalf("desired or unknown state is missing: %#v", current)
	}
}

func TestSeedIfAbsentReplacesOnlyMissingOrUnparseableContent(t *testing.T) {
	dataRoot := t.TempDir()
	logicalPath := "/data/harnesses/seed/config.json"
	physicalPath := filepath.Join(dataRoot, "harnesses", "seed", "config.json")
	writeTestFile(t, physicalPath, `{`)
	if err := os.Chmod(physicalPath, 0o644); err != nil {
		t.Fatal(err)
	}
	applier := newTestApplier(
		t,
		dataRoot,
		&fakeSecretResolver{values: map[render.SecretReference]SecretValue{}},
		&fakeUpstream{},
		&fakeActivity{},
	)
	manifest := render.Manifest{
		ProviderInstances: map[string]render.ProviderInstance{"seed": {}},
		Files: []render.FileTarget{{
			InstanceID: "seed",
			Path:       logicalPath,
			Mode:       render.WriteModeSeedIfAbsent,
			Format:     render.FileFormatJSON,
			Content:    `{"seeded":true}`,
		}},
	}

	transaction, err := applier.stageFiles(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(transaction.files) != 1 {
		t.Fatalf("unparseable file was not staged: %#v", transaction.files)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	raw, mode, exists, err := readManagedFile(physicalPath)
	if err != nil || !exists || string(raw) != `{"seeded":true}` || mode != 0o600 {
		t.Fatalf("unparseable file was not safely reseeded: content=%q mode=%#o exists=%v err=%v", raw, mode, exists, err)
	}

	writeTestFile(t, physicalPath, `{"userOwned":true}`)
	transaction, err = applier.stageFiles(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(transaction.files) != 0 {
		t.Fatalf("parseable file was not preserved: %#v", transaction.files)
	}
	raw, err = os.ReadFile(physicalPath)
	if err != nil || string(raw) != `{"userOwned":true}` {
		t.Fatalf("parseable file changed: content=%q err=%v", raw, err)
	}
}

func TestStageFilesRejectsInvalidDeclaredContent(t *testing.T) {
	applier := newTestApplier(
		t,
		t.TempDir(),
		&fakeSecretResolver{values: map[render.SecretReference]SecretValue{}},
		&fakeUpstream{},
		&fakeActivity{},
	)
	manifest := render.Manifest{
		ProviderInstances: map[string]render.ProviderInstance{"codex": {}},
		Files: []render.FileTarget{{
			InstanceID: "codex",
			Path:       "/data/harnesses/codex/config.json",
			Mode:       render.WriteModeReplace,
			Format:     render.FileFormatJSON,
			Content:    `{`,
		}},
	}
	if _, err := applier.stageFiles(manifest); err == nil || !strings.Contains(err.Error(), "invalid JSON content") {
		t.Fatalf("invalid declared content passed staging: %v", err)
	}
}

func driverManifest(t *testing.T, driver string, reference render.SecretReference, withMCP bool) render.Manifest {
	t.Helper()
	harness := render.Harness{InstanceID: driverInstanceID(driver), Driver: driver, Enabled: true}
	if withMCP {
		harness.MCPServers = []render.MCPServer{{
			Name:      "remote",
			Transport: "http",
			Config:    json.RawMessage(`{"url":"https://example.test/mcp"}`),
			Headers:   []render.Header{{Name: "Authorization", Prefix: "Bearer ", ValueFrom: &reference}},
		}}
	}
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []render.Harness{harness},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func driverInstanceID(driver string) string {
	switch driver {
	case "claudeAgent":
		return "claude"
	default:
		return driver
	}
}
