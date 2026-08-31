package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/render"
	"github.com/pelletier/go-toml/v2"
)

const resolverOutputLimit = 1 << 20

var defaultToolPlatforms = []string{"linux-arm64", "linux-x64"}

type MiseToolResolverConfig struct {
	Binary         string
	CacheDirectory string
	Platforms      []string
	Timeout        time.Duration
}

type MiseToolResolver struct {
	binary         string
	cacheDirectory string
	platforms      []string
	timeout        time.Duration

	mutex sync.RWMutex
	cache map[string]render.ResolvedTool
}

func NewMiseToolResolver(config MiseToolResolverConfig) (*MiseToolResolver, error) {
	if config.Binary == "" {
		return nil, errors.New("mise binary is required")
	}
	if config.CacheDirectory == "" || !filepath.IsAbs(config.CacheDirectory) {
		return nil, errors.New("mise resolver cache directory must be absolute")
	}
	platforms := append([]string(nil), config.Platforms...)
	if len(platforms) == 0 {
		platforms = append([]string(nil), defaultToolPlatforms...)
	}
	sort.Strings(platforms)
	platforms = compactStrings(platforms)
	for _, platform := range platforms {
		if platform == "" || strings.ContainsAny(platform, "\x00\r\n") {
			return nil, errors.New("mise resolver platform is invalid")
		}
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	if err := os.MkdirAll(config.CacheDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create mise resolver cache: %w", err)
	}
	return &MiseToolResolver{
		binary:         config.Binary,
		cacheDirectory: filepath.Clean(config.CacheDirectory),
		platforms:      platforms,
		timeout:        timeout,
		cache:          make(map[string]render.ResolvedTool),
	}, nil
}

func (resolver *MiseToolResolver) Resolve(
	ctx context.Context,
	tools []t3v1alpha1.ToolSpec,
) ([]render.ResolvedTool, error) {
	if err := validateToolRequests(tools); err != nil {
		return nil, err
	}
	result := make([]render.ResolvedTool, 0, len(tools))
	for _, tool := range tools {
		resolved, err := resolver.resolveOne(ctx, tool)
		if err != nil {
			return nil, fmt.Errorf("resolve tool %s: %w", tool.Name, err)
		}
		result = append(result, resolved)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	if err := validateResolvedTools(result); err != nil {
		return nil, err
	}
	return result, nil
}

func (resolver *MiseToolResolver) resolveOne(
	ctx context.Context,
	tool t3v1alpha1.ToolSpec,
) (render.ResolvedTool, error) {
	cacheKey, err := toolResolutionCacheKey(tool)
	if err != nil {
		return render.ResolvedTool{}, err
	}
	resolver.mutex.RLock()
	cached, exists := resolver.cache[cacheKey]
	resolver.mutex.RUnlock()
	if exists {
		return cloneResolvedTool(cached), nil
	}

	resolved := render.ResolvedTool{
		Name:    tool.Name,
		Backend: tool.Backend,
		Version: tool.Version,
		Options: cloneStringMap(tool.Options),
	}
	if len(tool.Artifacts) != 0 {
		resolved.Artifacts = convertToolArtifacts(tool.Artifacts)
	} else {
		artifacts, err := resolver.resolveArtifacts(ctx, tool)
		if err != nil {
			return render.ResolvedTool{}, err
		}
		resolved.Artifacts = artifacts
	}
	if err := validateResolvedTools([]render.ResolvedTool{resolved}); err != nil {
		return render.ResolvedTool{}, err
	}
	resolver.mutex.Lock()
	resolver.cache[cacheKey] = cloneResolvedTool(resolved)
	resolver.mutex.Unlock()
	return resolved, nil
}

func (resolver *MiseToolResolver) resolveArtifacts(
	ctx context.Context,
	tool t3v1alpha1.ToolSpec,
) ([]render.ToolArtifact, error) {
	directory, err := os.MkdirTemp("", "t3-tool-resolve-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	for _, path := range []string{"data", "state", "config", "system"} {
		if err := os.MkdirAll(filepath.Join(directory, path), 0o700); err != nil {
			return nil, err
		}
	}
	if err := writeMiseResolutionConfig(directory, tool); err != nil {
		return nil, err
	}
	artifacts := make([]render.ToolArtifact, 0, len(resolver.platforms))
	for _, platform := range resolver.platforms {
		if err := os.Remove(filepath.Join(directory, "mise.lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		commandContext, cancel := context.WithTimeout(ctx, resolver.timeout)
		err := resolver.runMiseLock(commandContext, directory, platform)
		cancel()
		if err != nil {
			continue
		}
		raw, err := readBoundedResolverFile(filepath.Join(directory, "mise.lock"))
		if err != nil {
			continue
		}
		artifact, err := parseMiseLockArtifact(raw, tool.Backend, tool.Version, platform)
		if err != nil {
			continue
		}
		artifacts = append(artifacts, artifact)
	}
	if len(artifacts) == 0 {
		return nil, errors.New("mise did not resolve an artifact for a supported platform")
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Platform < artifacts[j].Platform })
	return artifacts, nil
}

func (resolver *MiseToolResolver) runMiseLock(ctx context.Context, directory, platform string) error {
	command := exec.CommandContext(
		ctx,
		resolver.binary,
		"--no-hooks", "--yes", "-C", directory,
		"lock", "--platform", platform,
	)
	command.Dir = directory
	command.Env = resolverEnvironment(directory, resolver.cacheDirectory)
	command.Stdin = strings.NewReader("")
	output := &boundedResolverBuffer{limit: resolverOutputLimit}
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("mise lock command failed")
	}
	if output.truncated {
		return errors.New("mise lock output exceeded its size limit")
	}
	return nil
}

func resolverEnvironment(directory, cacheDirectory string) []string {
	result := make([]string, 0, 24)
	for _, name := range []string{
		"PATH", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR", "TMPDIR",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	} {
		if value, exists := os.LookupEnv(name); exists {
			result = append(result, name+"="+value)
		}
	}
	return append(result,
		"MISE_DATA_DIR="+filepath.Join(directory, "data"),
		"MISE_CACHE_DIR="+cacheDirectory,
		"MISE_STATE_DIR="+filepath.Join(directory, "state"),
		"MISE_CONFIG_DIR="+filepath.Join(directory, "config"),
		"MISE_SYSTEM_CONFIG_DIR="+filepath.Join(directory, "system"),
		"MISE_GLOBAL_CONFIG_FILE="+filepath.Join(directory, "global.toml"),
		"MISE_TRUSTED_CONFIG_PATHS="+directory,
		"MISE_SAFE=1",
		"MISE_NO_HOOKS=1",
		"MISE_YES=1",
		"CI=1",
		"NO_COLOR=1",
		"TERM=dumb",
	)
}

func writeMiseResolutionConfig(directory string, tool t3v1alpha1.ToolSpec) error {
	var configuration strings.Builder
	configuration.WriteString("[settings]\nnot_found_auto_install = false\nnot_found_system_fallback = false\n\n")
	configuration.WriteString("[tools.")
	configuration.WriteString(strconv.Quote(tool.Backend))
	configuration.WriteString("]\nversion = ")
	configuration.WriteString(strconv.Quote(tool.Version))
	configuration.WriteByte('\n')
	names := make([]string, 0, len(tool.Options))
	for name := range tool.Options {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		configuration.WriteString(name)
		configuration.WriteString(" = ")
		configuration.WriteString(strconv.Quote(tool.Options[name]))
		configuration.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(directory, "mise.toml"), []byte(configuration.String()), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "global.toml"), []byte("[settings]\nnot_found_auto_install = false\nnot_found_system_fallback = false\n"), 0o600)
}

type miseLockDocument struct {
	Tools map[string][]map[string]any `toml:"tools"`
}

func parseMiseLockArtifact(
	raw []byte,
	backend string,
	version string,
	platform string,
) (render.ToolArtifact, error) {
	var document miseLockDocument
	if err := toml.Unmarshal(raw, &document); err != nil {
		return render.ToolArtifact{}, err
	}
	entries := document.Tools[backend]
	if len(entries) != 1 {
		return render.ToolArtifact{}, errors.New("mise lock has an unexpected tool entry")
	}
	entry := entries[0]
	lockedVersion, _ := entry["version"].(string)
	lockedBackend, _ := entry["backend"].(string)
	if lockedVersion != version || lockedBackend != backend {
		return render.ToolArtifact{}, errors.New("mise lock tool identity does not match")
	}
	artifactMap, ok := entry["platforms."+platform].(map[string]any)
	if !ok {
		return render.ToolArtifact{}, errors.New("mise lock platform entry is missing")
	}
	urlValue, _ := artifactMap["url"].(string)
	checksum, _ := artifactMap["checksum"].(string)
	size := int64(0)
	switch value := artifactMap["size"].(type) {
	case int64:
		size = value
	case int:
		size = int64(value)
	}
	return render.ToolArtifact{Platform: platform, URL: urlValue, SHA256: checksum, Size: size}, nil
}

func readBoundedResolverFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, resolverOutputLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > resolverOutputLimit {
		return nil, errors.New("mise lock exceeds its size limit")
	}
	return raw, nil
}

