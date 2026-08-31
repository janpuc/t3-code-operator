package apply

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janpuc/t3-code-operator/internal/render"
)

func TestApplyPreservesHarnessStateAndMaterializesSecretsInMemory(t *testing.T) {
	dataRoot := t.TempDir()
	target := filepath.Join(dataRoot, "harnesses", "claude", "claude", ".claude.json")
	writeTestFile(t, target, `{"oauth":{"accessToken":"harness-owned"},"theme":"dark"}`)

	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret-canary", Version: "1"},
	}}
	upstream := &fakeUpstream{}
	applier := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})
	manifest := testManifest(t, reference, "https://first.example/mcp")

	report, err := applier.Apply(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != ApplyStateProgrammed || report.LiveRevision != manifest.DesiredRevision {
		t.Fatalf("unexpected report: %#v", report)
	}

	var current map[string]any
	readJSONFile(t, target, &current)
	if current["theme"] != "dark" || current["oauth"].(map[string]any)["accessToken"] != "harness-owned" {
		t.Fatalf("Merge clobbered harness state: %#v", current)
	}
	if _, ok := current["mcpServers"]; !ok {
		t.Fatalf("managed MCP state is missing: %#v", current)
	}
	if raw, err := os.ReadFile(target); err != nil || strings.Contains(string(raw), "resolved-secret-canary") {
		t.Fatalf("resolved Secret entered the harness file: err=%v content=%s", err, raw)
	}

	if len(upstream.calls) != 1 {
		t.Fatalf("expected one upstream call, got %d", len(upstream.calls))
	}
	if len(upstream.settingsCalls) != 1 ||
		upstream.settingsCalls[0].EnableProviderUpdateChecks != manifest.ServerSettings.EnableProviderUpdateChecks {
		t.Fatalf("managed server settings changed during materialization: %#v", upstream.settingsCalls)
	}
	variable := findUpstreamEnvironment(t, upstream.calls[0]["claude"])
	if variable.Value != "resolved-secret-canary" || !variable.Sensitive {
		t.Fatalf("Secret was not materialized for upstream: %#v", variable)
	}
	state, err := os.ReadFile(filepath.Join(dataRoot, "t3-coded", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), "resolved-secret-canary") || !strings.Contains(string(state), manifest.DesiredRevision) {
		t.Fatalf("persisted state is not Secret-safe: %s", state)
	}
}

func TestFailedRevisionRestoresLastKnownGoodFilesAndSettings(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	upstream := &fakeUpstream{}
	applier := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})
	first := testManifest(t, reference, "https://first.example/mcp")
	if _, err := applier.Apply(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dataRoot, "harnesses", "claude", "claude", ".claude.json")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	upstream.failNext = true
	second := testManifest(t, reference, "https://second.example/mcp")
	report, err := applier.Apply(context.Background(), second)
	if err == nil {
		t.Fatal("expected the upstream failure")
	}
	if report.LiveRevision != first.DesiredRevision || report.State != ApplyStateFailed {
		t.Fatalf("last-known-good report was lost: %#v", report)
	}
	after, readErr := os.ReadFile(target)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("file rollback failed: err=%v\nbefore=%s\nafter=%s", readErr, before, after)
	}
	if len(upstream.calls) != 3 {
		t.Fatalf("expected initial, failed, and rollback settings calls; got %d", len(upstream.calls))
	}
}

func TestDisruptiveApplyDefersWhileWorkIsActive(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	upstream := &fakeUpstream{}
	activity := &fakeActivity{active: []string{"claude"}}
	applier := newTestApplier(t, dataRoot, secrets, upstream, activity)
	manifest := testManifest(t, reference, "https://first.example/mcp")

	report, err := applier.Apply(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != ApplyStateDeferred || report.LiveRevision != "" {
		t.Fatalf("unexpected deferred report: %#v", report)
	}
	if len(upstream.calls) != 0 {
		t.Fatal("deferred apply changed upstream settings")
	}
	target := filepath.Join(dataRoot, "harnesses", "claude", "claude", ".claude.json")
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deferred apply changed a file: %v", err)
	}
}

