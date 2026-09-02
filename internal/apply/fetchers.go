package apply

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/janpuc/t3-code-operator/internal/render"
	digestpkg "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const extensionCommandErrorLimit = 16 << 10

var githubPathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

var gitCommitPinPattern = regexp.MustCompile(`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

type GitExtensionFetcher struct {
	GitBinary      string
	DataRoot       string
	KnownHostsFile string
	allowFile      bool
}

type OCIExtensionFetcher struct {
	MaxBytes   int64
	MaxEntries int
}

type boundedOCIFileStore struct {
	*file.Store
	temporaryRoot string
	budget        *extensionArchiveBudget
}

type ociCopyGuard struct {
	remainingBytes       int64
	remainingDescriptors int
}

type GitHubReleaseFetcher struct {
	BaseURL    string
	HTTPClient *http.Client
	MaxBytes   int64
}

func DefaultExtensionFetchers(dataRoot string) map[render.ExtensionSourceType]ExtensionFetcher {
	git := &GitExtensionFetcher{
		DataRoot:       dataRoot,
		KnownHostsFile: filepath.Join(dataRoot, "t3-coded", "ssh", "known_hosts"),
	}
	return map[render.ExtensionSourceType]ExtensionFetcher{
		render.ExtensionSourceGit:           git,
		render.ExtensionSourceMarketplace:   git,
		render.ExtensionSourceOCI:           &OCIExtensionFetcher{},
		render.ExtensionSourceGitHubRelease: &GitHubReleaseFetcher{},
	}
}

func (fetcher *GitExtensionFetcher) Fetch(
	ctx context.Context,
	source render.ExtensionSource,
	credential *SecretValue,
	destination string,
) error {
	repositoryURL, commit, err := gitExtensionIdentity(source)
	if err != nil {
		return err
	}
	parsedURL, err := url.Parse(repositoryURL)
	if err != nil {
		return errors.New("Git Extension URL is invalid")
	}
	if parsedURL.Scheme != "https" && parsedURL.Scheme != "ssh" && !(fetcher.allowFile && parsedURL.Scheme == "file") {
		return errors.New("Git Extension URL must use HTTPS or SSH")
	}
	if parsedURL.User != nil && parsedURL.Scheme == "https" {
		return errors.New("Git Extension HTTPS URL must not contain credentials")
	}
	if parsedURL.User != nil {
		if _, hasPassword := parsedURL.User.Password(); hasPassword {
			return errors.New("Git Extension URL must not contain a password")
		}
		if parsedURL.User.Username() == "" {
			return errors.New("Git Extension SSH username is empty")
		}
	}
	if !gitCommitPinPattern.MatchString(commit) {
		return errors.New("Git Extension commit must be a full hexadecimal object ID")
	}
	if err := requireEmptyDirectory(destination); err != nil {
		return err
	}
	gitBinary := fetcher.GitBinary
	if gitBinary == "" {
		gitBinary = "git"
	}
	repository, err := os.MkdirTemp(filepath.Dir(destination), ".git-source-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(repository)
	environment := gitExtensionEnvironment()
	var agent *extensionSSHAgent
	if parsedURL.Scheme == "ssh" {
		sshCommand, err := fetcher.prepareSSHCommand()
		if err != nil {
			return err
		}
		environment = append(environment, "GIT_SSH_COMMAND="+sshCommand)
	}
	if credential != nil {
		if err := validateGitExtensionCredential(parsedURL.Scheme, credential.Value); err != nil {
			return err
		}
		switch parsedURL.Scheme {
		case "https":
			environment = append(environment,
				"GIT_CONFIG_COUNT=1",
				"GIT_CONFIG_KEY_0=http."+repositoryURL+".extraHeader",
				"GIT_CONFIG_VALUE_0=Authorization: "+gitHTTPSAuthorization(parsedURL.Hostname(), credential.Value),
			)
		case "ssh":
			agent, err = startExtensionSSHAgent(ctx, repository, credential.Value)
			if err != nil {
				return err
			}
			defer agent.Stop()
			environment = append(environment, "SSH_AUTH_SOCK="+agent.socket)
		}
	}
	initArguments := []string{"init", "--quiet", "--bare"}
	if len(commit) == 64 {
		initArguments = append(initArguments, "--object-format=sha256")
	}
	initArguments = append(initArguments, repository)
	if err := runExtensionCommand(ctx, gitBinary, "", environment, credential, initArguments...); err != nil {
		return fmt.Errorf("initialize Git source: %w", err)
	}
	if err := runExtensionCommand(
		ctx,
		gitBinary,
		"",
		environment,
		credential,
		"-C", repository, "fetch", "--quiet", "--depth=1", "--no-tags", repositoryURL, commit,
	); err != nil {
		return fmt.Errorf("fetch pinned Git commit: %w", err)
	}
	resolved, err := outputExtensionCommand(
		ctx,
		gitBinary,
		"",
		environment,
		credential,
		"-C", repository, "rev-parse", "FETCH_HEAD^{commit}",
	)
	if err != nil {
		return fmt.Errorf("verify pinned Git commit: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(resolved), commit) {
		return errors.New("fetched Git commit does not match its pin")
	}
	if err := exportGitExtension(ctx, gitBinary, repository, environment, credential, destination); err != nil {
		return err
	}
	return nil
}

func gitHTTPSAuthorization(hostname, credential string) string {
	if strings.EqualFold(hostname, "github.com") {
		encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + credential))
		return "Basic " + encoded
	}
	return "Bearer " + credential
}

func (fetcher *GitExtensionFetcher) prepareSSHCommand() (string, error) {
	if fetcher.DataRoot == "" || !filepath.IsAbs(fetcher.DataRoot) || fetcher.KnownHostsFile == "" ||
		!filepath.IsAbs(fetcher.KnownHostsFile) || strings.ContainsAny(fetcher.KnownHostsFile, "\x00\r\n") {
		return "", errors.New("Git Extension SSH known-hosts path is invalid")
	}
	dataRoot := filepath.Clean(fetcher.DataRoot)
	knownHostsFile := filepath.Clean(fetcher.KnownHostsFile)
	relative, err := filepath.Rel(dataRoot, knownHostsFile)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("Git Extension SSH known-hosts path is outside the data root")
	}
	knownHostsDirectory := filepath.Dir(knownHostsFile)
	if err := rejectSymlinkComponents(dataRoot, knownHostsDirectory); err != nil {
		return "", fmt.Errorf("validate Git Extension SSH directory: %w", err)
	}
	if err := os.MkdirAll(knownHostsDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create Git Extension SSH directory: %w", err)
	}
	if err := os.Chmod(knownHostsDirectory, 0o700); err != nil {
		return "", fmt.Errorf("secure Git Extension SSH directory: %w", err)
	}
	if err := rejectSymlinkComponents(dataRoot, knownHostsFile); err != nil {
		return "", fmt.Errorf("validate Git Extension known-hosts file: %w", err)
	}
	file, err := os.OpenFile(knownHostsFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("prepare Git Extension known-hosts file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close Git Extension known-hosts file: %w", err)
	}
	info, err := os.Lstat(knownHostsFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Git Extension known-hosts path is not a regular file")
	}
	if err := os.Chmod(knownHostsFile, 0o600); err != nil {
		return "", fmt.Errorf("secure Git Extension known-hosts file: %w", err)
	}
	return "ssh -oBatchMode=yes -oStrictHostKeyChecking=accept-new -oUserKnownHostsFile=" + shellQuote(knownHostsFile), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func validateGitExtensionCredential(scheme, value string) error {
	if value == "" {
		return errors.New("Git Extension credential is empty")
	}
	switch scheme {
	case "https":
		if strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n\t ") {
			return errors.New("Git Extension HTTPS credential contains whitespace or control characters")
		}
	case "ssh":
		if strings.ContainsRune(value, '\x00') {
			return errors.New("Git Extension SSH credential contains a null character")
		}
	default:
		return errors.New("Git Extension credential is not supported for this URL scheme")
	}
	return nil
}

func gitExtensionIdentity(source render.ExtensionSource) (string, string, error) {
	switch source.Type {
	case render.ExtensionSourceGit:
		if source.Git == nil {
			return "", "", errors.New("Git source configuration is missing")
		}
		return source.Git.URL, source.Git.Commit, nil
	case render.ExtensionSourceMarketplace:
		if source.Marketplace == nil {
			return "", "", errors.New("Marketplace source configuration is missing")
		}
		return source.Marketplace.RepositoryURL, source.Marketplace.Commit, nil
	default:
		return "", "", fmt.Errorf("Git fetcher cannot fetch source type %s", source.Type)
	}
}

func gitExtensionEnvironment() []string {
	return append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LFS_SKIP_SMUDGE=1",
	)
}

func exportGitExtension(
	ctx context.Context,
	gitBinary string,
	repository string,
	environment []string,
	credential *SecretValue,
	destination string,
) error {
	commandContext, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandContext, gitBinary, "-C", repository, "archive", "--format=tar", "FETCH_HEAD")
	command.Env = environment
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	commandError := &boundedExtensionBuffer{limit: extensionCommandErrorLimit}
	command.Stderr = commandError
	if err := command.Start(); err != nil {
		return sanitizeExtensionError(err, credential)
	}
	extractErr := extractExtensionTarWithBudget(
		output,
		destination,
		newExtensionArchiveBudgetWithContext(defaultExtensionArchiveLimits(), ctx),
	)
	if extractErr != nil {
		cancel()
	}
	waitErr := command.Wait()
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		return extensionCommandError(waitErr, commandError.String(), credential)
	}
	return nil
}

func runExtensionCommand(
	ctx context.Context,
	name string,
	directory string,
	environment []string,
	credential *SecretValue,
	arguments ...string,
) error {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Dir = directory
	command.Env = environment
	output := &boundedExtensionBuffer{limit: extensionCommandErrorLimit}
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		return extensionCommandError(err, output.String(), credential)
	}
	return nil
}

func outputExtensionCommand(
	ctx context.Context,
	name string,
	directory string,
	environment []string,
	credential *SecretValue,
	arguments ...string,
) (string, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Dir = directory
	command.Env = environment
	standardOutput := &boundedExtensionBuffer{limit: extensionCommandErrorLimit}
	standardError := &boundedExtensionBuffer{limit: extensionCommandErrorLimit}
	command.Stdout = standardOutput
	command.Stderr = standardError
	if err := command.Run(); err != nil {
		return "", extensionCommandError(err, standardError.String(), credential)
	}
	return standardOutput.String(), nil
}

func extensionCommandError(commandErr error, output string, credential *SecretValue) error {
	message := redactExtensionCredential(strings.TrimSpace(output), credential)
	if message == "" {
		return sanitizeExtensionError(commandErr, credential)
	}
	return fmt.Errorf("%w: %s", sanitizeExtensionError(commandErr, credential), message)
}

func sanitizeExtensionError(err error, credential *SecretValue) error {
	if err == nil || credential == nil || credential.Value == "" {
		return err
	}
	return errors.New(redactExtensionCredential(err.Error(), credential))
}

func redactExtensionCredential(value string, credential *SecretValue) string {
	if credential == nil || credential.Value == "" {
		return value
	}
	value = strings.ReplaceAll(value, credential.Value, "[redacted]")
	githubBasic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + credential.Value))
	return strings.ReplaceAll(value, githubBasic, "[redacted]")
}

type boundedExtensionBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (buffer *boundedExtensionBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = buffer.buffer.Write(value)
	}
	return originalLength, nil
}

func (buffer *boundedExtensionBuffer) String() string {
	return buffer.buffer.String()
}

type extensionSSHAgent struct {
	command *exec.Cmd
	done    <-chan error
	socket  string
}

func startExtensionSSHAgent(ctx context.Context, directory, privateKey string) (*extensionSSHAgent, error) {
	socket := filepath.Join(directory, "agent.sock")
	output := &boundedExtensionBuffer{limit: extensionCommandErrorLimit}
	command := exec.CommandContext(ctx, "ssh-agent", "-D", "-a", socket)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		return nil, sanitizeExtensionError(err, &SecretValue{Value: privateKey})
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			agent := &extensionSSHAgent{command: command, done: done, socket: socket}
			add := exec.CommandContext(ctx, "ssh-add", "-")
			add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+socket, "SSH_ASKPASS_REQUIRE=never")
			add.Stdin = strings.NewReader(privateKey + "\n")
			addOutput := &boundedExtensionBuffer{limit: extensionCommandErrorLimit}
			add.Stdout = addOutput
			add.Stderr = addOutput
			if err := add.Run(); err != nil {
				agent.Stop()
				return nil, extensionCommandError(err, addOutput.String(), &SecretValue{Value: privateKey})
			}
			return agent, nil
		}
		select {
		case err := <-done:
			return nil, extensionCommandError(err, output.String(), &SecretValue{Value: privateKey})
		case <-ctx.Done():
			_ = command.Process.Kill()
			return nil, ctx.Err()
		case <-timeout.C:
			_ = command.Process.Kill()
			return nil, errors.New("SSH agent did not become ready")
		case <-ticker.C:
		}
	}
}

func (agent *extensionSSHAgent) Stop() {
	if agent == nil || agent.command == nil || agent.command.Process == nil {
		return
	}
	_ = agent.command.Process.Kill()
	select {
	case <-agent.done:
	case <-time.After(time.Second):
	}
}

func (fetcher *OCIExtensionFetcher) Fetch(
	ctx context.Context,
	source render.ExtensionSource,
	credential *SecretValue,
	destination string,
) (result error) {
	if source.Type != render.ExtensionSourceOCI || source.OCI == nil {
		return errors.New("OCI source configuration is missing")
	}
	if err := requireEmptyDirectory(destination); err != nil {
		return err
	}
	requestedDigest, err := digestpkg.Parse(source.OCI.Digest)
	if err != nil {
		return fmt.Errorf("parse OCI digest: %w", err)
	}
	repository, err := remote.NewRepository(source.OCI.Repository)
	if err != nil {
		return fmt.Errorf("open OCI repository: %w", err)
	}
	credentialFunction, err := ociCredential(repository.Reference.Registry, credential)
	if err != nil {
		return err
	}
	repository.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Cache:      auth.NewCache(),
		Credential: credentialFunction,
	}
	store, err := file.New(destination)
	if err != nil {
		return err
	}
	store.DisableOverwrite = true
	store.PreservePermissions = true
	defer func() { result = errors.Join(result, store.Close()) }()
	limits := fetcher.archiveLimits()
	target := &boundedOCIFileStore{
		Store:         store,
		temporaryRoot: filepath.Dir(destination),
		budget:        newExtensionArchiveBudgetWithContext(limits, ctx),
	}
	guard := newOCICopyGuard(limits)
	copyOptions := oras.DefaultCopyOptions
	copyOptions.Concurrency = 1
	copyOptions.PreCopy = guard.validate
	descriptor, err := oras.Copy(
		ctx,
		repository,
		requestedDigest.String(),
		target,
		requestedDigest.String(),
		copyOptions,
	)
	if err != nil {
		return sanitizeExtensionError(fmt.Errorf("pull pinned OCI source: %w", err), credential)
	}
	if descriptor.Digest != requestedDigest {
		return errors.New("pulled OCI manifest does not match its pinned digest")
	}
	return requireNonEmptyDirectory(destination)
}

func (fetcher *OCIExtensionFetcher) archiveLimits() extensionArchiveLimits {
	limits := defaultExtensionArchiveLimits()
	if fetcher.MaxBytes > 0 {
		limits.maxBytes = fetcher.MaxBytes
	}
	if fetcher.MaxEntries > 0 {
		limits.maxEntries = fetcher.MaxEntries
	}
	return limits
}

func newOCICopyGuard(limits extensionArchiveLimits) *ociCopyGuard {
	return &ociCopyGuard{
		remainingBytes:       limits.maxBytes,
		remainingDescriptors: limits.maxEntries,
	}
}

func (guard *ociCopyGuard) validate(ctx context.Context, descriptor ocispec.Descriptor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if guard == nil || guard.remainingDescriptors <= 0 {
		return errors.New("OCI Extension contains too many descriptors")
	}
	guard.remainingDescriptors--
	if descriptor.Size < 0 || descriptor.Size > guard.remainingBytes {
		return errors.New("OCI Extension exceeds its transfer size limit")
	}
	guard.remainingBytes -= descriptor.Size
	return nil
}

func (store *boundedOCIFileStore) Push(
	ctx context.Context,
	descriptor ocispec.Descriptor,
	reader io.Reader,
) error {
	name := descriptor.Annotations[ocispec.AnnotationTitle]
	if name == "" {
		return store.Store.Push(ctx, descriptor, reader)
	}
	if err := store.budget.consumeEntry(); err != nil {
		return err
	}
	if descriptor.Annotations[file.AnnotationUnpack] != "true" {
		if err := store.budget.consumeBytes(descriptor.Size); err != nil {
			return err
		}
		return store.Store.Push(ctx, descriptor, reader)
	}
	return store.preflightAndPush(ctx, descriptor, reader)
}

func (store *boundedOCIFileStore) preflightAndPush(
	ctx context.Context,
	descriptor ocispec.Descriptor,
	reader io.Reader,
) error {
	if descriptor.Size < 0 || descriptor.Size >= int64(^uint64(0)>>1) {
		return errors.New("OCI Extension layer size is invalid")
	}
	archive, err := os.CreateTemp(store.temporaryRoot, ".oci-layer-")
	if err != nil {
		return err
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	written, copyErr := io.Copy(archive, io.LimitReader(reader, descriptor.Size+1))
	closeErr := archive.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if written != descriptor.Size {
		return errors.New("OCI Extension layer size does not match its descriptor")
	}
	preflightRoot, err := os.MkdirTemp(store.temporaryRoot, ".oci-preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(preflightRoot)
	if err := extractExtensionArchiveWithBudget(archivePath, preflightRoot, store.budget); err != nil {
		return fmt.Errorf("validate OCI Extension archive: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	archive, err = os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	return store.Store.Push(ctx, descriptor, archive)
}

func ociCredential(registry string, secret *SecretValue) (auth.CredentialFunc, error) {
	if secret == nil {
		return auth.StaticCredential(registry, auth.EmptyCredential), nil
	}
	if secret.Value == "" {
		return nil, errors.New("OCI Extension credential is empty")
	}
	trimmed := strings.TrimSpace(secret.Value)
	if strings.HasPrefix(trimmed, "{") {
		if !json.Valid([]byte(trimmed)) {
			return nil, errors.New("OCI Extension Docker credential is invalid JSON")
		}
		store, err := credentials.NewMemoryStoreFromDockerConfig([]byte(trimmed))
		if err != nil {
			return nil, fmt.Errorf("parse OCI Extension Docker credential: %w", err)
		}
		return store.Get, nil
	}
	return auth.StaticCredential(registry, auth.Credential{AccessToken: secret.Value}), nil
}

func (fetcher *GitHubReleaseFetcher) Fetch(
	ctx context.Context,
	source render.ExtensionSource,
	credential *SecretValue,
	destination string,
) (result error) {
	if source.Type != render.ExtensionSourceGitHubRelease || source.GitHubRelease == nil {
		return errors.New("GitHub release source configuration is missing")
	}
	if err := requireEmptyDirectory(destination); err != nil {
		return err
	}
	release := source.GitHubRelease
	repositoryParts := strings.Split(release.Repository, "/")
	if len(repositoryParts) != 2 || !githubPathSegmentPattern.MatchString(repositoryParts[0]) || !githubPathSegmentPattern.MatchString(repositoryParts[1]) {
		return errors.New("GitHub release repository must contain one owner and repository")
	}
	if release.Tag == "" || release.Asset == "" ||
		release.Tag == "." || release.Tag == ".." || release.Asset == "." || release.Asset == ".." ||
		strings.ContainsAny(release.Tag+release.Asset, "\x00/\\") {
		return errors.New("GitHub release tag and asset must be safe path segments")
	}
	expectedDigest, err := hex.DecodeString(strings.ToLower(release.SHA256))
	if err != nil || len(expectedDigest) != sha256.Size {
		return errors.New("GitHub release SHA-256 is invalid")
	}
	baseURL := fetcher.BaseURL
	if baseURL == "" {
		baseURL = "https://github.com"
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" || base.Scheme != "https" {
		return errors.New("GitHub release base URL is invalid")
	}
	assetURL := strings.TrimRight(base.String(), "/") + "/" +
		url.PathEscape(repositoryParts[0]) + "/" + url.PathEscape(repositoryParts[1]) +
		"/releases/download/" + url.PathEscape(release.Tag) + "/" + url.PathEscape(release.Asset)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return err
	}
	if credential != nil {
		if credential.Value == "" {
			return errors.New("GitHub release credential is empty")
		}
		request.Header.Set("Authorization", "Bearer "+credential.Value)
	}
	client := releaseHTTPClient(fetcher.HTTPClient, base.Host)
	response, err := client.Do(request)
	if err != nil {
		return sanitizeExtensionError(fmt.Errorf("download GitHub release: %w", err), credential)
	}
	defer func() { result = errors.Join(result, response.Body.Close()) }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download GitHub release: HTTP status %d", response.StatusCode)
	}
	maxBytes := fetcher.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultExtensionArchiveLimits().maxBytes
	}
	if response.ContentLength > maxBytes {
		return errors.New("GitHub release exceeds its download size limit")
	}
	archive, err := os.CreateTemp(filepath.Dir(destination), ".release-archive-")
	if err != nil {
		return err
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(response.Body, maxBytes+1))
	closeErr := archive.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if written > maxBytes {
		return errors.New("GitHub release exceeds its download size limit")
	}
	if !bytes.Equal(hash.Sum(nil), expectedDigest) {
		return errors.New("GitHub release SHA-256 does not match")
	}
	return extractExtensionArchiveWithBudget(
		archivePath,
		destination,
		newExtensionArchiveBudgetWithContext(defaultExtensionArchiveLimits(), ctx),
	)
}

func releaseHTTPClient(configured *http.Client, credentialHost string) *http.Client {
	base := configured
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	originalRedirect := base.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != "https" {
			return errors.New("refuse a GitHub release redirect to non-HTTPS")
		}
		if request.URL.Host != credentialHost {
			request.Header.Del("Authorization")
		}
		if originalRedirect != nil {
			return originalRedirect(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}

func requireEmptyDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect Extension destination: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Extension destination is not a directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("Extension destination is not empty")
	}
	return nil
}

func requireNonEmptyDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return errors.New("Extension source did not contain files")
	}
	return nil
}
