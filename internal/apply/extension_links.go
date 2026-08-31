package apply

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/janpuc/t3-code-operator/internal/render"
)

type extensionLinkState struct {
	exists bool
	target string
}

type extensionLinkAction struct {
	logicalPath   string
	physicalPath  string
	desiredTarget string
	remove        bool
	original      extensionLinkState
	createdDirs   []string
}

type extensionLinkTransaction struct {
	actions   []extensionLinkAction
	committed int
}

type extensionLinkTarget struct {
	activation  render.ExtensionActivation
	destination render.ExtensionDestination
	cachePath   string
}

func (manager *CachedExtensionManager) stageExtensionLinks(
	previous []render.ExtensionActivation,
	desired []render.ExtensionActivation,
	cachePaths map[string]string,
	recovering bool,
) (*extensionLinkTransaction, error) {
	previousTargets, err := manager.extensionLinkTargets(previous, cachePaths, false)
	if err != nil {
		return nil, fmt.Errorf("stage previous Extension destinations: %w", err)
	}
	desiredTargets, err := manager.extensionLinkTargets(desired, cachePaths, true)
	if err != nil {
		return nil, fmt.Errorf("stage desired Extension destinations: %w", err)
	}
	paths := make(map[string]struct{}, len(previousTargets)+len(desiredTargets))
	for path := range previousTargets {
		paths[path] = struct{}{}
	}
	for path := range desiredTargets {
		paths[path] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)

	transaction := &extensionLinkTransaction{}
	for _, logicalPath := range ordered {
		previousTarget, wasOwned := previousTargets[logicalPath]
		desiredTarget, isDesired := desiredTargets[logicalPath]
		identity := desiredTarget.activation
		if !isDesired {
			identity = previousTarget.activation
		}
		physicalPath, err := physicalHarnessPath(manager.dataRoot, logicalPath, identity.InstanceID)
		if err != nil {
			return nil, err
		}
		original, err := readExtensionLink(physicalPath)
		if err != nil {
			return nil, fmt.Errorf("inspect Extension destination %s: %w", logicalPath, err)
		}
		if original.exists {
			matchesPrevious, err := extensionLinkMatchesTarget(physicalPath, original, previousTarget, wasOwned)
			if err != nil {
				return nil, fmt.Errorf("verify prior Extension destination %s: %w", logicalPath, err)
			}
			matchesDesired := false
			if recovering {
				matchesDesired, err = extensionLinkMatchesTarget(physicalPath, original, desiredTarget, isDesired)
				if err != nil {
					return nil, fmt.Errorf("verify recovery Extension destination %s: %w", logicalPath, err)
				}
			}
			if !matchesPrevious && !matchesDesired {
				return nil, fmt.Errorf("Extension destination %s drifted from its managed target", logicalPath)
			}
		}

		action := extensionLinkAction{
			logicalPath:  logicalPath,
			physicalPath: physicalPath,
			original:     original,
			remove:       !isDesired,
		}
		if isDesired {
			sourcePath, err := extensionSourcePath(desiredTarget.cachePath, desiredTarget.destination.SourcePath)
			if err != nil {
				return nil, fmt.Errorf("resolve Extension destination %s: %w", logicalPath, err)
			}
			relativeTarget, err := filepath.Rel(filepath.Dir(physicalPath), sourcePath)
			if err != nil {
				return nil, err
			}
			action.desiredTarget = relativeTarget
			if original.exists && extensionLinkResolvesTo(physicalPath, original.target, sourcePath) {
				continue
			}
		} else if !original.exists {
			continue
		}
		transaction.actions = append(transaction.actions, action)
	}
	return transaction, nil
}

func extensionLinkMatchesTarget(
	linkPath string,
	link extensionLinkState,
	target extensionLinkTarget,
	present bool,
) (bool, error) {
	if !link.exists || !present {
		return false, nil
	}
	sourcePath, err := extensionSourcePath(target.cachePath, target.destination.SourcePath)
	if err != nil {
		return false, err
	}
	return extensionLinkResolvesTo(linkPath, link.target, sourcePath), nil
}

func (manager *CachedExtensionManager) extensionLinkTargets(
	activations []render.ExtensionActivation,
	cachePaths map[string]string,
	requireSource bool,
) (map[string]extensionLinkTarget, error) {
	result := make(map[string]extensionLinkTarget)
	for _, activation := range activations {
		cachePath := cachePaths[activation.CacheKey]
		if len(activation.Destinations) != 0 && cachePath == "" {
			return nil, fmt.Errorf("Extension %s/%s has no cache path", activation.InstanceID, activation.Name)
		}
		for _, destination := range activation.Destinations {
			if _, exists := result[destination.Path]; exists {
				return nil, fmt.Errorf("Extension destination %s appears more than once", destination.Path)
			}
			if _, err := physicalHarnessPath(manager.dataRoot, destination.Path, activation.InstanceID); err != nil {
				return nil, err
			}
			if requireSource {
				if _, err := extensionSourcePath(cachePath, destination.SourcePath); err != nil {
					return nil, err
				}
			}
			result[destination.Path] = extensionLinkTarget{
				activation:  activation,
				destination: destination,
				cachePath:   cachePath,
			}
		}
	}
	return result, nil
}

