package apply

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janpuc/t3-code-operator/internal/render"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	orascontent "oras.land/oras-go/v2/content"
	orasfile "oras.land/oras-go/v2/content/file"
)

func TestGitExtensionFetcherExportsPinnedCommitWithoutRepositoryState(t *testing.T) {
	repository := t.TempDir()
	runTestCommand(t, repository, "git", "init", "--quiet")
	runTestCommand(t, repository, "git", "config", "user.name", "T3 Test")
	runTestCommand(t, repository, "git", "config", "user.email", "t3@example.invalid")
	writeTestFile(t, filepath.Join(repository, "skill", "SKILL.md"), "first\n")
	runTestCommand(t, repository, "git", "add", ".")
	runTestCommand(t, repository, "git", "commit", "--quiet", "-m", "first")
	firstCommit := strings.TrimSpace(runTestCommand(t, repository, "git", "rev-parse", "HEAD"))
	writeTestFile(t, filepath.Join(repository, "skill", "SKILL.md"), "second\n")
	runTestCommand(t, repository, "git", "commit", "--quiet", "-am", "second")

	destination := filepath.Join(t.TempDir(), "content")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	fetcher := &GitExtensionFetcher{allowFile: true}
	source := render.ExtensionSource{
		Type: render.ExtensionSourceGit,
		Git: &render.GitExtensionSource{
			URL:    "file://" + repository,
			Commit: firstCommit,
		},
	}
	if err := fetcher.Fetch(context.Background(), source, nil, destination); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(destination, "skill", "SKILL.md"))
	if err != nil || string(raw) != "first\n" {
		t.Fatalf("wrong exported commit: content=%q err=%v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".git")); !os.IsNotExist(err) {
		t.Fatalf("Git repository state entered the cache: %v", err)
	}
}

func TestGitExtensionFetcherRedactsCredentialFromCommandFailure(t *testing.T) {
	root := t.TempDir()
	gitBinary := filepath.Join(root, "failing-git")
	if err := os.WriteFile(
		gitBinary,
		[]byte("#!/bin/sh\nprintf '%s' \"$GIT_CONFIG_VALUE_0\" >&2\nexit 1\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "content")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	fetcher := &GitExtensionFetcher{GitBinary: gitBinary}
	credential := &SecretValue{Value: "git-secret-canary", Version: "1"}
	err := fetcher.Fetch(context.Background(), testGitSource(nil), credential, destination)
	if err == nil || strings.Contains(err.Error(), credential.Value) {
		t.Fatalf("Git command error exposed its credential: %v", err)
	}
}

func TestGitExtensionFetcherRedactsDerivedGitHubCredentialFromCommandFailure(t *testing.T) {
	root := t.TempDir()
	gitBinary := filepath.Join(root, "failing-git")
	if err := os.WriteFile(
		gitBinary,
		[]byte("#!/bin/sh\nprintf '%s' \"$GIT_CONFIG_VALUE_0\" >&2\nexit 1\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "content")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	fetcher := &GitExtensionFetcher{GitBinary: gitBinary}
	credential := &SecretValue{Value: "git-secret-canary", Version: "1"}
	source := testGitSource(nil)
	source.Git.URL = "https://github.com/example/skills.git"
	err := fetcher.Fetch(context.Background(), source, credential, destination)
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + credential.Value))
	if err == nil || strings.Contains(err.Error(), credential.Value) || strings.Contains(err.Error(), encoded) {
		t.Fatalf("Git command error exposed its derived credential: %v", err)
	}
}

func TestGitHubReleaseFetcherChecksDigestAndStripsRedirectCredential(t *testing.T) {
	archive := testTarGzip(t, map[string]string{"plugin/SKILL.md": "release skill\n"})
	digest := sha256.Sum256(archive)
	var redirectedAuthorization string
	assetServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		redirectedAuthorization = request.Header.Get("Authorization")
		_, _ = writer.Write(archive)
	}))
	defer assetServer.Close()
	var initialAuthorization string
	releaseServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		initialAuthorization = request.Header.Get("Authorization")
		http.Redirect(writer, request, assetServer.URL+"/asset", http.StatusFound)
	}))
	defer releaseServer.Close()
	client := releaseServer.Client()
	client.Transport = roundTripperMap{
		releaseServer.Listener.Addr().String(): releaseServer.Client().Transport,
		assetServer.Listener.Addr().String():   assetServer.Client().Transport,
	}

	destination := filepath.Join(t.TempDir(), "content")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	fetcher := &GitHubReleaseFetcher{BaseURL: releaseServer.URL, HTTPClient: client}
	source := testReleaseSource(nil)
	source.GitHubRelease.SHA256 = hex.EncodeToString(digest[:])
	credential := &SecretValue{Value: "release-secret-canary", Version: "1"}
	if err := fetcher.Fetch(context.Background(), source, credential, destination); err != nil {
		t.Fatal(err)
	}
	if initialAuthorization != "Bearer "+credential.Value {
		t.Fatalf("initial release request did not receive its credential: %q", initialAuthorization)
	}
	if redirectedAuthorization != "" {
		t.Fatalf("release credential crossed hosts: %q", redirectedAuthorization)
	}
	if raw, err := os.ReadFile(filepath.Join(destination, "plugin", "SKILL.md")); err != nil || string(raw) != "release skill\n" {
		t.Fatalf("release archive was not extracted: content=%q err=%v", raw, err)
	}
}

