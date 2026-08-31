package apply

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/janpuc/t3-code-operator/internal/render"
	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
	"sigs.k8s.io/yaml"
)

type stagedFile struct {
	logicalPath  string
	physicalPath string
	scope        render.FileScope
	instanceID   string
	original     []byte
	originalMode fs.FileMode
	desiredMode  fs.FileMode
	existed      bool
	desired      []byte
	delete       bool
	changed      bool
}

type fileTransaction struct {
	files     []stagedFile
	committed int
}

func (applier *Applier) stageFiles(
	manifest render.Manifest,
	materializedTargets ...[]render.FileTarget,
) (*fileTransaction, error) {
	desiredTargets := manifest.Files
	if len(materializedTargets) > 1 {
		return nil, errors.New("only one materialized file target set is allowed")
	}
	if len(materializedTargets) == 1 {
		desiredTargets = materializedTargets[0]
	}
	previousFiles := make(map[string]render.FileTarget)
	if applier.liveManifest != nil {
		for _, target := range applier.liveManifest.Files {
			previousFiles[target.Path] = target
		}
	}
	desiredFiles := make(map[string]render.FileTarget, len(desiredTargets))
	for _, target := range desiredTargets {
		if _, exists := desiredFiles[target.Path]; exists {
			return nil, fmt.Errorf("file target %s appears more than once", target.Path)
		}
		if normalizedFileScope(target.Scope) == render.FileScopeHarness {
			if _, exists := manifest.ProviderInstances[target.InstanceID]; !exists {
				return nil, fmt.Errorf("file target %s refers to unknown provider instance %s", target.Path, target.InstanceID)
			}
		}
		if err := validateFileTarget(target); err != nil {
			return nil, err
		}
		desiredFiles[target.Path] = target
	}

	paths := make(map[string]struct{}, len(previousFiles)+len(desiredFiles))
	for path := range previousFiles {
		paths[path] = struct{}{}
	}
	for path := range desiredFiles {
		paths[path] = struct{}{}
	}
	orderedPaths := make([]string, 0, len(paths))
	for path := range paths {
		orderedPaths = append(orderedPaths, path)
	}
	sort.Strings(orderedPaths)

	transaction := &fileTransaction{files: make([]stagedFile, 0, len(orderedPaths))}
	for _, logicalPath := range orderedPaths {
		previous, hadPrevious := previousFiles[logicalPath]
		desired, hasDesired := desiredFiles[logicalPath]
		identity := desired
		if !hasDesired {
			identity = previous
		}
		physicalPath, err := applier.physicalManagedPath(logicalPath, identity.Scope, identity.InstanceID)
		if err != nil {
			return nil, err
		}
		current, mode, exists, err := readManagedFile(physicalPath)
		if err != nil {
			return nil, fmt.Errorf("read managed file %s: %w", logicalPath, err)
		}
		staged := stagedFile{
			logicalPath:  logicalPath,
			physicalPath: physicalPath,
			scope:        normalizedFileScope(identity.Scope),
			instanceID:   identity.InstanceID,
			original:     current,
			originalMode: mode,
			desiredMode:  mode,
			existed:      exists,
		}
		if !exists {
			staged.desiredMode = 0o600
		}

		switch {
		case hasDesired && desired.Mode == render.WriteModeReplace:
			staged.desired = []byte(desired.Content)
			staged.changed = !exists || !bytes.Equal(current, staged.desired)
		case hasDesired && desired.Mode == render.WriteModeSeedIfAbsent:
			if !exists || !managedFileContentIsParseable(desired.Format, current) {
				staged.desired = []byte(desired.Content)
				staged.desiredMode = 0o600
				staged.changed = true
			}
		case hasDesired && desired.Mode == render.WriteModeMerge:
			var previousTarget *render.FileTarget
			if hadPrevious {
				copy := previous
				previousTarget = &copy
			}
			merged, err := mergeManagedFile(current, exists, previousTarget, &desired)
			if err != nil {
				return nil, fmt.Errorf("merge managed file %s: %w", logicalPath, err)
			}
			staged.desired = merged
			staged.changed = !exists || !bytes.Equal(current, merged)
		case hasDesired && desired.Mode == render.WriteModeManagedBlock:
			merged, err := mergeManagedBlock(current, exists, desired.Content)
			if err != nil {
				return nil, fmt.Errorf("merge managed block in %s: %w", logicalPath, err)
			}
			staged.desired = merged
			staged.changed = !exists || !bytes.Equal(current, merged)
		case !hasDesired && previous.Mode == render.WriteModeReplace:
			staged.delete = exists
			staged.changed = exists
		case !hasDesired && previous.Mode == render.WriteModeSeedIfAbsent:
			staged.changed = false
		case !hasDesired && previous.Mode == render.WriteModeMerge:
			if exists {
				merged, err := mergeManagedFile(current, true, &previous, nil)
				if err != nil {
					return nil, fmt.Errorf("remove managed fields from %s: %w", logicalPath, err)
				}
				staged.desired = merged
				staged.changed = !bytes.Equal(current, merged)
			}
		case !hasDesired && previous.Mode == render.WriteModeManagedBlock:
			if exists {
				merged, err := removeManagedBlock(current)
				if err != nil {
					return nil, fmt.Errorf("remove managed block from %s: %w", logicalPath, err)
				}
				staged.desired = merged
				staged.changed = !bytes.Equal(current, merged)
			}
		}
		if hasDesired && desired.Mode != render.WriteModeSeedIfAbsent {
			staged.desiredMode = 0o600
			if exists && staged.originalMode.Perm() != staged.desiredMode {
				staged.changed = true
			}
		}
		if staged.changed {
			transaction.files = append(transaction.files, staged)
		}
	}
	return transaction, nil
}