func TestSecretRotationReappliesTheSameDesiredRevision(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "first-value", Version: "1"},
	}}
	upstream := &fakeUpstream{}
	applier := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})
	manifest := testManifest(t, reference, "https://first.example/mcp")

	first, err := applier.Apply(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	secrets.values[reference] = SecretValue{Value: "second-value", Version: "2"}
	second, err := applier.Apply(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if first.LiveRevision != second.LiveRevision || first.MaterializationRevision == second.MaterializationRevision {
		t.Fatalf("Secret rotation revisions are wrong: first=%#v second=%#v", first, second)
	}
	if len(upstream.calls) != 2 || findUpstreamEnvironment(t, upstream.calls[1]["claude"]).Value != "second-value" {
		t.Fatalf("rotated Secret did not reach upstream: %#v", upstream.calls)
	}
}

func TestFailedSecretRotationRestoresTheResolvedLastKnownGoodSettings(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "first-value", Version: "1"},
	}}
	upstream := &fakeUpstream{}
	applier := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})
	manifest := testManifest(t, reference, "https://first.example/mcp")
	first, err := applier.Apply(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}

	secrets.values[reference] = SecretValue{Value: "second-value", Version: "2"}
	upstream.failNext = true
	failed, err := applier.Apply(context.Background(), manifest)
	if err == nil {
		t.Fatal("expected the injected upstream failure")
	}
	if failed.LiveRevision != first.LiveRevision ||
		failed.MaterializationRevision != first.MaterializationRevision {
		t.Fatalf("failed rotation changed the live report: first=%#v failed=%#v", first, failed)
	}
	if len(upstream.calls) != 3 {
		t.Fatalf("expected initial, failed, and rollback settings calls; got %d", len(upstream.calls))
	}
	failedValue := findUpstreamEnvironment(t, upstream.calls[1]["claude"]).Value
	rollbackValue := findUpstreamEnvironment(t, upstream.calls[2]["claude"]).Value
	if failedValue != "second-value" || rollbackValue != "first-value" {
		t.Fatalf("Secret rollback used wrong values: failed=%q rollback=%q", failedValue, rollbackValue)
	}
}

func TestLiveManifestIsIsolatedFromCallerMutations(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	applier := newTestApplier(t, dataRoot, secrets, &fakeUpstream{}, &fakeActivity{})
	manifest := testManifest(t, reference, "https://first.example/mcp")
	wantRevision := manifest.DesiredRevision
	if _, err := applier.Apply(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}

	delete(manifest.ProviderInstances, "claude")
	manifest.Files[0].Content = "caller mutation"
	live, exists, err := applier.LiveManifest()
	if err != nil || !exists {
		t.Fatalf("read live manifest: exists=%v err=%v", exists, err)
	}
	live.DesiredRevision = "caller mutation"
	delete(live.ProviderInstances, "claude")
	live.Files[0].Content = "caller mutation"

	again, exists, err := applier.LiveManifest()
	if err != nil || !exists {
		t.Fatalf("read live manifest again: exists=%v err=%v", exists, err)
	}
	if again.DesiredRevision != wantRevision || again.ProviderInstances["claude"].Driver != "claudeAgent" ||
		again.Files[0].Content == "caller mutation" {
		t.Fatalf("caller mutated live state: %#v", again)
	}
}

func TestRefreshDetectsSecretRotationWithoutChangingLiveState(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "first-value", Version: "1"},
	}}
	applier := newTestApplier(t, dataRoot, secrets, &fakeUpstream{}, &fakeActivity{})
	manifest := testManifest(t, reference, "https://first.example/mcp")
	if _, err := applier.Apply(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	secrets.values[reference] = SecretValue{Value: "second-value", Version: "2"}

	report, needsApply, err := applier.Refresh(context.Background())
	if err != nil || !needsApply || report.State != ApplyStateDeferred || report.Reason != "SecretChanged" ||
		report.LiveRevision != manifest.DesiredRevision {
		t.Fatalf("Secret rotation refresh was wrong: report=%#v needsApply=%v err=%v", report, needsApply, err)
	}
}

func TestRefreshDetectsAuthoritativeProviderDrift(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	upstream := &fakeUpstream{}
	applier := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})
	manifest := testManifest(t, reference, "https://first.example/mcp")
	if _, err := applier.Apply(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	upstream.drifted = true

	report, needsApply, err := applier.Refresh(context.Background())
	if err != nil || !needsApply || report.State != ApplyStateFailed || report.Reason != "DriftDetected" ||
		report.LiveRevision != manifest.DesiredRevision {
		t.Fatalf("upstream drift refresh was wrong: report=%#v needsApply=%v err=%v", report, needsApply, err)
	}
}

func TestMalformedMergeInputKeepsLastKnownGoodState(t *testing.T) {
	dataRoot := t.TempDir()
	target := filepath.Join(dataRoot, "harnesses", "claude", "claude", ".claude.json")
	writeTestFile(t, target, `{not-json`)
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	upstream := &fakeUpstream{}
	applier := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})

	report, err := applier.Apply(context.Background(), testManifest(t, reference, "https://first.example/mcp"))
	if err == nil || report.State != ApplyStateFailed {
		t.Fatalf("expected malformed input failure, got report=%#v err=%v", report, err)
	}
	if len(upstream.calls) != 0 {
		t.Fatal("malformed file changed upstream settings")
	}
	raw, readErr := os.ReadFile(target)
	if readErr != nil || string(raw) != `{not-json` {
		t.Fatalf("malformed file changed: err=%v content=%s", readErr, raw)
	}
}