func extensionSourcePath(cachePath, sourcePath string) (string, error) {
	if sourcePath == "" || filepath.IsAbs(sourcePath) {
		return "", errors.New("Extension source path must be relative")
	}
	native := filepath.FromSlash(sourcePath)
	clean := filepath.Clean(native)
	if clean != native || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("Extension source path escapes its cache entry")
	}
	path := filepath.Join(cachePath, clean)
	if !isPathWithin(cachePath, path) {
		return "", errors.New("Extension source path escapes its cache entry")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve source path %s: %w", sourcePath, err)
	}
	resolvedCache, err := filepath.EvalSymlinks(cachePath)
	if err != nil {
		return "", err
	}
	if !isPathWithin(resolvedCache, resolved) {
		return "", errors.New("Extension source path escapes its cache entry")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("Extension source path is not a directory")
	}
	return resolved, nil
}

func readExtensionLink(path string) (extensionLinkState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return extensionLinkState{}, nil
	}
	if err != nil {
		return extensionLinkState{}, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return extensionLinkState{}, errors.New("target is not a symbolic link")
	}
	target, err := os.Readlink(path)
	if err != nil {
		return extensionLinkState{}, err
	}
	return extensionLinkState{exists: true, target: target}, nil
}

func extensionLinkResolvesTo(path, linkTarget, desired string) bool {
	resolved := linkTarget
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(path), resolved)
	}
	return filepath.Clean(resolved) == filepath.Clean(desired)
}

func (transaction *extensionLinkTransaction) Commit() error {
	for index := range transaction.actions {
		action := &transaction.actions[index]
		current, err := readExtensionLink(action.physicalPath)
		if err != nil {
			return fmt.Errorf("recheck Extension destination %s: %w", action.logicalPath, err)
		}
		if current != action.original {
			return fmt.Errorf("Extension destination %s changed during staging", action.logicalPath)
		}
		if action.remove {
			if err := os.Remove(action.physicalPath); err != nil {
				return fmt.Errorf("remove Extension destination %s: %w", action.logicalPath, err)
			}
		} else {
			created, err := ensureExtensionLinkParent(action.physicalPath)
			if err != nil {
				return fmt.Errorf("create Extension destination parent %s: %w", action.logicalPath, err)
			}
			action.createdDirs = created
			if err := atomicReplaceSymlink(action.physicalPath, action.desiredTarget); err != nil {
				return fmt.Errorf("activate Extension destination %s: %w", action.logicalPath, err)
			}
		}
		transaction.committed = index + 1
	}
	return nil
}

func (transaction *extensionLinkTransaction) Rollback() error {
	var result error
	for index := transaction.committed - 1; index >= 0; index-- {
		action := &transaction.actions[index]
		current, err := readExtensionLink(action.physicalPath)
		if err != nil {
			result = errors.Join(result, fmt.Errorf("inspect Extension destination %s during rollback: %w", action.logicalPath, err))
			continue
		}
		if action.original.exists {
			if err := atomicReplaceSymlink(action.physicalPath, action.original.target); err != nil {
				result = errors.Join(result, fmt.Errorf("restore Extension destination %s: %w", action.logicalPath, err))
			}
		} else if current.exists {
			if err := os.Remove(action.physicalPath); err != nil {
				result = errors.Join(result, fmt.Errorf("remove Extension destination %s during rollback: %w", action.logicalPath, err))
			}
		}
		for directoryIndex := len(action.createdDirs) - 1; directoryIndex >= 0; directoryIndex-- {
			if err := os.Remove(action.createdDirs[directoryIndex]); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrExist) {
				result = errors.Join(result, err)
			}
		}
	}
	transaction.committed = 0
	return result
}

func (transaction *extensionLinkTransaction) Finalize() error {
	return nil
}

func ensureExtensionLinkParent(path string) ([]string, error) {
	parent := filepath.Dir(path)
	missing := make([]string, 0)
	current := parent
	for {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, current)
		next := filepath.Dir(current)
		if next == current {
			return nil, errors.New("cannot find an existing Extension destination parent")
		}
		current = next
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	return missing, nil
}

func atomicReplaceSymlink(path, target string) (result error) {
	directory := filepath.Dir(path)
	placeholder, err := os.CreateTemp(directory, ".t3-coded-link-*")
	if err != nil {
		return err
	}
	temporaryPath := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		return err
	}
	defer func() {
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, removeErr)
		}
	}()
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	if err := os.Symlink(target, temporaryPath); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func physicalHarnessPath(dataRoot, logicalPath, instanceID string) (string, error) {
	clean := filepath.Clean(logicalPath)
	prefix := "/data/harnesses/" + instanceID + "/"
	if clean != logicalPath || !strings.HasPrefix(logicalPath, prefix) {
		return "", fmt.Errorf("managed path %s escapes provider instance %s", logicalPath, instanceID)
	}
	relative := strings.TrimPrefix(logicalPath, "/data/")
	physical := filepath.Join(dataRoot, filepath.FromSlash(relative))
	if !isPathWithin(dataRoot, physical) || physical == dataRoot {
		return "", fmt.Errorf("managed path %s escapes the data root", logicalPath)
	}
	if err := rejectSymlinkComponents(dataRoot, filepath.Dir(physical)); err != nil {
		return "", fmt.Errorf("managed path %s: %w", logicalPath, err)
	}
	return physical, nil
}
