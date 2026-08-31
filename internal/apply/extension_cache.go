package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/janpuc/t3-code-operator/internal/render"
)

const extensionCacheVersion = 1

var extensionCacheKeyPattern = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

type extensionCacheMetadata struct {
	Version  int    `json:"version"`
	CacheKey string `json:"cacheKey"`
}

func (manager *CachedExtensionManager) ensureCache(
	ctx context.Context,
	source render.ExtensionSource,
	cacheKey string,
	credential *SecretValue,
) (string, error) {
	path, exists, err := manager.loadCache(source, cacheKey)
	if err != nil || exists {
		return path, err
	}
	fetcher := manager.fetchers[source.Type]
	if fetcher == nil {
		return "", fmt.Errorf("source fetcher %s is unavailable", source.Type)
	}
	if err := manager.ensureCacheRoot(); err != nil {
		return "", err
	}
	digest, err := extensionCacheDigest(cacheKey)
	if err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(manager.cacheRoot, ".stage-"+digest[:12]+"-")
	if err != nil {
		return "", err
	}
	keepStage := false
	defer func() {
		if !keepStage {
			_ = removeExtensionCacheStage(manager.cacheRoot, stage)
		}
	}()
	content := filepath.Join(stage, "content")
	if err := os.Mkdir(content, 0o700); err != nil {
		return "", err
	}
	fetchContext, cancel := context.WithTimeout(ctx, manager.fetchTimeout)
	defer cancel()
	if err := fetcher.Fetch(fetchContext, source, credential, content); err != nil {
		return "", fmt.Errorf("fetch pinned %s source: %w", source.Type, err)
	}
	if err := validateExtensionCacheContent(content); err != nil {
		return "", err
	}
	metadata, err := json.Marshal(extensionCacheMetadata{Version: extensionCacheVersion, CacheKey: cacheKey})
	if err != nil {
		return "", err
	}
	if err := atomicWriteFile(filepath.Join(stage, "metadata.json"), metadata, 0o400); err != nil {
		return "", err
	}
	if err := sealExtensionCache(stage); err != nil {
		return "", err
	}
	entry := filepath.Join(manager.cacheRoot, digest)
	if err := os.Rename(stage, entry); err != nil {
		if _, statErr := os.Lstat(entry); statErr == nil {
			return manager.requireCache(source, cacheKey)
		}
		return "", err
	}
	keepStage = true
	return filepath.Join(entry, "content"), nil
}

func (manager *CachedExtensionManager) loadCache(
	source render.ExtensionSource,
	cacheKey string,
) (string, bool, error) {
	expected, err := render.ExtensionCacheKey(source)
	if err != nil {
		return "", false, err
	}
	if expected != cacheKey {
		return "", false, errors.New("Extension cache key does not match its source")
	}
	digest, err := extensionCacheDigest(cacheKey)
	if err != nil {
		return "", false, err
	}
	entry := filepath.Join(manager.cacheRoot, digest)
	info, err := os.Lstat(entry)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("Extension cache entry is not a directory")
	}
	path, err := manager.requireCache(source, cacheKey)
	return path, err == nil, err
}

func (manager *CachedExtensionManager) requireCache(
	source render.ExtensionSource,
	cacheKey string,
) (string, error) {
	expected, err := render.ExtensionCacheKey(source)
	if err != nil {
		return "", err
	}
	if expected != cacheKey {
		return "", errors.New("Extension cache key does not match its source")
	}
	digest, err := extensionCacheDigest(cacheKey)
	if err != nil {
		return "", err
	}
	entry := filepath.Join(manager.cacheRoot, digest)
	if err := rejectSymlinkComponents(manager.dataRoot, manager.cacheRoot); err != nil {
		return "", fmt.Errorf("Extension cache root: %w", err)
	}
	if err := rejectSymlinkComponents(manager.cacheRoot, entry); err != nil {
		return "", err
	}
	metadataPath := filepath.Join(entry, "metadata.json")
	metadataInfo, err := os.Lstat(metadataPath)
	if err != nil {
		return "", fmt.Errorf("read Extension cache metadata: %w", err)
	}
	if !metadataInfo.Mode().IsRegular() {
		return "", errors.New("Extension cache metadata is not a regular file")
	}
	raw, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", err
	}
	var metadata extensionCacheMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return "", fmt.Errorf("parse Extension cache metadata: %w", err)
	}
	if metadata.Version != extensionCacheVersion || metadata.CacheKey != cacheKey {
		return "", errors.New("Extension cache metadata does not match its source")
	}
	content := filepath.Join(entry, "content")
	if err := validateExtensionCacheContent(content); err != nil {
		return "", err
	}
	return content, nil
}

func (manager *CachedExtensionManager) ensureCacheRoot() error {
	if err := rejectSymlinkComponents(manager.dataRoot, manager.cacheRoot); err != nil {
		return fmt.Errorf("Extension cache root: %w", err)
	}
	if err := os.MkdirAll(manager.cacheRoot, 0o700); err != nil {
		return err
	}
	if err := rejectSymlinkComponents(manager.dataRoot, manager.cacheRoot); err != nil {
		return fmt.Errorf("Extension cache root: %w", err)
	}
	return nil
}

func extensionCacheDigest(cacheKey string) (string, error) {
	match := extensionCacheKeyPattern.FindStringSubmatch(cacheKey)
	if match == nil {
		return "", errors.New("Extension cache key must be a lowercase SHA-256 digest")
	}
	return match[1], nil
}

func validateExtensionCacheContent(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect Extension cache content: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Extension cache content is not a directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() == ".gitmodules" {
			return fmt.Errorf("Extension source contains unsupported git submodules at %s", path)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				return fmt.Errorf("Extension source link %s has an absolute target", path)
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("resolve Extension source link %s: %w", path, err)
			}
			if !isPathWithin(resolvedRoot, resolved) {
				return fmt.Errorf("Extension source link %s escapes the source root", path)
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		prefix := make([]byte, 200)
		count, readErr := io.ReadFull(file, prefix)
		closeErr := file.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if bytes.HasPrefix(prefix[:count], []byte("version https://git-lfs.github.com/spec/v1")) {
			return fmt.Errorf("Extension source contains an unsupported Git LFS pointer at %s", path)
		}
		return nil
	})
}

func sealExtensionCache(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() &^ 0o222
		if info.IsDir() {
			mode = 0o500
		} else {
			mode |= 0o400
		}
		return os.Chmod(path, mode)
	})
}

func removeExtensionCacheStage(cacheRoot, stage string) error {
	relative, err := filepath.Rel(cacheRoot, stage)
	if err != nil || strings.Contains(relative, string(filepath.Separator)) || !strings.HasPrefix(relative, ".stage-") {
		return errors.New("refuse to remove an invalid Extension cache stage")
	}
	makeExtensionTreeWritable(stage)
	return os.RemoveAll(stage)
}

func makeExtensionTreeWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && entry.Type()&os.ModeSymlink == 0 {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}