func TestUnknownProtocolKeepsTheSidecarFailOpen(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	upstream := &fakeUpstream{}
	applier := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})
	manifest := testManifest(t, reference, "https://first.example/mcp")
	manifest.APIVersion = "unknown.example/v9"

	report, err := applier.Apply(context.Background(), manifest)
	if err == nil || report.State != ApplyStateFailed || len(upstream.calls) != 0 {
		t.Fatalf("unknown protocol was not rejected safely: report=%#v err=%v", report, err)
	}
}

func TestUpstreamFailureRollsBackCommittedExtensions(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	upstream := &fakeUpstream{failNext: true}
	extensionTransaction := &recordingExtensionTransaction{}
	extensions := &fakeExtensionManager{transaction: extensionTransaction}
	applier, err := New(Config{
		DataRoot:   dataRoot,
		Secrets:    secrets,
		Upstream:   upstream,
		Activity:   &fakeActivity{},
		Extensions: extensions,
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := applier.Apply(context.Background(), testManifestWithExtension(t, reference))
	if err == nil || report.State != ApplyStateFailed || report.Reason != "UpstreamApplyFailed" {
		t.Fatalf("expected an upstream failure, got report=%#v err=%v", report, err)
	}
	if extensionTransaction.commitCalls != 1 || extensionTransaction.rollbackCalls != 1 || extensionTransaction.finalizeCalls != 0 {
		t.Fatalf("unexpected Extension transaction calls: %#v", extensionTransaction)
	}
}

func TestUpstreamFailureRollsBackCommittedTools(t *testing.T) {
	dataRoot := t.TempDir()
	toolTransaction := &recordingToolTransaction{}
	tools := &fakeToolManager{transaction: toolTransaction}
	applier, err := New(Config{
		DataRoot: dataRoot,
		Secrets:  &fakeSecretResolver{values: map[render.SecretReference]SecretValue{}},
		Upstream: &fakeUpstream{failNext: true},
		Activity: &fakeActivity{},
		Tools:    tools,
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := applier.Apply(context.Background(), testManifestWithTool(t, "14.1.1", "a"))
	if err == nil || report.State != ApplyStateFailed || report.Reason != "UpstreamApplyFailed" {
		t.Fatalf("expected an upstream failure, got report=%#v err=%v", report, err)
	}
	if toolTransaction.commitCalls != 1 || toolTransaction.rollbackCalls != 1 || toolTransaction.finalizeCalls != 0 {
		t.Fatalf("unexpected tool transaction calls: %#v", toolTransaction)
	}
}

func TestApplyCommitsManagedFilesBeforeNativeInstallerState(t *testing.T) {
	dataRoot := t.TempDir()
	settingsPath := filepath.Join(dataRoot, "harnesses", "claude", "claude", "settings.json")
	extensionTransaction := &recordingExtensionTransaction{
		commitAction: func() error {
			raw, err := os.ReadFile(settingsPath)
			if err != nil {
				return err
			}
			settings := map[string]any{}
			if err := json.Unmarshal(raw, &settings); err != nil {
				return err
			}
			settings["enabledPlugins"] = map[string]any{"memini@memini": true}
			raw, err = json.Marshal(settings)
			if err != nil {
				return err
			}
			return os.WriteFile(settingsPath, raw, 0o600)
		},
	}
	applier, err := New(Config{
		DataRoot: dataRoot,
		Secrets:  &fakeSecretResolver{values: map[render.SecretReference]SecretValue{}},
		Upstream: &fakeUpstream{},
		Activity: &fakeActivity{},
		Extensions: &fakeExtensionManager{
			transaction: extensionTransaction,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []render.Harness{{
			InstanceID: "claude",
			Driver:     "claudeAgent",
			Enabled:    true,
			Config:     json.RawMessage(`{"file":{"effortLevel":"xhigh"}}`),
			Extensions: []render.Extension{{
				Name:   "memini",
				Source: testMarketplaceSource(nil),
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := applier.Apply(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != ApplyStateProgrammed {
		t.Fatalf("unexpected report: %#v", report)
	}
	settings := map[string]any{}
	readJSONFile(t, settingsPath, &settings)
	if settings["effortLevel"] != "xhigh" || settings["enabledPlugins"] == nil {
		t.Fatalf("managed and installer-owned settings did not survive: %#v", settings)
	}
}

func testManifest(t *testing.T, reference render.SecretReference, endpoint string) render.Manifest {
	t.Helper()
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []render.Harness{{
			InstanceID: "claude",
			Driver:     "claudeAgent",
			Enabled:    true,
			MCPServers: []render.MCPServer{{
				Name:      "remote",
				Transport: "http",
				Config:    json.RawMessage(`{"url":"` + endpoint + `"}`),
				Headers:   []render.Header{{Name: "Authorization", Prefix: "Bearer ", ValueFrom: &reference}},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func testManifestWithExtension(t *testing.T, reference render.SecretReference) render.Manifest {
	t.Helper()
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []render.Harness{{
			InstanceID: "codex",
			Driver:     "codex",
			Enabled:    true,
			Environment: []render.EnvironmentVariable{{
				Name:      "OPENAI_API_KEY",
				ValueFrom: &reference,
			}},
			Extensions: []render.Extension{{
				Name:   "research",
				Source: testGitSource(nil),
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func testManifestWithTool(t *testing.T, version, digestCharacter string) render.Manifest {
	t.Helper()
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Tools: []render.ResolvedTool{{
			Name:    "rg",
			Backend: "aqua:BurntSushi/ripgrep",
			Version: version,
			Artifacts: []render.ToolArtifact{{
				Platform: "linux-x64",
				URL:      "https://example.test/rg-" + version + ".tar.gz",
				SHA256:   "sha256:" + strings.Repeat(digestCharacter, 64),
			}},
		}},
		Harnesses: []render.Harness{{
			InstanceID: "codex",
			Driver:     "codex",
			Enabled:    true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func newTestApplier(
	t *testing.T,
	dataRoot string,
	secrets SecretResolver,
	upstream UpstreamClient,
	activity ActivityReader,
) *Applier {
	t.Helper()
	applier, err := New(Config{DataRoot: dataRoot, Secrets: secrets, Upstream: upstream, Activity: activity})
	if err != nil {
		t.Fatal(err)
	}
	return applier
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSONFile(t *testing.T, path string, output any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, output); err != nil {
		t.Fatal(err)
	}
}

type fakeSecretResolver struct {
	values map[render.SecretReference]SecretValue
}

func (resolver *fakeSecretResolver) Resolve(_ context.Context, reference render.SecretReference) (SecretValue, error) {
	value, ok := resolver.values[reference]
	if !ok {
		return SecretValue{}, errors.New("Secret reference is unavailable")
	}
	return value, nil
}

type fakeUpstream struct {
	calls         []map[string]ProviderInstance
	settingsCalls []ManagedSettings
	failNext      bool
	drifted       bool
	refreshErr    error
}

func (upstream *fakeUpstream) ApplyManagedSettings(_ context.Context, settings ManagedSettings) error {
	upstream.calls = append(upstream.calls, settings.ProviderInstances)
	upstream.settingsCalls = append(upstream.settingsCalls, settings)
	if upstream.failNext {
		upstream.failNext = false
		return errors.New("injected upstream failure")
	}
	return nil
}

func (upstream *fakeUpstream) ManagedSettingsMatch(
	_ context.Context,
	_ ManagedSettings,
) (bool, error) {
	if upstream.refreshErr != nil {
		return false, upstream.refreshErr
	}
	return !upstream.drifted, nil
}

type fakeActivity struct {
	active []string
}

type fakeExtensionManager struct {
	transaction ExtensionTransaction
	stageCalls  int
}

type fakeToolManager struct {
	transaction ToolTransaction
	stageCalls  int
}

func (manager *fakeToolManager) Stage(
	_ context.Context,
	_ []render.ToolActivation,
	_ []render.ToolActivation,
) (ToolTransaction, error) {
	manager.stageCalls++
	return manager.transaction, nil
}

type recordingToolTransaction struct {
	commitCalls   int
	rollbackCalls int
	finalizeCalls int
}

func (transaction *recordingToolTransaction) Commit() error {
	transaction.commitCalls++
	return nil
}

func (transaction *recordingToolTransaction) Rollback() error {
	transaction.rollbackCalls++
	return nil
}

func (transaction *recordingToolTransaction) Finalize() error {
	transaction.finalizeCalls++
	return nil
}

func (manager *fakeExtensionManager) Stage(
	_ context.Context,
	_ []render.ExtensionActivation,
	_ []render.ExtensionActivation,
	_ map[render.SecretReference]SecretValue,
) (ExtensionTransaction, error) {
	manager.stageCalls++
	return manager.transaction, nil
}

func (activity *fakeActivity) ActiveInstances(_ context.Context, _ []string) ([]string, error) {
	return append([]string(nil), activity.active...), nil
}

func findUpstreamEnvironment(t *testing.T, instance ProviderInstance) ProviderEnvironment {
	t.Helper()
	for _, variable := range instance.Environment {
		if variable.Sensitive {
			return variable
		}
	}
	t.Fatalf("sensitive upstream environment is missing: %#v", instance.Environment)
	return ProviderEnvironment{}
}
