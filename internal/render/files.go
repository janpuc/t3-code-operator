package render

import (
	"fmt"
	pathpkg "path"
	"sort"
	"strings"
)

type renderedWorkstationFileContract struct {
	mode   WriteMode
	format FileFormat
}

var renderedWorkstationFileContracts = map[string]renderedWorkstationFileContract{
	machineInfoPath:     {mode: WriteModeReplace, format: FileFormatText},
	gitConfigPath:       {mode: WriteModeManagedBlock, format: FileFormatText},
	githubConfigPath:    {mode: WriteModeReplace, format: FileFormatYAML},
	githubHostsPath:     {mode: WriteModeReplace, format: FileFormatYAML},
	gitPrivateKeyPath:   {mode: WriteModeReplace, format: FileFormatText},
	gitPublicKeyPath:    {mode: WriteModeReplace, format: FileFormatText},
	gitAllowedUsersPath: {mode: WriteModeReplace, format: FileFormatText},
}

func validateFileTargets(namespace string, providers map[string]ProviderInstance, files []FileTarget) error {
	if len(files) > 512 {
		return validationError("files", "contains more than 512 targets")
	}
	paths := make(map[string]struct{}, len(files))
	for index := range files {
		target := &files[index]
		path := fmt.Sprintf("files[%d]", index)
		if target.Path == "" {
			return validationError(path+".path", "is required")
		}
		if !strings.HasPrefix(target.Path, "/") || pathpkg.Clean(target.Path) != target.Path {
			return validationError(path+".path", "must be a clean absolute path")
		}
		if _, exists := paths[target.Path]; exists {
			return validationError(path+".path", "duplicates file target "+target.Path)
		}
		paths[target.Path] = struct{}{}

		scope := target.Scope
		if scope == "" {
			scope = FileScopeHarness
		}
		switch scope {
		case FileScopeHarness:
			if target.InstanceID == "" {
				return validationError(path+".instanceId", "is required for a Harness file")
			}
			if _, exists := providers[target.InstanceID]; !exists {
				return validationError(path+".instanceId", "does not identify a provider instance")
			}
			prefix := "/data/harnesses/" + target.InstanceID + "/"
			if !strings.HasPrefix(target.Path, prefix) {
				return validationError(path+".path", "is outside the provider instance home")
			}
		case FileScopeWorkstation:
			if target.InstanceID != "" {
				return validationError(path+".instanceId", "must be empty for a Workstation file")
			}
			contract, exists := renderedWorkstationFileContracts[target.Path]
			if !exists || contract.mode != target.Mode || contract.format != target.Format {
				return validationError(path, "does not match an allowed Workstation file target")
			}
		default:
			return validationError(path+".scope", "is not supported")
		}

		switch target.Mode {
		case WriteModeReplace, WriteModeSeedIfAbsent:
		case WriteModeMerge:
			if len(target.OwnedPaths) == 0 {
				return validationError(path+".ownedPaths", "is required for Merge mode")
			}
		case WriteModeManagedBlock:
			if target.Format != FileFormatText {
				return validationError(path+".format", "must be Text for ManagedBlock mode")
			}
		default:
			return validationError(path+".mode", "is not supported")
		}
		switch target.Format {
		case FileFormatJSON, FileFormatTOML, FileFormatYAML, FileFormatText:
		default:
			return validationError(path+".format", "is not supported")
		}
		if target.SecretContent != nil && (target.Content != "" || len(target.Values) != 0) {
			return validationError(path, "secretContent cannot be combined with content or values")
		}
		if target.SecretContent != nil {
			if err := validateSecretReference(namespace, path+".secretContent.valueFrom", target.SecretContent.ValueFrom); err != nil {
				return err
			}
			if err := validateSecretTransform(path+".secretContent.transform", target.SecretContent.Transform); err != nil {
				return err
			}
		}
		owned := make(map[string]struct{}, len(target.OwnedPaths))
		for _, ownedPath := range target.OwnedPaths {
			if err := validateRenderedJSONPointer(ownedPath); err != nil {
				return validationError(path+".ownedPaths", err.Error())
			}
			if _, exists := owned[ownedPath]; exists {
				return validationError(path+".ownedPaths", "contains duplicate "+ownedPath)
			}
			owned[ownedPath] = struct{}{}
		}
		for valueIndex, source := range target.Values {
			valuePath := fmt.Sprintf("%s.values[%d]", path, valueIndex)
			if _, exists := owned[source.Path]; !exists {
				return validationError(valuePath+".path", "must also be an owned path")
			}
			if err := validateSecretReference(namespace, valuePath+".valueFrom", source.ValueFrom); err != nil {
				return err
			}
			if err := validateSecretTransform(valuePath+".transform", source.Transform); err != nil {
				return err
			}
		}
		if target.DiscoverGitSafeDirectories && (scope != FileScopeWorkstation || target.Path != gitConfigPath) {
			return validationError(path+".discoverGitSafeDirectories", "is allowed only for the Workstation Git config")
		}
	}
	return nil
}

func validateRenderedJSONPointer(pointer string) error {
	if pointer == "" || !strings.HasPrefix(pointer, "/") {
		return fmt.Errorf("must contain JSON pointers")
	}
	for _, segment := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		if segment == "" {
			return fmt.Errorf("JSON pointer %q contains an empty segment", pointer)
		}
		for index := 0; index < len(segment); index++ {
			if segment[index] != '~' {
				continue
			}
			if index+1 >= len(segment) || segment[index+1] != '0' && segment[index+1] != '1' {
				return fmt.Errorf("JSON pointer %q contains an invalid escape", pointer)
			}
			index++
		}
	}
	return nil
}

func validateSecretTransform(path string, transform SecretTransform) error {
	switch transform {
	case SecretTransformNone, SecretTransformTrimSpace, SecretTransformOpenSSHPrivateKey:
		return nil
	default:
		return validationError(path, "is not supported")
	}
}

func sortFileTargets(files []FileTarget) {
	for index := range files {
		sort.Strings(files[index].OwnedPaths)
		sort.Slice(files[index].Values, func(left, right int) bool {
			return files[index].Values[left].Path < files[index].Values[right].Path
		})
	}
}