func TestGitHubReleaseFetcherRejectsChecksumMismatchBeforeExtraction(t *testing.T) {
	archive := testTarGzip(t, map[string]string{"plugin/SKILL.md": "release skill\n"})
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(archive)
	}))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "content")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	fetcher := &GitHubReleaseFetcher{BaseURL: server.URL, HTTPClient: server.Client()}
	source := testReleaseSource(nil)
	source.GitHubRelease.SHA256 = strings.Repeat("0", 64)
	if err := fetcher.Fetch(context.Background(), source, nil, destination); err == nil {
		t.Fatal("expected a checksum mismatch")
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed release changed the destination: entries=%v err=%v", entries, err)
	}
}

func TestArchiveExtractionRejectsTraversal(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	content := []byte("escape")
	if err := writer.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := extractExtensionTar(bytes.NewReader(archive.Bytes()), root, defaultExtensionArchiveLimits()); err == nil {
		t.Fatal("expected path traversal rejection")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape")); !os.IsNotExist(err) {
		t.Fatalf("archive wrote outside its destination: %v", err)
	}
}

func TestOCICredentialAcceptsTokenAndDockerConfigInMemory(t *testing.T) {
	tokenFunction, err := ociCredential("registry.example.com", &SecretValue{Value: "registry-token", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokenFunction(context.Background(), "registry.example.com")
	if err != nil || token.AccessToken != "registry-token" {
		t.Fatalf("wrong token credential: credential=%#v err=%v", token, err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("robot:password"))
	dockerConfig := fmt.Sprintf(`{"auths":{"registry.example.com":{"auth":%q}}}`, auth)
	configFunction, err := ociCredential("registry.example.com", &SecretValue{Value: dockerConfig, Version: "2"})
	if err != nil {
		t.Fatal(err)
	}
	configured, err := configFunction(context.Background(), "registry.example.com")
	if err != nil || configured.Username != "robot" || configured.Password != "password" {
		t.Fatalf("wrong Docker credential: credential=%#v err=%v", configured, err)
	}
}

func TestOCIExtensionPreflightBoundsAggregateExpandedContent(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "content")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	fileStore, err := orasfile.New(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer fileStore.Close()
	fileStore.DisableOverwrite = true
	fileStore.PreservePermissions = true
	store := &boundedOCIFileStore{
		Store:         fileStore,
		temporaryRoot: root,
		budget:        newExtensionArchiveBudget(extensionArchiveLimits{maxBytes: 10, maxEntries: 10}),
	}

	first := testTarGzip(t, map[string]string{"one/SKILL.md": "123456"})
	if err := store.Push(context.Background(), testOCIDirectoryDescriptor(first, "one"), bytes.NewReader(first)); err != nil {
		t.Fatal(err)
	}
	second := testTarGzip(t, map[string]string{"two/SKILL.md": "abcdef"})
	err = store.Push(context.Background(), testOCIDirectoryDescriptor(second, "two"), bytes.NewReader(second))
	if err == nil || !strings.Contains(err.Error(), "extracted size limit") {
		t.Fatalf("aggregate OCI expansion passed its limit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "two")); !os.IsNotExist(err) {
		t.Fatalf("rejected OCI layer changed its destination: %v", err)
	}
}

func TestOCIExtensionPreflightRejectsArchiveTraversalBeforeORASWrite(t *testing.T) {
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	content := []byte("escape")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	destination := filepath.Join(root, "content")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	fileStore, err := orasfile.New(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer fileStore.Close()
	store := &boundedOCIFileStore{
		Store:         fileStore,
		temporaryRoot: root,
		budget:        newExtensionArchiveBudget(defaultExtensionArchiveLimits()),
	}
	err = store.Push(
		context.Background(),
		testOCIDirectoryDescriptor(archive.Bytes(), "skill"),
		bytes.NewReader(archive.Bytes()),
	)
	if err == nil || !strings.Contains(err.Error(), "parent traversal") {
		t.Fatalf("unsafe OCI archive passed preflight: %v", err)
	}
	entries, readErr := os.ReadDir(destination)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("unsafe OCI archive changed its destination: entries=%v err=%v", entries, readErr)
	}
}

func TestOCICopyGuardBoundsAggregateTransferAndDescriptors(t *testing.T) {
	guard := newOCICopyGuard(extensionArchiveLimits{maxBytes: 10, maxEntries: 2})
	if err := guard.validate(context.Background(), ocispec.Descriptor{Size: 6}); err != nil {
		t.Fatal(err)
	}
	if err := guard.validate(context.Background(), ocispec.Descriptor{Size: 5}); err == nil ||
		!strings.Contains(err.Error(), "transfer size limit") {
		t.Fatalf("aggregate OCI transfer passed its limit: %v", err)
	}

	guard = newOCICopyGuard(extensionArchiveLimits{maxBytes: 10, maxEntries: 1})
	if err := guard.validate(context.Background(), ocispec.Descriptor{Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := guard.validate(context.Background(), ocispec.Descriptor{Size: 1}); err == nil ||
		!strings.Contains(err.Error(), "too many descriptors") {
		t.Fatalf("OCI descriptor count passed its limit: %v", err)
	}
}

func testOCIDirectoryDescriptor(content []byte, name string) ocispec.Descriptor {
	descriptor := orascontent.NewDescriptorFromBytes(ocispec.MediaTypeImageLayerGzip, content)
	descriptor.Annotations = map[string]string{
		ocispec.AnnotationTitle:   name,
		orasfile.AnnotationUnpack: "true",
	}
	return descriptor
}

func TestGitCredentialValidationAllowsMultilineSSHKeys(t *testing.T) {
	privateKey := "-----BEGIN OPENSSH PRIVATE KEY-----\nmultiline\n-----END OPENSSH PRIVATE KEY-----\n"
	if err := validateGitExtensionCredential("ssh", privateKey); err != nil {
		t.Fatalf("multiline SSH key was rejected: %v", err)
	}
	for _, value := range []string{"token with space", "token\nsecond-header"} {
		if err := validateGitExtensionCredential("https", value); err == nil {
			t.Fatalf("unsafe HTTPS credential %q passed validation", value)
		}
	}
}

func TestGitHTTPSAuthorizationUsesGitHubBasicAuthentication(t *testing.T) {
	credential := "git-secret-canary"
	wantGitHub := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+credential))
	if got := gitHTTPSAuthorization("github.com", credential); got != wantGitHub {
		t.Fatalf("GitHub credential used the wrong authorization scheme: %q", got)
	}
	if got := gitHTTPSAuthorization("git.example.com", credential); got != "Bearer "+credential {
		t.Fatalf("generic Git credential changed authorization scheme: %q", got)
	}
}

func TestDefaultExtensionFetchersCoverEveryRenderedSource(t *testing.T) {
	fetchers := DefaultExtensionFetchers(t.TempDir())
	for _, sourceType := range []render.ExtensionSourceType{
		render.ExtensionSourceGit,
		render.ExtensionSourceOCI,
		render.ExtensionSourceMarketplace,
		render.ExtensionSourceGitHubRelease,
	} {
		if fetchers[sourceType] == nil {
			t.Fatalf("default fetcher %s is missing", sourceType)
		}
	}
}

func TestGitSSHCommandUsesRetainedTrustOnFirstUseState(t *testing.T) {
	dataRoot := t.TempDir()
	fetcher := &GitExtensionFetcher{
		DataRoot:       dataRoot,
		KnownHostsFile: filepath.Join(dataRoot, "t3-coded", "ssh", "known_hosts"),
	}
	command, err := fetcher.prepareSSHCommand()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "StrictHostKeyChecking=accept-new") ||
		!strings.Contains(command, "UserKnownHostsFile=") || !strings.Contains(command, fetcher.KnownHostsFile) {
		t.Fatalf("SSH command lacks retained host-key verification: %q", command)
	}
	info, err := os.Lstat(fetcher.KnownHostsFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("known-hosts file is unsafe: info=%#v err=%v", info, err)
	}
	outside := &GitExtensionFetcher{DataRoot: dataRoot, KnownHostsFile: filepath.Join(filepath.Dir(dataRoot), "outside")}
	if _, err := outside.prepareSSHCommand(); err == nil {
		t.Fatal("SSH known-hosts path escaped the data root")
	}
}

func testTarGzip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var result bytes.Buffer
	gzipWriter := gzip.NewWriter(&result)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, value := range files {
		content := []byte(value)
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func runTestCommand(t *testing.T, directory, name string, arguments ...string) string {
	t.Helper()
	command := exec.Command(name, arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v: %s", name, err, output)
	}
	return string(output)
}

type roundTripperMap map[string]http.RoundTripper

func (transports roundTripperMap) RoundTrip(request *http.Request) (*http.Response, error) {
	transport := transports[request.URL.Host]
	if transport == nil {
		return nil, fmt.Errorf("no test transport for %s", request.URL.Host)
	}
	return transport.RoundTrip(request)
}