func validateFileTarget(target render.FileTarget) error {
	if target.Path == "" {
		return errors.New("file target path is required")
	}
	scope := normalizedFileScope(target.Scope)
	switch scope {
	case render.FileScopeHarness:
		if target.InstanceID == "" {
			return errors.New("harness file target instance ID is required")
		}
	case render.FileScopeWorkstation:
		if target.InstanceID != "" {
			return fmt.Errorf("Workstation file target %s must not have an instance ID", target.Path)
		}
		contract, exists := workstationFileContracts[target.Path]
		if !exists || contract.mode != target.Mode || contract.format != target.Format {
			return fmt.Errorf("Workstation file target %s is not allowed", target.Path)
		}
	default:
		return fmt.Errorf("file target %s has unknown scope %q", target.Path, target.Scope)
	}
	switch target.Mode {
	case render.WriteModeReplace, render.WriteModeSeedIfAbsent:
	case render.WriteModeMerge:
		if len(target.OwnedPaths) == 0 {
			return fmt.Errorf("Merge target %s has no owned paths", target.Path)
		}
	case render.WriteModeManagedBlock:
		if target.Format != render.FileFormatText {
			return fmt.Errorf("ManagedBlock target %s must use Text format", target.Path)
		}
	default:
		return fmt.Errorf("file target %s has unknown write mode %q", target.Path, target.Mode)
	}
	switch target.Format {
	case render.FileFormatJSON, render.FileFormatTOML, render.FileFormatYAML, render.FileFormatText:
	default:
		return fmt.Errorf("file target %s has unknown format %q", target.Path, target.Format)
	}
	if target.SecretContent == nil && len(target.Values) == 0 &&
		!managedFileContentIsParseable(target.Format, []byte(target.Content)) {
		return fmt.Errorf("file target %s has invalid %s content", target.Path, target.Format)
	}
	return nil
}

func managedFileContentIsParseable(format render.FileFormat, content []byte) bool {
	if format == render.FileFormatText {
		return true
	}
	_, err := decodeManagedObject(format, content)
	return err == nil
}

type workstationFileContract struct {
	mode   render.WriteMode
	format render.FileFormat
}

var workstationFileContracts = map[string]workstationFileContract{
	"/data/t3-coded/machine-info":     {mode: render.WriteModeReplace, format: render.FileFormatText},
	"/data/t3-coded/gh/config.yml":    {mode: render.WriteModeReplace, format: render.FileFormatYAML},
	"/data/t3-coded/gh/hosts.yml":     {mode: render.WriteModeReplace, format: render.FileFormatYAML},
	"/data/home/.gitconfig":           {mode: render.WriteModeManagedBlock, format: render.FileFormatText},
	"/data/home/.ssh/id_signing":      {mode: render.WriteModeReplace, format: render.FileFormatText},
	"/data/home/.ssh/id_signing.pub":  {mode: render.WriteModeReplace, format: render.FileFormatText},
	"/data/home/.ssh/allowed_signers": {mode: render.WriteModeReplace, format: render.FileFormatText},
}