func validateResolvedTools(tools []render.ResolvedTool) error {
	_, err := render.Render(render.ResolvedWorkstation{
		Namespace: "validation",
		Name:      "validation",
		UID:       "validation",
		Tools:     tools,
	})
	return err
}

func validateToolRequests(tools []t3v1alpha1.ToolSpec) error {
	resolved := make([]render.ResolvedTool, 0, len(tools))
	for _, tool := range tools {
		artifacts := convertToolArtifacts(tool.Artifacts)
		if strings.HasPrefix(tool.Backend, "http:") && len(artifacts) == 0 {
			return fmt.Errorf("tool %s: http tools require pinned artifacts", tool.Name)
		}
		if len(artifacts) == 0 {
			artifacts = []render.ToolArtifact{{
				Platform: "linux-x64",
				URL:      "https://invalid.example/t3-tool-validation",
				SHA256:   "sha256:" + strings.Repeat("0", 64),
			}}
		}
		resolved = append(resolved, render.ResolvedTool{
			Name:      tool.Name,
			Backend:   tool.Backend,
			Version:   tool.Version,
			Options:   cloneStringMap(tool.Options),
			Artifacts: artifacts,
		})
	}
	return validateResolvedTools(resolved)
}

func convertToolArtifacts(input []t3v1alpha1.ToolArtifactSpec) []render.ToolArtifact {
	result := make([]render.ToolArtifact, 0, len(input))
	for _, artifact := range input {
		result = append(result, render.ToolArtifact{
			Platform: artifact.Platform,
			URL:      artifact.URL,
			SHA256:   artifact.SHA256,
			Size:     artifact.Size,
		})
	}
	return result
}

func toolResolutionCacheKey(tool t3v1alpha1.ToolSpec) (string, error) {
	raw, err := json.Marshal(tool)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func cloneResolvedTool(tool render.ResolvedTool) render.ResolvedTool {
	result := tool
	result.Options = cloneStringMap(tool.Options)
	result.Artifacts = append([]render.ToolArtifact(nil), tool.Artifacts...)
	return result
}

type boundedResolverBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *boundedResolverBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := buffer.limit - buffer.Len()
	if len(value) > remaining {
		buffer.truncated = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	_, _ = buffer.Buffer.Write(value)
	return originalLength, nil
}
