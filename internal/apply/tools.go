package apply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/janpuc/t3-code-operator/internal/render"
)

const maxToolRevisionMetadataBytes = 1 << 20

var toolExecutablePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+@-]{0,254}$`)

type MiseExecutable struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Symlink bool   `json:"symlink"`
}

type ToolRuntime interface {
	Prepare(context.Context, string, string) ([]MiseExecutable, error)
}

type MiseToolManagerConfig struct {
	DataRoot string
	Runtime  ToolRuntime
}

type MiseToolManager struct {
	root          string
	revisionsRoot string
	stagingRoot   string
	toolsetsRoot  string
	runtime       ToolRuntime
}

type toolRevisionMetadata struct {
	Tools []render.ToolActivation `json:"tools"`
}

type toolCurrentState struct {
	exists bool
	target string
}

type toolLinkTransaction struct {
	currentPath string
	original    toolCurrentState
	desired     string
	committed   bool
}

func NewMiseToolManager(config MiseToolManagerConfig) (*MiseToolManager, error) {
	if config.DataRoot == "" || !filepath.IsAbs(config.DataRoot) {
		return nil, errors.New("data root must be an absolute path")
	}
	if config.Runtime == nil {
		return nil, errors.New("tool runtime is required")
	}
	root := filepath.Join(filepath.Clean(config.DataRoot), "t3-coded", "tools")
	manager := &MiseToolManager{
		root:          root,
		revisionsRoot: filepath.Join(root, "revisions"),
		stagingRoot:   filepath.Join(root, "transactions"),
		toolsetsRoot:  filepath.Join(filepath.Clean(config.DataRoot), "t3-coded", "mise", "toolsets"),
		runtime:       config.Runtime,
	}
	for _, directory := range []string{manager.root, manager.revisionsRoot, manager.stagingRoot, manager.toolsetsRoot} {
		if err := rejectSymlinkComponents(filepath.Clean(config.DataRoot), directory); err != nil {
			return nil, fmt.Errorf("validate tool directory: %w", err)
		}
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create tool directory: %w", err)
		}
		if err := rejectSymlinkComponents(filepath.Clean(config.DataRoot), directory); err != nil {
			return nil, fmt.Errorf("validate created tool directory: %w", err)
		}
	}
	return manager, nil
}

func (manager *MiseToolManager) Stage(
	ctx context.Context,
	previous []render.ToolActivation,
	desired []render.ToolActivation,
) (ToolTransaction, error) {
	return manager.stage(ctx, previous, desired, false)
}

func (manager *MiseToolManager) StageRecovery(
	ctx context.Context,
	interrupted []render.ToolActivation,
	desired []render.ToolActivation,
) (ToolTransaction, error) {
	return manager.stage(ctx, interrupted, desired, true)
}

func (manager *MiseToolManager) stage(
	ctx context.Context,
	previous []render.ToolActivation,
	desired []render.ToolActivation,
	recovering bool,
) (ToolTransaction, error) {
	previous = normalizedToolActivations(previous)
	desired = normalizedToolActivations(desired)
	if err := render.ValidateToolActivations(previous); err != nil {
		return nil, fmt.Errorf("validate previous tools: %w", err)
	}
	if err := render.ValidateToolActivations(desired); err != nil {
		return nil, fmt.Errorf("validate desired tools: %w", err)
	}

	currentPath := filepath.Join(manager.root, "current")
	current, err := manager.readCurrent(currentPath)
	if err != nil {
		return nil, err
	}
	previousTarget, err := manager.revisionTarget(previous)
	if err != nil {
		return nil, err
	}
	if !recovering && !currentMatchesExpected(current, previousTarget, len(previous) == 0) {
		return nil, errors.New("active tool revision does not match the last-known-good manifest")
	}

	if len(previous) == 0 && len(desired) == 0 {
		return noOpToolTransaction{}, nil
	}
	desiredTarget, err := manager.ensureRevision(ctx, desired)
	if err != nil {
		return nil, toolFailureFor(err, desired)
	}
	if current.exists && current.target == desiredTarget {
		return noOpToolTransaction{}, nil
	}
	return &toolLinkTransaction{
		currentPath: currentPath,
		original:    current,
		desired:     desiredTarget,
	}, nil
}

func currentMatchesExpected(current toolCurrentState, expected string, allowAbsent bool) bool {
	return (!current.exists && allowAbsent) || (current.exists && current.target == expected)
}

func (manager *MiseToolManager) ensureRevision(
	ctx context.Context,
	tools []render.ToolActivation,
) (string, error) {
	target, err := manager.revisionTarget(tools)
	if err != nil {
		return "", err
	}
	revisionPath := filepath.Join(manager.root, filepath.FromSlash(target))
	if matches, err := manager.revisionMatches(revisionPath, tools); err != nil {
		return "", err
	} else if matches {
		return target, nil
	}

	stage, err := os.MkdirTemp(manager.stagingRoot, ".tool-revision-")
	if err != nil {
		return "", fmt.Errorf("create tool staging directory: %w", err)
	}
	removeStage := true
	defer func() {
		if removeStage {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0o700); err != nil {
		return "", err
	}
	toolsetID := filepath.Base(revisionPath)
	toolDataRoot := filepath.Join(manager.toolsetsRoot, toolsetID)
	dataComplete, err := manager.toolDataMatches(toolDataRoot, tools)
	if err != nil {
		return "", err
	}
	if !dataComplete {
		if err := manager.resetIncompleteToolData(toolDataRoot); err != nil {
			return "", err
		}
	}
	configuration, lockfile, err := renderMiseFiles(tools)
	if err != nil {
		return "", err
	}
	if err := atomicWriteFile(filepath.Join(stage, "mise.toml"), configuration, 0o600); err != nil {
		return "", fmt.Errorf("write tool mise configuration: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(stage, "mise.lock"), lockfile, 0o600); err != nil {
		return "", fmt.Errorf("write tool mise lock: %w", err)
	}

	executables, err := manager.runtime.Prepare(ctx, stage, toolDataRoot)
	if err != nil {
		if !dataComplete {
			err = errors.Join(err, joinError("clean incomplete tool install", manager.resetIncompleteToolData(toolDataRoot)))
		}
		return "", fmt.Errorf("prepare tools with mise: %w", err)
	}
	if err := manager.writeExecutableLinks(stage, filepath.Join(toolDataRoot, "installs"), tools, executables); err != nil {
		if !dataComplete {
			err = errors.Join(err, joinError("clean invalid tool install", manager.resetIncompleteToolData(toolDataRoot)))
		}
		return "", err
	}
	if !dataComplete {
		toolDataMetadata, err := json.Marshal(toolRevisionMetadata{Tools: tools})
		if err != nil {
			return "", err
		}
		if err := atomicWriteFile(filepath.Join(toolDataRoot, "complete.json"), toolDataMetadata, 0o600); err != nil {
			return "", fmt.Errorf("record complete tool install: %w", err)
		}
	}
	metadata, err := json.Marshal(toolRevisionMetadata{Tools: tools})
	if err != nil {
		return "", err
	}
	if err := atomicWriteFile(filepath.Join(stage, "metadata.json"), metadata, 0o600); err != nil {
		return "", fmt.Errorf("write tool revision metadata: %w", err)
	}

	if err := os.Rename(stage, revisionPath); err != nil {
		if matches, matchErr := manager.revisionMatches(revisionPath, tools); matchErr == nil && matches {
			return target, nil
		}
		return "", fmt.Errorf("publish tool revision: %w", err)
	}
	removeStage = false
	return target, nil
}

func (manager *MiseToolManager) revisionTarget(tools []render.ToolActivation) (string, error) {
	raw, err := json.Marshal(toolRevisionMetadata{Tools: normalizedToolActivations(tools)})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return filepath.ToSlash(filepath.Join("revisions", hex.EncodeToString(digest[:]))), nil
}

func (manager *MiseToolManager) revisionMatches(path string, tools []render.ToolActivation) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect tool revision: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("tool revision path is not a directory")
	}
	raw, err := readBoundedToolMetadata(filepath.Join(path, "metadata.json"))
	if err != nil {
		return false, fmt.Errorf("read tool revision metadata: %w", err)
	}
	var metadata toolRevisionMetadata
	if err := decodeToolMetadata(raw, &metadata); err != nil {
		return false, fmt.Errorf("parse tool revision metadata: %w", err)
	}
	return reflect.DeepEqual(normalizedToolActivations(metadata.Tools), normalizedToolActivations(tools)), nil
}

func readBoundedToolMetadata(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("tool metadata is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxToolRevisionMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxToolRevisionMetadataBytes {
		return nil, errors.New("tool metadata exceeds its size limit")
	}
	return raw, nil
}

func decodeToolMetadata(raw []byte, metadata *toolRevisionMetadata) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(metadata); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("tool metadata contains trailing data")
	}
	return nil
}

func (manager *MiseToolManager) writeExecutableLinks(
	stage string,
	installRoot string,
	tools []render.ToolActivation,
	executables []MiseExecutable,
) error {
	binDirectory := filepath.Join(stage, "bin")
	if err := os.Mkdir(binDirectory, 0o700); err != nil {
		return fmt.Errorf("create tool link directory: %w", err)
	}
	byName := make(map[string]string, len(executables))
	for _, executable := range executables {
		if !toolExecutableNameValid(executable.Name) {
			return fmt.Errorf("mise returned invalid executable name %q", executable.Name)
		}
		if _, exists := byName[executable.Name]; exists {
			return fmt.Errorf("mise returned duplicate executable %q", executable.Name)
		}
		resolved, err := manager.validateExecutablePath(installRoot, executable.Path)
		if err != nil {
			return fmt.Errorf("validate mise executable %s: %w", executable.Name, err)
		}
		byName[executable.Name] = resolved
	}
	for _, tool := range tools {
		if _, exists := byName[tool.Name]; !exists {
			return fmt.Errorf("tool %s did not install an executable with the same name", tool.Name)
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.Symlink(byName[name], filepath.Join(binDirectory, name)); err != nil {
			return fmt.Errorf("link tool executable %s: %w", name, err)
		}
	}
	return nil
}

func (manager *MiseToolManager) validateExecutablePath(installRoot, path string) (string, error) {
	if !filepath.IsAbs(path) || !isPathWithin(installRoot, filepath.Clean(path)) {
		return "", errors.New("path is outside the retained mise install root")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !isPathWithin(installRoot, resolved) {
		return "", errors.New("resolved path is outside the retained mise install root")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("path is not an executable regular file")
	}
	return resolved, nil
}

func (manager *MiseToolManager) toolDataMatches(path string, tools []render.ToolActivation) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect tool install data: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("tool install data is not a directory")
	}
	raw, err := readBoundedToolMetadata(filepath.Join(path, "complete.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read tool install metadata: %w", err)
	}
	var metadata toolRevisionMetadata
	if err := decodeToolMetadata(raw, &metadata); err != nil {
		return false, fmt.Errorf("parse tool install metadata: %w", err)
	}
	return reflect.DeepEqual(normalizedToolActivations(metadata.Tools), normalizedToolActivations(tools)), nil
}

func (manager *MiseToolManager) resetIncompleteToolData(path string) error {
	if filepath.Dir(path) != manager.toolsetsRoot || path == manager.toolsetsRoot || !isPathWithin(manager.toolsetsRoot, path) {
		return errors.New("tool install cleanup target is unsafe")
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove incomplete tool install: %w", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return fmt.Errorf("create isolated tool install: %w", err)
	}
	return nil
}

func toolExecutableNameValid(name string) bool {
	return name != "." && name != ".." && toolExecutablePattern.MatchString(name)
}

func (manager *MiseToolManager) readCurrent(path string) (toolCurrentState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return toolCurrentState{}, nil
	}
	if err != nil {
		return toolCurrentState{}, fmt.Errorf("inspect active tool revision: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return toolCurrentState{}, errors.New("active tool revision is not a symbolic link")
	}
	target, err := os.Readlink(path)
	if err != nil {
		return toolCurrentState{}, err
	}
	if filepath.IsAbs(target) || filepath.Clean(target) != target || !strings.HasPrefix(filepath.ToSlash(target), "revisions/") {
		return toolCurrentState{}, errors.New("active tool revision has an unsafe target")
	}
	resolved := filepath.Join(manager.root, target)
	if !isPathWithin(manager.revisionsRoot, resolved) || resolved == manager.revisionsRoot {
		return toolCurrentState{}, errors.New("active tool revision escapes the revisions root")
	}
	return toolCurrentState{exists: true, target: filepath.ToSlash(target)}, nil
}

func normalizedToolActivations(tools []render.ToolActivation) []render.ToolActivation {
	result := make([]render.ToolActivation, len(tools))
	for index, tool := range tools {
		result[index] = tool
		result[index].Options = make(map[string]string, len(tool.Options))
		for key, value := range tool.Options {
			result[index].Options[key] = value
		}
		if len(result[index].Options) == 0 {
			result[index].Options = nil
		}
		result[index].Artifacts = append([]render.ToolArtifact(nil), tool.Artifacts...)
		sort.Slice(result[index].Artifacts, func(i, j int) bool {
			return result[index].Artifacts[i].Platform < result[index].Artifacts[j].Platform
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func toolFailureFor(err error, tools []render.ToolActivation) error {
	if err == nil {
		return nil
	}
	names := make([]string, 0, len(tools))
	message := err.Error()
	for _, tool := range tools {
		if strings.Contains(message, tool.Backend) || strings.Contains(message, tool.Version) {
			names = append(names, tool.Name)
		}
	}
	if len(names) == 0 && len(tools) == 1 {
		names = append(names, tools[0].Name)
	}
	return &ToolFailure{Names: names, Err: err}
}

func (transaction *toolLinkTransaction) Commit() error {
	current, err := readToolCurrentWithoutManager(transaction.currentPath)
	if err != nil {
		return err
	}
	if current != transaction.original {
		return errors.New("active tool revision changed during staging")
	}
	if err := atomicReplaceSymlink(transaction.currentPath, transaction.desired); err != nil {
		return fmt.Errorf("activate tool revision: %w", err)
	}
	transaction.committed = true
	return nil
}

func (transaction *toolLinkTransaction) Rollback() error {
	if !transaction.committed {
		return nil
	}
	if transaction.original.exists {
		if err := atomicReplaceSymlink(transaction.currentPath, transaction.original.target); err != nil {
			return fmt.Errorf("restore active tool revision: %w", err)
		}
	} else if err := os.Remove(transaction.currentPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove active tool revision: %w", err)
	}
	transaction.committed = false
	return nil
}

func (transaction *toolLinkTransaction) Finalize() error {
	return nil
}

func readToolCurrentWithoutManager(path string) (toolCurrentState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return toolCurrentState{}, nil
	}
	if err != nil {
		return toolCurrentState{}, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return toolCurrentState{}, errors.New("active tool revision is not a symbolic link")
	}
	target, err := os.Readlink(path)
	if err != nil {
		return toolCurrentState{}, err
	}
	return toolCurrentState{exists: true, target: filepath.ToSlash(target)}, nil
}

type noOpToolTransaction struct{}

func (noOpToolTransaction) Commit() error   { return nil }
func (noOpToolTransaction) Rollback() error { return nil }
func (noOpToolTransaction) Finalize() error { return nil }
