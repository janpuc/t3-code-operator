package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janpuc/t3-code-operator/internal/render"
)

func TestCachedExtensionManagerFetchesEverySourceType(t *testing.T) {
	dataRoot := t.TempDir()
	fetchers := map[render.ExtensionSourceType]*recordingExtensionFetcher{}
	configuredFetchers := map[render.ExtensionSourceType]ExtensionFetcher{}
	for _, sourceType := range []render.ExtensionSourceType{
		render.ExtensionSourceGit,
		render.ExtensionSourceOCI,
		render.ExtensionSourceMarketplace,
		render.ExtensionSourceGitHubRelease,
	} {
		fetcher := &recordingExtensionFetcher{}
		fetchers[sourceType] = fetcher
		configuredFetchers[sourceType] = fetcher
	}
	installer := &recordingInstallerRunner{}
	manager := newTestExtensionManager(t, CachedExtensionManagerConfig{
		DataRoot: dataRoot,
		Fetchers: configuredFetchers,
		Installers: map[render.InstallerKind]ExtensionInstallerRunner{
			render.InstallerClaudeMarketplace:  installer,
			render.InstallerCodexReleaseBundle: installer,
		},
	})

	credentialReference := render.SecretReference{Namespace: "agents", Name: "source-token", Key: "token"}
	credential := SecretValue{Value: "extension-secret-canary", Version: "7"}
	activations := []render.ExtensionActivation{
		testDirectExtension(t, "git", testGitSource(&credentialReference), "/data/harnesses/codex/codex/skills/git"),
		testDirectExtension(t, "oci", testOCISource(&credentialReference), "/data/harnesses/codex/codex/skills/oci"),
		testInstallerExtension(t, "marketplace", testMarketplaceSource(&credentialReference), render.InstallerClaudeMarketplace),
		testInstallerExtension(t, "release", testReleaseSource(&credentialReference), render.InstallerCodexReleaseBundle),
	}

	transaction, err := manager.Stage(
		context.Background(),
		nil,
		activations,
		map[render.SecretReference]SecretValue{credentialReference: credential},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := transaction.Rollback(); err != nil {
			t.Fatal(err)
		}
	}()

	for sourceType, fetcher := range fetchers {
		if len(fetcher.calls) != 1 {
			t.Fatalf("source %s received %d fetch calls", sourceType, len(fetcher.calls))
		}
		if fetcher.calls[0].credential == nil || *fetcher.calls[0].credential != credential {
			t.Fatalf("source %s received the wrong credential: %#v", sourceType, fetcher.calls[0].credential)
		}
	}
	if len(installer.requests) != 2 {
		t.Fatalf("expected two installer requests, got %d", len(installer.requests))
	}
	for _, request := range installer.requests {
		if len(request.Desired) != 1 {
			t.Fatalf("unexpected installer request: %#v", request)
		}
		if info, err := os.Stat(request.Desired[0].CachePath); err != nil || !info.IsDir() {
			t.Fatalf("installer cache path is unavailable: path=%s err=%v", request.Desired[0].CachePath, err)
		}
	}
	assertTreeDoesNotContain(t, dataRoot, credential.Value)
}

