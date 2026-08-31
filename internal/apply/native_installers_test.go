package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/janpuc/t3-code-operator/internal/render"
)

func TestNativeInstallerReplacesPinnedMarketplaceAndRollsBack(t *testing.T) {
	dataRoot := t.TempDir()
	oldCache := makeNativeInstallerCache(t, dataRoot, strings.Repeat("a", 64))
	newCache := makeNativeInstallerCache(t, dataRoot, strings.Repeat("b", 64))
	cli := newFakeNativePluginCLI(nativePluginState{
		marketplaces: map[string]string{"memini": oldCache},
		plugins:      map[string]bool{"memini@memini": true},
	})
	runner := testNativeInstallerRunner(dataRoot, cli)
	previous := cachedNativeActivation("codex", "memini", "memini", oldCache)
	desired := cachedNativeActivation("codex", "memini", "memini", newCache)

	transaction, err := runner.Stage(context.Background(), ExtensionInstallerRequest{
		Kind:     render.InstallerCodexMarketplace,
		Previous: []CachedExtensionActivation{previous},
		Desired:  []CachedExtensionActivation{desired},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	assertFakeNativeState(t, cli.state, newCache)
	forward := []string{
		"remove-plugin:memini@memini",
		"remove-marketplace:memini",
		"add-marketplace:memini:" + newCache,
		"install-plugin:memini@memini",
	}
	if !reflect.DeepEqual(cli.calls, forward) {
		t.Fatalf("unexpected forward operations: %#v", cli.calls)
	}

	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertFakeNativeState(t, cli.state, oldCache)
}

func TestNativeInstallerRejectsUserMarketplaceCollision(t *testing.T) {
	dataRoot := t.TempDir()
	desiredCache := makeNativeInstallerCache(t, dataRoot, strings.Repeat("c", 64))
	cli := newFakeNativePluginCLI(nativePluginState{
		marketplaces: map[string]string{"memini": "/user/marketplace"},
		plugins:      map[string]bool{"personal@memini": true},
	})
	runner := testNativeInstallerRunner(dataRoot, cli)
	transaction, err := runner.Stage(context.Background(), ExtensionInstallerRequest{
		Kind:    render.InstallerCodexMarketplace,
		Desired: []CachedExtensionActivation{cachedNativeActivation("codex", "memini", "memini", desiredCache)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("expected a user collision, got %v", err)
	}
	if len(cli.calls) != 0 {
		t.Fatalf("collision changed native state: %#v", cli.calls)
	}
}

func TestNativeInstallerRestoresPriorStateAfterPartialFailure(t *testing.T) {
	dataRoot := t.TempDir()
	oldCache := makeNativeInstallerCache(t, dataRoot, strings.Repeat("d", 64))
	newCache := makeNativeInstallerCache(t, dataRoot, strings.Repeat("e", 64))
	cli := newFakeNativePluginCLI(nativePluginState{
		marketplaces: map[string]string{"memini": oldCache},
		plugins:      map[string]bool{"memini@memini": true},
	})
	cli.failOperation = "add-marketplace"
	runner := testNativeInstallerRunner(dataRoot, cli)
	transaction, err := runner.Stage(context.Background(), ExtensionInstallerRequest{
		Kind:     render.InstallerCodexMarketplace,
		Previous: []CachedExtensionActivation{cachedNativeActivation("codex", "memini", "memini", oldCache)},
		Desired:  []CachedExtensionActivation{cachedNativeActivation("codex", "memini", "memini", newCache)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err == nil || !strings.Contains(err.Error(), "injected native failure") {
		t.Fatalf("expected a partial native failure, got %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertFakeNativeState(t, cli.state, oldCache)
}

func TestNativeInstallerCrashRecoveryReconcilesPartialState(t *testing.T) {
	dataRoot := t.TempDir()
	oldCache := makeNativeInstallerCache(t, dataRoot, strings.Repeat("f", 64))
	newCache := makeNativeInstallerCache(t, dataRoot, strings.Repeat("1", 64))
	cli := newFakeNativePluginCLI(nativePluginState{
		marketplaces: map[string]string{},
		plugins:      map[string]bool{},
	})
	runner := testNativeInstallerRunner(dataRoot, cli)
	transaction, err := runner.Stage(context.Background(), ExtensionInstallerRequest{
		Kind:       render.InstallerCodexMarketplace,
		Previous:   []CachedExtensionActivation{cachedNativeActivation("codex", "memini", "memini", newCache)},
		Desired:    []CachedExtensionActivation{cachedNativeActivation("codex", "memini", "memini", oldCache)},
		Recovering: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	assertFakeNativeState(t, cli.state, oldCache)
}

func TestParseNativePluginCLIJSON(t *testing.T) {
	codex, err := parseCodexPluginState(
		[]byte(`{"marketplaces":[{"name":"agent-kit","root":"/cache/agent-kit","marketplaceSource":{"sourceType":"local","source":"/cache/agent-kit"}}]}`),
		[]byte(`{"installed":[{"pluginId":"agent-kit@agent-kit","installed":true,"enabled":true}],"available":[]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	assertParsedNativeState(t, codex)

	claude, err := parseClaudePluginState(
		[]byte(`[{"name":"agent-kit","source":"directory","path":"/cache/agent-kit","installLocation":"/cache/agent-kit"}]`),
		[]byte(`[{"id":"agent-kit@agent-kit","version":"0.1.0","scope":"user","enabled":true,"installPath":"/home/plugins/cache"}]`),
	)
	if err != nil {
		t.Fatal(err)
	}
	assertParsedNativeState(t, claude)
}

func testNativeInstallerRunner(dataRoot string, cli nativePluginCLI) *nativeMarketplaceInstallerRunner {
	return &nativeMarketplaceInstallerRunner{
		dataRoot:        dataRoot,
		cacheRoot:       filepath.Join(dataRoot, "t3-coded", "extensions", "cache"),
		driverDirectory: "codex",
		kinds: map[render.InstallerKind]struct{}{
			render.InstallerCodexMarketplace:   {},
			render.InstallerCodexReleaseBundle: {},
		},
		cli: cli,
	}
}

func makeNativeInstallerCache(t *testing.T, dataRoot, digest string) string {
	t.Helper()
	path := filepath.Join(dataRoot, "t3-coded", "extensions", "cache", digest, "content")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeExtensionTreeWritable(filepath.Join(dataRoot, "t3-coded", "extensions")) })
	return path
}

func cachedNativeActivation(instanceID, marketplace, plugin, cachePath string) CachedExtensionActivation {
	return CachedExtensionActivation{
		Activation: render.ExtensionActivation{
			InstanceID: instanceID,
			Name:       plugin,
			Source: render.ExtensionSource{
				Type: render.ExtensionSourceMarketplace,
				Marketplace: &render.MarketplaceExtensionSource{
					Marketplace: marketplace,
					Extension:   plugin,
				},
			},
			Installer: &render.ExtensionInstaller{
				Kind:        render.InstallerCodexMarketplace,
				Marketplace: marketplace,
				Extension:   plugin,
			},
		},
		CachePath: cachePath,
	}
}

func assertFakeNativeState(t *testing.T, state nativePluginState, cachePath string) {
	t.Helper()
	if state.marketplaces["memini"] != cachePath || !state.plugins["memini@memini"] {
		t.Fatalf("unexpected native state: %#v", state)
	}
}

func assertParsedNativeState(t *testing.T, state nativePluginState) {
	t.Helper()
	if state.marketplaces["agent-kit"] != "/cache/agent-kit" || !state.plugins["agent-kit@agent-kit"] {
		t.Fatalf("unexpected parsed state: %#v", state)
	}
}

type fakeNativePluginCLI struct {
	state         nativePluginState
	calls         []string
	failOperation string
}

func newFakeNativePluginCLI(state nativePluginState) *fakeNativePluginCLI {
	return &fakeNativePluginCLI{state: cloneNativePluginState(state)}
}

func (cli *fakeNativePluginCLI) Snapshot(context.Context, string) (nativePluginState, error) {
	return cloneNativePluginState(cli.state), nil
}

func (cli *fakeNativePluginCLI) AddMarketplace(_ context.Context, _, marketplace, source string) error {
	if err := cli.maybeFail("add-marketplace"); err != nil {
		return err
	}
	cli.calls = append(cli.calls, "add-marketplace:"+marketplace+":"+source)
	cli.state.marketplaces[marketplace] = source
	return nil
}

func (cli *fakeNativePluginCLI) RemoveMarketplace(_ context.Context, _, marketplace string) error {
	if err := cli.maybeFail("remove-marketplace"); err != nil {
		return err
	}
	cli.calls = append(cli.calls, "remove-marketplace:"+marketplace)
	delete(cli.state.marketplaces, marketplace)
	return nil
}

func (cli *fakeNativePluginCLI) InstallPlugin(_ context.Context, _, pluginID string) error {
	if err := cli.maybeFail("install-plugin"); err != nil {
		return err
	}
	cli.calls = append(cli.calls, "install-plugin:"+pluginID)
	cli.state.plugins[pluginID] = true
	return nil
}

func (cli *fakeNativePluginCLI) RemovePlugin(_ context.Context, _, pluginID string) error {
	if err := cli.maybeFail("remove-plugin"); err != nil {
		return err
	}
	cli.calls = append(cli.calls, "remove-plugin:"+pluginID)
	delete(cli.state.plugins, pluginID)
	return nil
}

func (cli *fakeNativePluginCLI) maybeFail(operation string) error {
	if cli.failOperation != operation {
		return nil
	}
	cli.failOperation = ""
	return errors.New("injected native failure")
}
