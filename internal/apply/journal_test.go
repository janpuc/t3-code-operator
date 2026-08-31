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

func TestRestartRecoversCrashBetweenFileCommits(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	upstream := &fakeUpstream{}
	applier := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})
	first := twoProviderManifest(t, reference, "https://first.example/mcp", "first-model")
	if _, err := applier.Apply(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	baseline := readManifestFiles(t, applier, first)

	second := twoProviderManifest(t, reference, "https://second.example/mcp", "second-model")
	files, err := applier.stageFiles(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(files.files) != 2 {
		t.Fatalf("expected two staged files, got %d", len(files.files))
	}
	if _, err := applier.prepareJournal(second, files); err != nil {
		t.Fatal(err)
	}
	partial := files.files[0]
	if err := atomicWriteFile(partial.physicalPath, partial.desired, partial.desiredMode); err != nil {
		t.Fatal(err)
	}
	if string(partial.desired) == string(baseline[partial.logicalPath]) {
		t.Fatal("test did not create a partial file commit")
	}

	restarted := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})
	deferred, err := restarted.recoverPending(context.Background())
	if err != nil || deferred {
		t.Fatalf("recovery failed: deferred=%v err=%v", deferred, err)
	}
	assertManifestFilesEqual(t, restarted, first, baseline)
	if restarted.pendingTransaction != nil {
		t.Fatal("recovered transaction remains pending")
	}
}