func normalizedFileScope(scope render.FileScope) render.FileScope {
	if scope == "" {
		return render.FileScopeHarness
	}
	return scope
}

func (applier *Applier) physicalHarnessPath(logicalPath, instanceID string) (string, error) {
	return physicalHarnessPath(applier.dataRoot, logicalPath, instanceID)
}

func (applier *Applier) physicalManagedPath(
	logicalPath string,
	scope render.FileScope,
	instanceID string,
) (string, error) {
	if normalizedFileScope(scope) == render.FileScopeHarness {
		return applier.physicalHarnessPath(logicalPath, instanceID)
	}
	if _, exists := workstationFileContracts[logicalPath]; !exists {
		return "", fmt.Errorf("Workstation file target %s is not allowed", logicalPath)
	}
	const dataPrefix = "/data/"
	if !strings.HasPrefix(logicalPath, dataPrefix) {
		return "", fmt.Errorf("Workstation file target %s is outside the data root", logicalPath)
	}
	physicalPath := filepath.Join(applier.dataRoot, filepath.FromSlash(strings.TrimPrefix(logicalPath, dataPrefix)))
	if !isPathWithin(applier.dataRoot, physicalPath) || physicalPath == applier.dataRoot {
		return "", fmt.Errorf("Workstation file target %s escapes the data root", logicalPath)
	}
	if err := rejectSymlinkComponents(applier.dataRoot, filepath.Dir(physicalPath)); err != nil {
		return "", err
	}
	return physicalPath, nil
}

const (
	managedBlockStart = "# t3-code-operator:managed:start\n"
	managedBlockEnd   = "# t3-code-operator:managed:end\n"
)

func mergeManagedBlock(current []byte, exists bool, content string) ([]byte, error) {
	start, end, found, err := managedBlockBounds(current)
	if err != nil {
		return nil, err
	}
	block := append([]byte(nil), managedBlockStart...)
	block = append(block, content...)
	if len(content) != 0 && content[len(content)-1] != '\n' {
		block = append(block, '\n')
	}
	block = append(block, managedBlockEnd...)
	if found {
		result := append([]byte(nil), current[:start]...)
		result = append(result, block...)
		result = append(result, current[end:]...)
		return result, nil
	}
	result := append([]byte(nil), current...)
	if !exists {
		result = nil
	}
	if len(result) != 0 && result[len(result)-1] != '\n' {
		result = append(result, '\n')
	}
	if len(result) != 0 && !bytes.HasSuffix(result, []byte("\n\n")) {
		result = append(result, '\n')
	}
	result = append(result, block...)
	return result, nil
}

func removeManagedBlock(current []byte) ([]byte, error) {
	start, end, found, err := managedBlockBounds(current)
	if err != nil {
		return nil, err
	}
	if !found {
		return append([]byte(nil), current...), nil
	}
	return append(append([]byte(nil), current[:start]...), current[end:]...), nil
}

func managedBlockBounds(current []byte) (int, int, bool, error) {
	text := string(current)
	if strings.Count(text, managedBlockStart) != strings.Count(text, managedBlockEnd) {
		return 0, 0, false, errors.New("managed block markers are incomplete")
	}
	if strings.Count(text, managedBlockStart) > 1 {
		return 0, 0, false, errors.New("managed block markers occur more than once")
	}
	start := strings.Index(text, managedBlockStart)
	if start < 0 {
		return 0, 0, false, nil
	}
	endOffset := strings.Index(text[start+len(managedBlockStart):], managedBlockEnd)
	if endOffset < 0 {
		return 0, 0, false, errors.New("managed block end marker is missing")
	}
	end := start + len(managedBlockStart) + endOffset + len(managedBlockEnd)
	return start, end, true, nil
}

func rejectSymlinkComponents(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symbolic links are not allowed in managed paths")
		}
	}
	return nil
}

func readManagedFile(path string) ([]byte, fs.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, errors.New("target is not a regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, err
	}
	return raw, info.Mode().Perm(), true, nil
}

