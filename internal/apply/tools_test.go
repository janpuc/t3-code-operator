package apply

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janpuc/t3-code-operator/internal/render"
)

func TestMiseToolManagerFailedInstallPreservesPriorActiveSet(t *testing.T) {
	dataRoot := t.TempDir()
	runtime := &fakeToolRuntime{dataRoot: dataRoot}
	manager := newTestMiseToolManager(t, dataRoot, runtime)
	v1 := []render.ToolActivation{miseTool("rg", "14.1.1", "a")}
	v2 := []render.ToolActivation{miseTool("rg", "14.1.2", "b")}

	transaction, err := manager.Stage(context.Background(), nil, v1)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	assertActiveToolContent(t, dataRoot, "rg", "14.1.1")
	currentBefore, err := os.Readlink(filepath.Join(dataRoot, "t3-coded", "tools", "current"))
	if err != nil {
		t.Fatal(err)
	}

	runtime.failVersion = "14.1.2"
	_, stageErr := manager.Stage(context.Background(), v1, v2)
	if stageErr == nil || !strings.Contains(stageErr.Error(), "install failed") {
		t.Fatalf("expected failed install, got %v", stageErr)
	}
	if names := FailedToolNames(stageErr); len(names) != 1 || names[0] != "rg" {
		t.Fatalf("failed tool identity was lost: %#v", names)
	}
	currentAfter, err := os.Readlink(filepath.Join(dataRoot, "t3-coded", "tools", "current"))
	if err != nil {
		t.Fatal(err)
	}
	if currentAfter != currentBefore {
		t.Fatalf("failed install changed current from %q to %q", currentBefore, currentAfter)
	}
	assertActiveToolContent(t, dataRoot, "rg", "14.1.1")
}

func TestMiseToolManagerCommitRollbackAndRemoval(t *testing.T) {
	dataRoot := t.TempDir()
	runtime := &fakeToolRuntime{dataRoot: dataRoot}
	manager := newTestMiseToolManager(t, dataRoot, runtime)
	v1 := []render.ToolActivation{miseTool("rg", "14.1.1", "a")}
	v2 := []render.ToolActivation{miseTool("rg", "14.1.2", "b")}

	first, err := manager.Stage(context.Background(), nil, v1)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}

	update, err := manager.Stage(context.Background(), v1, v2)
	if err != nil {
		t.Fatal(err)
	}
	assertActiveToolContent(t, dataRoot, "rg", "14.1.1")
	if err := update.Commit(); err != nil {
		t.Fatal(err)
	}
	assertActiveToolContent(t, dataRoot, "rg", "14.1.2")
	if err := update.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertActiveToolContent(t, dataRoot, "rg", "14.1.1")

	remove, err := manager.Stage(context.Background(), v1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := remove.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "t3-coded", "tools", "current", "bin", "rg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed tool remains active: %v", err)
	}
	installedV1, err := filepath.Glob(filepath.Join(dataRoot, "t3-coded", "mise", "toolsets", "*", "installs", "rg", "14.1.1", "bin", "rg"))
	if err != nil || len(installedV1) != 1 {
		t.Fatalf("removal deleted the retained install: paths=%#v err=%v", installedV1, err)
	}

	if _, err := manager.Stage(context.Background(), nil, v1); err != nil {
		t.Fatalf("empty live revision was not recognized: %v", err)
	}
}