func TestCachedExtensionManagerReusesOfflineCache(t *testing.T) {
	dataRoot := t.TempDir()
	source := testGitSource(nil)
	activation := testDirectExtension(t, "research", source, "/data/harnesses/codex/codex/skills/research")
	firstFetcher := &recordingExtensionFetcher{}
	first := newTestExtensionManager(t, CachedExtensionManagerConfig{
		DataRoot: dataRoot,
		Fetchers: map[render.ExtensionSourceType]ExtensionFetcher{
			render.ExtensionSourceGit: firstFetcher,
		},
	})
	transaction, err := first.Stage(context.Background(), nil, []render.ExtensionActivation{activation}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	if len(firstFetcher.calls) != 1 {
		t.Fatalf("expected one initial fetch, got %d", len(firstFetcher.calls))
	}

	offline := newTestExtensionManager(t, CachedExtensionManagerConfig{DataRoot: dataRoot})
	transaction, err = offline.Stage(context.Background(), nil, []render.ExtensionActivation{activation}, nil)
	if err != nil {
		t.Fatalf("cached source was not usable offline: %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestCachedExtensionManagerBoundsSourceFetch(t *testing.T) {
	dataRoot := t.TempDir()
	source := testGitSource(nil)
	activation := testDirectExtension(t, "research", source, "/data/harnesses/codex/codex/skills/research")
	manager := newTestExtensionManager(t, CachedExtensionManagerConfig{
		DataRoot:     dataRoot,
		FetchTimeout: 10 * time.Millisecond,
		Fetchers: map[render.ExtensionSourceType]ExtensionFetcher{
			render.ExtensionSourceGit: blockingExtensionFetcher{},
		},
	})

	_, err := manager.Stage(context.Background(), nil, []render.ExtensionActivation{activation}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unbounded Extension fetch returned %v", err)
	}
}

func TestCachedExtensionManagerCommitsAndRemovesOwnedSkillLinks(t *testing.T) {
	dataRoot := t.TempDir()
	activation := testDirectExtension(
		t,
		"research",
		testGitSource(nil),
		"/data/harnesses/codex/codex/skills/research",
	)
	fetcher := &recordingExtensionFetcher{}
	manager := newTestExtensionManager(t, CachedExtensionManagerConfig{
		DataRoot: dataRoot,
		Fetchers: map[render.ExtensionSourceType]ExtensionFetcher{
			render.ExtensionSourceGit: fetcher,
		},
	})

	transaction, err := manager.Stage(context.Background(), nil, []render.ExtensionActivation{activation}, nil)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dataRoot, "harnesses", "codex", "codex", "skills", "research")
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage changed the live destination: %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Finalize(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(destination); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destination is not a symbolic link: info=%v err=%v", info, err)
	}
	if raw, err := os.ReadFile(filepath.Join(destination, "SKILL.md")); err != nil || string(raw) != "managed skill\n" {
		t.Fatalf("activated content is wrong: content=%q err=%v", raw, err)
	}

	removal, err := manager.Stage(context.Background(), []render.ExtensionActivation{activation}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := removal.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned destination was not removed: %v", err)
	}
	if err := removal.Rollback(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(destination); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("rollback did not restore the owned link: info=%v err=%v", info, err)
	}
}

func TestCachedExtensionManagerRejectsUserOwnedDestination(t *testing.T) {
	dataRoot := t.TempDir()
	destination := filepath.Join(dataRoot, "harnesses", "codex", "codex", "skills", "research")
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(destination, "SKILL.md"), "user owned\n")
	activation := testDirectExtension(t, "research", testGitSource(nil), "/data/harnesses/codex/codex/skills/research")
	manager := newTestExtensionManager(t, CachedExtensionManagerConfig{
		DataRoot: dataRoot,
		Fetchers: map[render.ExtensionSourceType]ExtensionFetcher{
			render.ExtensionSourceGit: &recordingExtensionFetcher{},
		},
	})

	if _, err := manager.Stage(context.Background(), nil, []render.ExtensionActivation{activation}, nil); err == nil {
		t.Fatal("expected a destination collision")
	}
	if raw, err := os.ReadFile(filepath.Join(destination, "SKILL.md")); err != nil || string(raw) != "user owned\n" {
		t.Fatalf("collision changed user content: content=%q err=%v", raw, err)
	}
}

func TestCachedExtensionManagerRejectsDriftedOwnedDestination(t *testing.T) {
	dataRoot := t.TempDir()
	activation := testDirectExtension(t, "research", testGitSource(nil), "/data/harnesses/codex/codex/skills/research")
	manager := newTestExtensionManager(t, CachedExtensionManagerConfig{
		DataRoot: dataRoot,
		Fetchers: map[render.ExtensionSourceType]ExtensionFetcher{
			render.ExtensionSourceGit: &recordingExtensionFetcher{},
		},
	})
	transaction, err := manager.Stage(context.Background(), nil, []render.ExtensionActivation{activation}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Finalize(); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(dataRoot, "harnesses", "codex", "codex", "skills", "research")
	userTarget := filepath.Join(dataRoot, "user-skill")
	if err := os.MkdirAll(userTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(userTarget, destination); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Stage(context.Background(), []render.ExtensionActivation{activation}, nil, nil); err == nil {
		t.Fatal("expected managed destination drift rejection")
	}
	resolved, err := filepath.EvalSymlinks(destination)
	if err != nil || resolved != userTarget {
		t.Fatalf("drift rejection changed the user link: target=%q err=%v", resolved, err)
	}
}

func TestCachedExtensionManagerRecoveryAcceptsAnUncommittedRemoval(t *testing.T) {
	dataRoot := t.TempDir()
	activation := testDirectExtension(t, "research", testGitSource(nil), "/data/harnesses/codex/codex/skills/research")
	manager := newTestExtensionManager(t, CachedExtensionManagerConfig{
		DataRoot: dataRoot,
		Fetchers: map[render.ExtensionSourceType]ExtensionFetcher{
			render.ExtensionSourceGit: &recordingExtensionFetcher{},
		},
	})
	transaction, err := manager.Stage(context.Background(), nil, []render.ExtensionActivation{activation}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Finalize(); err != nil {
		t.Fatal(err)
	}

	recovery, err := manager.StageRecovery(context.Background(), nil, []render.ExtensionActivation{activation})
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := recovery.Finalize(); err != nil {
		t.Fatal(err)
	}
}

func TestCachedExtensionManagerRejectsEscapingSourceLink(t *testing.T) {
	dataRoot := t.TempDir()
	fetcher := &recordingExtensionFetcher{populate: func(destination string) error {
		return os.Symlink("../../outside", filepath.Join(destination, "skill"))
	}}
	activation := testDirectExtension(t, "research", testGitSource(nil), "/data/harnesses/codex/codex/skills/research")
	manager := newTestExtensionManager(t, CachedExtensionManagerConfig{
		DataRoot: dataRoot,
		Fetchers: map[render.ExtensionSourceType]ExtensionFetcher{
			render.ExtensionSourceGit: fetcher,
		},
	})

	if _, err := manager.Stage(context.Background(), nil, []render.ExtensionActivation{activation}, nil); err == nil {
		t.Fatal("expected an escaping source link error")
	}
}

func TestCachedExtensionManagerRollsBackAfterInstallerFailure(t *testing.T) {
	dataRoot := t.TempDir()
	direct := testDirectExtension(t, "research", testGitSource(nil), "/data/harnesses/codex/codex/skills/research")
	marketplace := testInstallerExtension(t, "plugin", testMarketplaceSource(nil), render.InstallerClaudeMarketplace)
	installerTransaction := &recordingExtensionTransaction{commitError: errors.New("injected installer failure")}
	manager := newTestExtensionManager(t, CachedExtensionManagerConfig{
		DataRoot: dataRoot,
		Fetchers: map[render.ExtensionSourceType]ExtensionFetcher{
			render.ExtensionSourceGit:         &recordingExtensionFetcher{},
			render.ExtensionSourceMarketplace: &recordingExtensionFetcher{},
		},
		Installers: map[render.InstallerKind]ExtensionInstallerRunner{
			render.InstallerClaudeMarketplace: &recordingInstallerRunner{transaction: installerTransaction},
		},
	})

	transaction, err := manager.Stage(context.Background(), nil, []render.ExtensionActivation{direct, marketplace}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err == nil {
		t.Fatal("expected an installer commit failure")
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dataRoot, "harnesses", "codex", "codex", "skills", "research")
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("link transaction was not rolled back: %v", err)
	}
	if installerTransaction.rollbackCalls != 1 {
		t.Fatalf("installer rollback calls: %d", installerTransaction.rollbackCalls)
	}
}

func newTestExtensionManager(t *testing.T, config CachedExtensionManagerConfig) *CachedExtensionManager {
	t.Helper()
	manager, err := NewCachedExtensionManager(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeExtensionTreeWritable(filepath.Join(config.DataRoot, "t3-coded", "extensions")) })
	return manager
}

func testDirectExtension(
	t *testing.T,
	name string,
	source render.ExtensionSource,
	destination string,
) render.ExtensionActivation {
	t.Helper()
	cacheKey, err := render.ExtensionCacheKey(source)
	if err != nil {
		t.Fatal(err)
	}
	return render.ExtensionActivation{
		InstanceID: "codex",
		Name:       name,
		Source:     source,
		CacheKey:   cacheKey,
		Destinations: []render.ExtensionDestination{{
			SourcePath: "skill",
			Path:       destination,
			Mode:       render.WriteModeReplace,
		}},
	}
}

func testInstallerExtension(
	t *testing.T,
	name string,
	source render.ExtensionSource,
	kind render.InstallerKind,
) render.ExtensionActivation {
	t.Helper()
	cacheKey, err := render.ExtensionCacheKey(source)
	if err != nil {
		t.Fatal(err)
	}
	return render.ExtensionActivation{
		InstanceID: "claude",
		Name:       name,
		Source:     source,
		CacheKey:   cacheKey,
		Installer:  &render.ExtensionInstaller{Kind: kind, Extension: name},
	}
}

func testGitSource(reference *render.SecretReference) render.ExtensionSource {
	return render.ExtensionSource{
		Type: render.ExtensionSourceGit,
		Git: &render.GitExtensionSource{
			URL:                 "https://example.com/skills.git",
			Commit:              strings.Repeat("a", 40),
			CredentialSecretRef: reference,
		},
	}
}

func testOCISource(reference *render.SecretReference) render.ExtensionSource {
	return render.ExtensionSource{
		Type: render.ExtensionSourceOCI,
		OCI: &render.OCIExtensionSource{
			Repository:          "registry.example.com/skills",
			Digest:              "sha256:" + strings.Repeat("b", 64),
			CredentialSecretRef: reference,
		},
	}
}

func testMarketplaceSource(reference *render.SecretReference) render.ExtensionSource {
	return render.ExtensionSource{
		Type: render.ExtensionSourceMarketplace,
		Marketplace: &render.MarketplaceExtensionSource{
			Marketplace:         "example",
			Extension:           "plugin",
			RepositoryURL:       "https://example.com/marketplace.git",
			Commit:              strings.Repeat("c", 40),
			CredentialSecretRef: reference,
		},
	}
}

func testReleaseSource(reference *render.SecretReference) render.ExtensionSource {
	return render.ExtensionSource{
		Type: render.ExtensionSourceGitHubRelease,
		GitHubRelease: &render.GitHubReleaseExtensionSource{
			Repository:          "example/plugin",
			Tag:                 "v1.0.0",
			Asset:               "plugin.tar.gz",
			SHA256:              strings.Repeat("d", 64),
			CredentialSecretRef: reference,
		},
	}
}

type extensionFetchCall struct {
	source      render.ExtensionSource
	credential  *SecretValue
	destination string
}

type recordingExtensionFetcher struct {
	calls    []extensionFetchCall
	populate func(string) error
	fail     error
}

type blockingExtensionFetcher struct{}

func (blockingExtensionFetcher) Fetch(
	ctx context.Context,
	_ render.ExtensionSource,
	_ *SecretValue,
	_ string,
) error {
	<-ctx.Done()
	return ctx.Err()
}

func (fetcher *recordingExtensionFetcher) Fetch(
	_ context.Context,
	source render.ExtensionSource,
	credential *SecretValue,
	destination string,
) error {
	credentialCopy := credential
	if credential != nil {
		copy := *credential
		credentialCopy = &copy
	}
	fetcher.calls = append(fetcher.calls, extensionFetchCall{
		source: source, credential: credentialCopy, destination: destination,
	})
	if fetcher.fail != nil {
		return fetcher.fail
	}
	if fetcher.populate != nil {
		return fetcher.populate(destination)
	}
	skill := filepath.Join(destination, "skill")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("managed skill\n"), 0o600)
}

type recordingInstallerRunner struct {
	requests    []ExtensionInstallerRequest
	transaction ExtensionTransaction
}

func (runner *recordingInstallerRunner) Stage(
	_ context.Context,
	request ExtensionInstallerRequest,
) (ExtensionTransaction, error) {
	runner.requests = append(runner.requests, request)
	if runner.transaction != nil {
		return runner.transaction, nil
	}
	return &recordingExtensionTransaction{}, nil
}

type recordingExtensionTransaction struct {
	commitCalls   int
	rollbackCalls int
	finalizeCalls int
	commitError   error
	commitAction  func() error
}

func (transaction *recordingExtensionTransaction) Commit() error {
	transaction.commitCalls++
	if transaction.commitAction != nil {
		return transaction.commitAction()
	}
	return transaction.commitError
}

func (transaction *recordingExtensionTransaction) Rollback() error {
	transaction.rollbackCalls++
	return nil
}

func (transaction *recordingExtensionTransaction) Finalize() error {
	transaction.finalizeCalls++
	return nil
}

func assertTreeDoesNotContain(t *testing.T, root, needle string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if strings.Contains(target, needle) {
				t.Fatalf("Secret value entered link %s", path)
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), needle) {
			t.Fatalf("Secret value entered file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