func mergeManagedFile(
	current []byte,
	exists bool,
	previous *render.FileTarget,
	desired *render.FileTarget,
) ([]byte, error) {
	format := render.FileFormatJSON
	if desired != nil {
		format = desired.Format
	} else if previous != nil {
		format = previous.Format
	}
	if format == render.FileFormatJSON {
		return mergeManagedJSON(current, exists, previous, desired)
	}
	currentObject := map[string]any{}
	if exists {
		var err error
		currentObject, err = decodeManagedObject(format, current)
		if err != nil {
			return nil, fmt.Errorf("current %s is invalid: %w", format, err)
		}
	}

	previousOwned := map[string]struct{}{}
	if previous != nil {
		for _, ownedPath := range previous.OwnedPaths {
			previousOwned[ownedPath] = struct{}{}
		}
	}
	desiredOwned := map[string]struct{}{}
	var desiredObject map[string]any
	if desired != nil {
		var err error
		desiredObject, err = decodeManagedObject(format, []byte(desired.Content))
		if err != nil {
			return nil, fmt.Errorf("desired %s is invalid: %w", format, err)
		}
		for _, ownedPath := range desired.OwnedPaths {
			desiredOwned[ownedPath] = struct{}{}
		}
	}

	for ownedPath := range previousOwned {
		if _, stillOwned := desiredOwned[ownedPath]; !stillOwned {
			if err := deleteJSONPointer(currentObject, ownedPath); err != nil {
				return nil, err
			}
		}
	}
	if desired != nil {
		for _, ownedPath := range desired.OwnedPaths {
			value, found, err := getJSONPointer(desiredObject, ownedPath)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, fmt.Errorf("desired JSON does not contain owned path %s", ownedPath)
			}
			if err := setJSONPointer(currentObject, ownedPath, value); err != nil {
				return nil, err
			}
		}
	}
	return encodeManagedObject(format, currentObject)
}

func mergeManagedJSON(
	current []byte,
	exists bool,
	previous *render.FileTarget,
	desired *render.FileTarget,
) ([]byte, error) {
	if !exists {
		current = []byte("{}")
	}
	if _, err := decodeManagedObject(render.FileFormatJSON, current); err != nil {
		return nil, fmt.Errorf("current JSON is invalid: %w", err)
	}
	document, err := hujson.Parse(current)
	if err != nil {
		return nil, fmt.Errorf("current JSON is invalid: %w", err)
	}

	previousOwned := map[string]struct{}{}
	if previous != nil {
		for _, ownedPath := range previous.OwnedPaths {
			if _, err := jsonPointerSegments(ownedPath); err != nil {
				return nil, err
			}
			previousOwned[ownedPath] = struct{}{}
		}
	}
	desiredOwned := map[string]struct{}{}
	var desiredObject map[string]any
	if desired != nil {
		desiredObject, err = decodeManagedObject(render.FileFormatJSON, []byte(desired.Content))
		if err != nil {
			return nil, fmt.Errorf("desired JSON is invalid: %w", err)
		}
		for _, ownedPath := range desired.OwnedPaths {
			if _, err := jsonPointerSegments(ownedPath); err != nil {
				return nil, err
			}
			desiredOwned[ownedPath] = struct{}{}
		}
	}

	removed := make([]string, 0)
	for ownedPath := range previousOwned {
		if _, stillOwned := desiredOwned[ownedPath]; !stillOwned {
			removed = append(removed, ownedPath)
		}
	}
	sort.Strings(removed)
	for _, ownedPath := range removed {
		if document.Find(ownedPath) == nil {
			continue
		}
		if err := patchHuJSON(&document, "remove", ownedPath, nil); err != nil {
			return nil, err
		}
	}
	if desired != nil {
		for _, ownedPath := range desired.OwnedPaths {
			value, found, err := getJSONPointer(desiredObject, ownedPath)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, fmt.Errorf("desired JSON does not contain owned path %s", ownedPath)
			}
			operation := "add"
			if document.Find(ownedPath) != nil {
				operation = "replace"
			}
			if err := patchHuJSON(&document, operation, ownedPath, value); err != nil {
				return nil, err
			}
		}
	}
	return document.Pack(), nil
}