func TestMiseToolManagerIsolatesSameVersionArtifactChanges(t *testing.T) {
	dataRoot := t.TempDir()
	runtime := &fakeToolRuntime{dataRoot: dataRoot}
	manager := newTestMiseToolManager(t, dataRoot, runtime)
	firstTools := []render.ToolActivation{miseTool("rg", "14.1.1", "a")}
	changedArtifact := []render.ToolActivation{miseTool("rg", "14.1.1", "b")}

	first, err := manager.Stage(context.Background(), nil, firstTools)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	activeExecutable := filepath.Join(dataRoot, "t3-coded", "tools", "current", "bin", "rg")
	firstTarget, err := os.Readlink(activeExecutable)
	if err != nil {
		t.Fatal(err)
	}

	update, err := manager.Stage(context.Background(), firstTools, changedArtifact)
	if err != nil {
		t.Fatal(err)
	}
	beforeCommit, err := os.Readlink(activeExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if beforeCommit != firstTarget {
		t.Fatal("staging changed the active same-version executable")
	}
	if err := update.Commit(); err != nil {
		t.Fatal(err)
	}
	secondTarget, err := os.Readlink(activeExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if secondTarget == firstTarget {
		t.Fatal("same-version artifact revisions shared one install path")
	}
	if err := update.Rollback(); err != nil {
		t.Fatal(err)
	}
	restoredTarget, err := os.Readlink(activeExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if restoredTarget != firstTarget {
		t.Fatalf("rollback restored %q, want %q", restoredTarget, firstTarget)
	}
}

func TestMiseToolManagerRejectsExecutableCollision(t *testing.T) {
	dataRoot := t.TempDir()
	runtime := &fakeToolRuntime{dataRoot: dataRoot, duplicate: true}
	manager := newTestMiseToolManager(t, dataRoot, runtime)
	_, err := manager.Stage(context.Background(), nil, []render.ToolActivation{miseTool("rg", "14.1.1", "a")})
	if err == nil || !strings.Contains(err.Error(), "duplicate executable") {
		t.Fatalf("expected executable collision, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dataRoot, "t3-coded", "tools", "current")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collision created an active revision: %v", err)
	}
}

func TestRenderMiseFilesUsesStrictResolvedLock(t *testing.T) {
	tool := miseTool("rg", "14.1.1", "a")
	tool.Options = map[string]string{"matching": "musl"}
	configuration, lockfile, err := renderMiseFiles([]render.ToolActivation{tool})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"[tool_config]\nlocked = true",
		`[tools."aqua:BurntSushi/ripgrep"]`,
		`version = "14.1.1"`,
		`matching = "musl"`,
	} {
		if !strings.Contains(string(configuration), expected) {
			t.Fatalf("mise configuration lacks %q:\n%s", expected, configuration)
		}
	}
	for _, expected := range []string{
		"lockfile_version = 1",
		`[[tools."aqua:BurntSushi/ripgrep"]]`,
		`backend = "aqua:BurntSushi/ripgrep"`,
		`[tools."aqua:BurntSushi/ripgrep"."platforms.linux-x64"]`,
		`checksum = "sha256:` + strings.Repeat("a", 64) + `"`,
		`url = "https://example.test/rg-14.1.1.tar.gz"`,
	} {
		if !strings.Contains(string(lockfile), expected) {
			t.Fatalf("mise lock lacks %q:\n%s", expected, lockfile)
		}
	}
}

func TestMiseRuntimeInstallsResolvedArtifact(t *testing.T) {
	if os.Getenv("T3_MISE_INTEGRATION") != "1" {
		t.Skip("set T3_MISE_INTEGRATION=1 to run the real mise test")
	}
	binary := os.Getenv("T3_MISE_BINARY")
	if binary == "" {
		binary = "mise"
	}
	dataRoot := t.TempDir()
	runtime, err := NewMiseRuntime(MiseRuntimeConfig{Binary: binary, DataRoot: dataRoot})
	if err != nil {
		t.Fatal(err)
	}
	manager := newTestMiseToolManager(t, dataRoot, runtime)
	tool := miseTool("rg", "14.1.1", "4")
	tool.Artifacts[0].URL = "https://github.com/BurntSushi/ripgrep/releases/download/14.1.1/ripgrep-14.1.1-x86_64-unknown-linux-musl.tar.gz"
	tool.Artifacts[0].SHA256 = "sha256:4cf9f2741e6c465ffdb7c26f38056a59e2a2544b51f7cc128ef28337eeae4d8e"
	transaction, err := manager.Stage(context.Background(), nil, []render.ToolActivation{tool})
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataRoot, "t3-coded", "tools", "current", "bin", "rg")
	output, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(output), "ripgrep 14.1.1") {
		t.Fatalf("unexpected ripgrep version: %s", output)
	}
}

type fakeToolRuntime struct {
	dataRoot    string
	failVersion string
	duplicate   bool
}

func (runtime *fakeToolRuntime) Prepare(_ context.Context, directory, toolDataRoot string) ([]MiseExecutable, error) {
	raw, err := os.ReadFile(filepath.Join(directory, "mise.toml"))
	if err != nil {
		return nil, err
	}
	version := ""
	for _, candidate := range []string{"14.1.1", "14.1.2"} {
		if strings.Contains(string(raw), `version = "`+candidate+`"`) {
			version = candidate
			break
		}
	}
	if version == "" {
		return nil, nil
	}
	if version == runtime.failVersion {
		return nil, errors.New("install failed")
	}
	path := filepath.Join(toolDataRoot, "installs", "rg", version, "bin", "rg")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(version), 0o700); err != nil {
		return nil, err
	}
	result := []MiseExecutable{{Name: "rg", Path: path}}
	if runtime.duplicate {
		result = append(result, MiseExecutable{Name: "rg", Path: path})
	}
	return result, nil
}

func newTestMiseToolManager(t *testing.T, dataRoot string, runtime ToolRuntime) *MiseToolManager {
	t.Helper()
	manager, err := NewMiseToolManager(MiseToolManagerConfig{DataRoot: dataRoot, Runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func miseTool(name, version, digestCharacter string) render.ToolActivation {
	return render.ToolActivation{
		Name:    name,
		Backend: "aqua:BurntSushi/ripgrep",
		Version: version,
		Artifacts: []render.ToolArtifact{{
			Platform: "linux-x64",
			URL:      "https://example.test/rg-" + version + ".tar.gz",
			SHA256:   "sha256:" + strings.Repeat(digestCharacter, 64),
		}},
		Apply: render.ApplyPolicy{
			Class:     render.ChangeClassDisruptive,
			When:      render.ApplyWhenIdle,
			Mechanism: render.ReloadNextSession,
		},
	}
}

func assertActiveToolContent(t *testing.T, dataRoot, name, want string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataRoot, "t3-coded", "tools", "current", "bin", name))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("active %s content is %q, want %q", name, raw, want)
	}
}