func TestRestartRecoversWorkstationFileCommit(t *testing.T) {
	dataRoot := t.TempDir()
	workspaceRoot := t.TempDir()
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{}}
	upstream := &fakeUpstream{}
	newApplier := func() *Applier {
		applier, err := New(Config{
			DataRoot:      dataRoot,
			WorkspaceRoot: workspaceRoot,
			Secrets:       secrets,
			Upstream:      upstream,
			Activity:      &fakeActivity{},
		})
		if err != nil {
			t.Fatal(err)
		}
		return applier
	}

	applier := newApplier()
	first := workstationJournalManifest(t, "First Workstation", "First Agent")
	if _, err := applier.Apply(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	baseline := readManifestFiles(t, applier, first)

	second := workstationJournalManifest(t, "Second Workstation", "Second Agent")
	materialized, err := materializeManifest(context.Background(), secrets, second)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := applier.prepareFileTargets(context.Background(), materialized.files)
	if err != nil {
		t.Fatal(err)
	}
	files, err := applier.stageFiles(second, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if len(files.files) != 2 {
		t.Fatalf("expected two staged Workstation files, got %d", len(files.files))
	}
	journal, err := applier.prepareJournal(second, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range journal.record.Files {
		if file.Scope != render.FileScopeWorkstation || file.InstanceID != "" {
			t.Fatalf("journal lost Workstation file identity: %#v", file)
		}
	}
	partial := files.files[0]
	if err := atomicWriteFile(partial.physicalPath, partial.desired, partial.desiredMode); err != nil {
		t.Fatal(err)
	}

	restarted := newApplier()
	deferred, err := restarted.recoverPending(context.Background())
	if err != nil || deferred {
		t.Fatalf("Workstation file recovery failed: deferred=%v err=%v", deferred, err)
	}
	assertManifestFilesEqual(t, restarted, first, baseline)
	if restarted.pendingTransaction != nil {
		t.Fatal("recovered Workstation transaction remains pending")
	}
}

func TestRestartRecoversCrashAfterUpstreamCommit(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	upstream := &fakeUpstream{}
	applier := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})
	first := twoProviderManifest(t, reference, "https://first.example/mcp", "first-model")
	if _, err := applier.Apply(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	baseline := readManifestFiles(t, applier, first)

	second := twoProviderManifest(t, reference, "https://second.example/mcp", "second-model")
	files, err := applier.stageFiles(second)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := applier.prepareJournal(second, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := journal.setPhase(transactionFilesCommitted); err != nil {
		t.Fatal(err)
	}
	materialized, err := materializeManifest(context.Background(), secrets, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := upstream.ApplyManagedSettings(context.Background(), materialized.settings); err != nil {
		t.Fatal(err)
	}
	if err := journal.setPhase(transactionUpstreamCommitted); err != nil {
		t.Fatal(err)
	}

	restarted := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})
	deferred, err := restarted.recoverPending(context.Background())
	if err != nil || deferred {
		t.Fatalf("recovery failed: deferred=%v err=%v", deferred, err)
	}
	assertManifestFilesEqual(t, restarted, first, baseline)
	if len(upstream.calls) != 3 {
		t.Fatalf("expected baseline, interrupted, and recovered settings calls; got %d", len(upstream.calls))
	}
	config := string(upstream.calls[2]["codex"].Config)
	if !strings.Contains(config, "first-model") || strings.Contains(config, "second-model") {
		t.Fatalf("upstream settings were not recovered: %s", config)
	}
}

func TestRecoveryWaitsForActiveWork(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	upstream := &fakeUpstream{}
	applier := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{})
	first := twoProviderManifest(t, reference, "https://first.example/mcp", "first-model")
	if _, err := applier.Apply(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := twoProviderManifest(t, reference, "https://second.example/mcp", "second-model")
	files, err := applier.stageFiles(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.prepareJournal(second, files); err != nil {
		t.Fatal(err)
	}
	partial := files.files[0]
	if err := atomicWriteFile(partial.physicalPath, partial.desired, partial.desiredMode); err != nil {
		t.Fatal(err)
	}

	restarted := newTestApplier(t, dataRoot, secrets, upstream, &fakeActivity{active: []string{"claude"}})
	deferred, err := restarted.recoverPending(context.Background())
	if err != nil || !deferred {
		t.Fatalf("active recovery was not deferred: deferred=%v err=%v", deferred, err)
	}
	if restarted.pendingTransaction == nil {
		t.Fatal("deferred recovery discarded its journal")
	}
}

func TestApplyCleansACommittedJournalBeforeRetry(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	applier := newTestApplier(t, dataRoot, secrets, &fakeUpstream{}, &fakeActivity{})
	manifest := testManifest(t, reference, "https://first.example/mcp")
	if _, err := applier.Apply(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}

	digest := strings.TrimPrefix(manifest.DesiredRevision, "sha256:")
	directory := filepath.Join(applier.transactionsPath(), digest)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(directory, "journal.json"), `{}`)

	report, err := applier.Apply(context.Background(), manifest)
	if err != nil || report.State != ApplyStateProgrammed {
		t.Fatalf("retry failed: report=%#v err=%v", report, err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed journal remains after retry: %v", err)
	}
}

func TestCommittedJournalCleanupRejectsASymlink(t *testing.T) {
	dataRoot := t.TempDir()
	reference := render.SecretReference{Namespace: "agents", Name: "provider-secrets", Key: "token"}
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{
		reference: {Value: "resolved-secret", Version: "1"},
	}}
	applier := newTestApplier(t, dataRoot, secrets, &fakeUpstream{}, &fakeActivity{})
	manifest := testManifest(t, reference, "https://first.example/mcp")
	if _, err := applier.Apply(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}

	victim := t.TempDir()
	victimPath := filepath.Join(victim, "keep")
	writeTestFile(t, victimPath, "keep")
	digest := strings.TrimPrefix(manifest.DesiredRevision, "sha256:")
	directory := filepath.Join(applier.transactionsPath(), digest)
	if err := os.Symlink(victim, directory); err != nil {
		t.Fatal(err)
	}
	if err := applier.cleanupCommittedJournal(); err == nil {
		t.Fatal("expected symbolic-link cleanup rejection")
	}
	if raw, err := os.ReadFile(victimPath); err != nil || string(raw) != "keep" {
		t.Fatalf("cleanup changed the external directory: content=%q err=%v", raw, err)
	}
}

func TestRestartRecoversCommittedExtensionLinks(t *testing.T) {
	dataRoot := t.TempDir()
	manager := newTestExtensionManager(t, CachedExtensionManagerConfig{
		DataRoot: dataRoot,
		Fetchers: map[render.ExtensionSourceType]ExtensionFetcher{
			render.ExtensionSourceGit: &recordingExtensionFetcher{},
		},
	})
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{}}
	upstream := &fakeUpstream{}
	applier, err := New(Config{
		DataRoot:   dataRoot,
		Secrets:    secrets,
		Upstream:   upstream,
		Activity:   &fakeActivity{},
		Extensions: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := extensionLinkManifest(t, strings.Repeat("a", 40))
	if _, err := applier.Apply(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dataRoot, "harnesses", "codex", "codex", "skills", "skill")
	baselineTarget, err := os.Readlink(destination)
	if err != nil {
		t.Fatal(err)
	}

	second := extensionLinkManifest(t, strings.Repeat("b", 40))
	materialized, err := materializeManifest(context.Background(), secrets, second)
	if err != nil {
		t.Fatal(err)
	}
	files, err := applier.stageFiles(second)
	if err != nil {
		t.Fatal(err)
	}
	extensions, err := applier.stageExtensions(context.Background(), second, materialized.secrets)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := applier.prepareJournal(second, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := journal.setPhase(transactionFilesCommitted); err != nil {
		t.Fatal(err)
	}
	if err := journal.setPhase(transactionExtensionsCommitting); err != nil {
		t.Fatal(err)
	}
	if err := extensions.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := journal.setPhase(transactionExtensionsCommitted); err != nil {
		t.Fatal(err)
	}
	interruptedTarget, err := os.Readlink(destination)
	if err != nil {
		t.Fatal(err)
	}
	if interruptedTarget == baselineTarget {
		t.Fatal("test did not commit the interrupted Extension revision")
	}

	restarted, err := New(Config{
		DataRoot:   dataRoot,
		Secrets:    secrets,
		Upstream:   upstream,
		Activity:   &fakeActivity{},
		Extensions: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := restarted.recoverPending(context.Background())
	if err != nil || deferred {
		t.Fatalf("Extension recovery failed: deferred=%v err=%v", deferred, err)
	}
	recoveredTarget, err := os.Readlink(destination)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredTarget != baselineTarget {
		t.Fatalf("Extension link was not recovered: got=%s want=%s", recoveredTarget, baselineTarget)
	}
	if restarted.pendingTransaction != nil {
		t.Fatal("recovered Extension transaction remains pending")
	}
}

func TestRestartRecoversCommittedToolRevision(t *testing.T) {
	dataRoot := t.TempDir()
	runtime := &fakeToolRuntime{dataRoot: dataRoot}
	tools := newTestMiseToolManager(t, dataRoot, runtime)
	secrets := &fakeSecretResolver{values: map[render.SecretReference]SecretValue{}}
	upstream := &fakeUpstream{}
	applier, err := New(Config{
		DataRoot: dataRoot,
		Secrets:  secrets,
		Upstream: upstream,
		Activity: &fakeActivity{},
		Tools:    tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := testManifestWithTool(t, "14.1.1", "a")
	if _, err := applier.Apply(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	assertActiveToolContent(t, dataRoot, "rg", "14.1.1")

	second := testManifestWithTool(t, "14.1.2", "b")
	files, err := applier.stageFiles(second)
	if err != nil {
		t.Fatal(err)
	}
	toolTransaction, err := applier.stageTools(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := applier.prepareJournal(second, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := journal.setPhase(transactionFilesCommitted); err != nil {
		t.Fatal(err)
	}
	if err := journal.setPhase(transactionToolsCommitting); err != nil {
		t.Fatal(err)
	}
	if err := toolTransaction.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := journal.setPhase(transactionToolsCommitted); err != nil {
		t.Fatal(err)
	}
	assertActiveToolContent(t, dataRoot, "rg", "14.1.2")

	restarted, err := New(Config{
		DataRoot: dataRoot,
		Secrets:  secrets,
		Upstream: upstream,
		Activity: &fakeActivity{},
		Tools:    tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := restarted.recoverPending(context.Background())
	if err != nil || deferred {
		t.Fatalf("tool recovery failed: deferred=%v err=%v", deferred, err)
	}
	assertActiveToolContent(t, dataRoot, "rg", "14.1.1")
	if restarted.pendingTransaction != nil {
		t.Fatal("recovered tool transaction remains pending")
	}
}

func extensionLinkManifest(t *testing.T, commit string) render.Manifest {
	t.Helper()
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []render.Harness{{
			InstanceID: "codex",
			Driver:     "codex",
			Enabled:    true,
			Extensions: []render.Extension{{
				Name: "agent-kit",
				Source: render.ExtensionSource{
					Type: render.ExtensionSourceGit,
					Git: &render.GitExtensionSource{
						URL:    "https://example.test/agent-kit.git",
						Commit: commit,
					},
					Include: []string{"skill"},
				},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func twoProviderManifest(
	t *testing.T,
	reference render.SecretReference,
	endpoint string,
	model string,
) render.Manifest {
	t.Helper()
	server := render.MCPServer{
		Name:      "remote",
		Transport: "http",
		Config:    json.RawMessage(`{"url":"` + endpoint + `"}`),
		Headers:   []render.Header{{Name: "Authorization", Prefix: "Bearer ", ValueFrom: &reference}},
	}
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []render.Harness{
			{InstanceID: "claude", Driver: "claudeAgent", Enabled: true, Config: json.RawMessage(`{"customModels":["` + model + `"]}`), MCPServers: []render.MCPServer{server}},
			{InstanceID: "codex", Driver: "codex", Enabled: true, Config: json.RawMessage(`{"customModels":["` + model + `"]}`), MCPServers: []render.MCPServer{server}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func workstationJournalManifest(t *testing.T, prettyHostname, userName string) render.Manifest {
	t.Helper()
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace:   "agents",
		Name:        "primary",
		UID:         "workstation-uid",
		MachineInfo: &render.MachineInfo{PrettyHostname: prettyHostname},
		Git:         &render.GitConfiguration{UserName: userName},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func readManifestFiles(t *testing.T, applier *Applier, manifest render.Manifest) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte, len(manifest.Files))
	for _, file := range manifest.Files {
		physical, err := applier.physicalManagedPath(file.Path, file.Scope, file.InstanceID)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(physical)
		if err != nil {
			t.Fatal(err)
		}
		result[file.Path] = raw
	}
	return result
}

func assertManifestFilesEqual(t *testing.T, applier *Applier, manifest render.Manifest, expected map[string][]byte) {
	t.Helper()
	for _, file := range manifest.Files {
		physical, err := applier.physicalManagedPath(file.Path, file.Scope, file.InstanceID)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(physical)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != string(expected[file.Path]) {
			t.Fatalf("file %s was not recovered\nwant=%s\ngot=%s", file.Path, expected[file.Path], raw)
		}
	}
}