func patchHuJSON(document *hujson.Value, operation, path string, value any) error {
	patchOperation := map[string]any{"op": operation, "path": path}
	if operation != "remove" {
		patchOperation["value"] = value
	}
	patch, err := json.Marshal([]any{patchOperation})
	if err != nil {
		return err
	}
	if err := document.Patch(patch); err != nil {
		return fmt.Errorf("apply %s to owned path %s: %w", operation, path, err)
	}
	return nil
}

func decodeManagedObject(format render.FileFormat, raw []byte) (map[string]any, error) {
	object := map[string]any{}
	switch format {
	case render.FileFormatJSON:
		standard, err := hujson.Standardize(append([]byte(nil), raw...))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(standard, &object); err != nil {
			return nil, err
		}
	case render.FileFormatTOML:
		if err := toml.Unmarshal(raw, &object); err != nil {
			return nil, err
		}
	case render.FileFormatYAML:
		if err := yaml.Unmarshal(raw, &object); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("format %s is not supported", format)
	}
	if object == nil {
		return nil, errors.New("document must be an object")
	}
	return object, nil
}

func encodeManagedObject(format render.FileFormat, object map[string]any) ([]byte, error) {
	switch format {
	case render.FileFormatJSON:
		return json.Marshal(object)
	case render.FileFormatTOML:
		return toml.Marshal(object)
	case render.FileFormatYAML:
		return yaml.Marshal(object)
	default:
		return nil, fmt.Errorf("format %s is not supported", format)
	}
}

func getJSONPointer(object map[string]any, pointer string) (any, bool, error) {
	segments, err := jsonPointerSegments(pointer)
	if err != nil {
		return nil, false, err
	}
	var current any = object
	for _, segment := range segments {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		current, ok = mapping[segment]
		if !ok {
			return nil, false, nil
		}
	}
	return current, true, nil
}

func setJSONPointer(object map[string]any, pointer string, value any) error {
	segments, err := jsonPointerSegments(pointer)
	if err != nil {
		return err
	}
	current := object
	for _, segment := range segments[:len(segments)-1] {
		next, ok := current[segment].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[segment] = next
		}
		current = next
	}
	current[segments[len(segments)-1]] = value
	return nil
}

func deleteJSONPointer(object map[string]any, pointer string) error {
	segments, err := jsonPointerSegments(pointer)
	if err != nil {
		return err
	}
	current := object
	for _, segment := range segments[:len(segments)-1] {
		next, ok := current[segment].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	delete(current, segments[len(segments)-1])
	return nil
}

func jsonPointerSegments(pointer string) ([]string, error) {
	if pointer == "" || !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("owned path %q is not a JSON pointer", pointer)
	}
	raw := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	segments := make([]string, len(raw))
	for index, segment := range raw {
		segment = strings.ReplaceAll(segment, "~1", "/")
		segment = strings.ReplaceAll(segment, "~0", "~")
		if segment == "" {
			return nil, fmt.Errorf("owned path %q contains an empty segment", pointer)
		}
		segments[index] = segment
	}
	return segments, nil
}

func (transaction *fileTransaction) Commit() error {
	for index := range transaction.files {
		file := &transaction.files[index]
		var err error
		if file.delete {
			err = os.Remove(file.physicalPath)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		} else {
			err = atomicWriteFile(file.physicalPath, file.desired, file.desiredMode)
		}
		if err != nil {
			rollbackErr := transaction.Rollback()
			return errors.Join(fmt.Errorf("commit %s: %w", file.logicalPath, err), rollbackErr)
		}
		transaction.committed = index + 1
	}
	return nil
}

func (transaction *fileTransaction) Rollback() error {
	var result error
	for index := transaction.committed - 1; index >= 0; index-- {
		file := transaction.files[index]
		var err error
		if file.existed {
			err = atomicWriteFile(file.physicalPath, file.original, file.originalMode)
		} else {
			err = os.Remove(file.physicalPath)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		if err != nil {
			result = errors.Join(result, fmt.Errorf("rollback %s: %w", file.logicalPath, err))
		}
	}
	transaction.committed = 0
	return result
}

func atomicWriteFile(path string, content []byte, mode fs.FileMode) (result error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".t3-coded-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); closeErr != nil {
				result = errors.Join(result, closeErr)
			}
		}
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, removeErr)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}
