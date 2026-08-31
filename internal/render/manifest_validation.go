package render

import (
	"fmt"
	pathpkg "path"
	"strings"
)

func validateRenderedProviders(namespace string, providers map[string]ProviderInstance) error {
	harnesses := make([]Harness, 0, len(providers))
	for instanceID, provider := range providers {
		path := "providerInstances." + instanceID
		harnesses = append(harnesses, Harness{InstanceID: instanceID, Driver: provider.Driver})
		adapter, err := adapterFor(provider.Driver)
		if err != nil {
			return validationError(path+".driver", err.Error())
		}
		if provider.SupportLevel != adapter.supportLevel() {
			return validationError(path+".supportLevel", "does not match the driver support level")
		}
		if provider.Apply != providerApplyPolicy() {
			return validationError(path+".apply", "does not match the provider apply policy")
		}
		config, err := decodeObject(provider.Config, path+".config")
		if err != nil {
			return err
		}
		if err := rejectInlineSecretFields(config, path+".config"); err != nil {
			return err
		}
		if len(provider.Environment) > 512 {
			return validationError(path+".environment", "contains more than 512 variables")
		}
		seenEnvironment := make(map[string]struct{}, len(provider.Environment))
		for index, variable := range provider.Environment {
			variablePath := fmt.Sprintf("%s.environment[%d]", path, index)
			if !environmentPattern.MatchString(variable.Name) {
				return validationError(variablePath+".name", "is not a valid environment variable name")
			}
			if _, exists := seenEnvironment[variable.Name]; exists {
				return validationError(variablePath+".name", "duplicates environment variable "+variable.Name)
			}
			seenEnvironment[variable.Name] = struct{}{}
			if variable.ValueFrom == nil {
				if variable.Sensitive || variable.SecretPrefix != "" {
					return validationError(variablePath, "a literal variable cannot be sensitive or have a Secret prefix")
				}
				if isSensitiveEnvironmentName(variable.Name) {
					return validationError(variablePath+".value", "a sensitive environment variable must use valueFrom")
				}
				if strings.ContainsRune(variable.Value, '\x00') {
					return validationError(variablePath+".value", "must not contain NUL")
				}
				continue
			}
			if variable.Value != "" || !variable.Sensitive {
				return validationError(variablePath, "a Secret-backed variable must omit value and be sensitive")
			}
			if strings.ContainsAny(variable.SecretPrefix, "\x00\r\n") {
				return validationError(variablePath+".secretPrefix", "must be a single-line value")
			}
			if err := validateSecretReference(namespace, variablePath+".valueFrom", *variable.ValueFrom); err != nil {
				return err
			}
		}
	}
	return validateHarnessSet(harnesses)
}

func validateRenderedExtensions(
	namespace string,
	providers map[string]ProviderInstance,
	activations []ExtensionActivation,
) error {
	identities := make(map[string]struct{}, len(activations))
	for index, activation := range activations {
		fieldPath := fmt.Sprintf("extensions[%d]", index)
		provider, exists := providers[activation.InstanceID]
		if !exists {
			return validationError(fieldPath+".instanceId", "does not identify a provider instance")
		}
		if !safePathSegment.MatchString(activation.Name) || activation.Name == "." || activation.Name == ".." {
			return validationError(fieldPath+".name", "must be a safe path segment")
		}
		identity := activation.InstanceID + "\x00" + activation.Name
		if _, exists := identities[identity]; exists {
			return validationError(fieldPath, "duplicates Extension activation "+activation.Name)
		}
		identities[identity] = struct{}{}
		if err := validateExtensionSource(namespace, fieldPath+".source", activation.Source); err != nil {
			return err
		}
		expectedCacheKey, err := ExtensionCacheKey(activation.Source)
		if err != nil {
			return validationError(fieldPath+".cacheKey", err.Error())
		}
		if activation.CacheKey != expectedCacheKey {
			return validationError(fieldPath+".cacheKey", "does not match the Extension source")
		}
		reload, skillRoot, installerKind, releaseKind, supported := renderedExtensionDialect(
			activation.InstanceID,
			provider.Driver,
		)
		if !supported {
			return validationError(fieldPath+".instanceId", "selects a driver without an Extension dialect")
		}
		expectedApply := extensionApplyPolicy(reload)
		if activation.Apply != expectedApply {
			return validationError(fieldPath+".apply", "does not match the Extension apply policy")
		}

		switch activation.Source.Type {
		case ExtensionSourceGit, ExtensionSourceOCI:
			if activation.Installer != nil || len(activation.Destinations) == 0 {
				return validationError(fieldPath, "a direct source requires destinations and no installer")
			}
		case ExtensionSourceMarketplace:
			if installerKind == "" || activation.Installer == nil || len(activation.Destinations) != 0 {
				return validationError(fieldPath, "the driver cannot install this Marketplace source")
			}
			installer := activation.Installer
			if installer.Kind != installerKind ||
				installer.Marketplace != activation.Source.Marketplace.Marketplace ||
				installer.Extension != activation.Source.Marketplace.Extension {
				return validationError(fieldPath+".installer", "does not match the Marketplace source")
			}
		case ExtensionSourceGitHubRelease:
			if releaseKind == "" || activation.Installer == nil || len(activation.Destinations) != 0 {
				return validationError(fieldPath, "the driver cannot install this GitHubRelease source")
			}
			installer := activation.Installer
			if installer.Kind != releaseKind ||
				installer.Marketplace != repositoryOwner(activation.Source.GitHubRelease.Repository) ||
				installer.Extension != activation.Name {
				return validationError(fieldPath+".installer", "does not match the GitHubRelease source")
			}
		}

		for destinationIndex, destination := range activation.Destinations {
			destinationPath := fmt.Sprintf("%s.destinations[%d]", fieldPath, destinationIndex)
			if destination.SourcePath != "." && !safeRelativePath(destination.SourcePath) {
				return validationError(destinationPath+".sourcePath", "must be a safe relative path")
			}
			if destination.Path == skillRoot || pathpkg.Clean(destination.Path) != destination.Path ||
				!strings.HasPrefix(destination.Path, skillRoot+"/") {
				return validationError(destinationPath+".path", "is outside the driver skill root")
			}
			if destination.Mode != WriteModeReplace {
				return validationError(destinationPath+".mode", "must be Replace")
			}
			if destination.Apply != expectedApply {
				return validationError(destinationPath+".apply", "does not match the Extension apply policy")
			}
		}
	}
	return validateExtensionDestinations(activations)
}

func renderedExtensionDialect(
	instanceID string,
	driver string,
) (ReloadMechanism, string, InstallerKind, InstallerKind, bool) {
	adapter, err := adapterFor(driver)
	if err != nil {
		return "", "", "", "", false
	}
	dialect, supported := adapter.extensionDialect(instanceID)
	return dialect.reload,
		dialect.skillRoot,
		dialect.marketplaceInstaller,
		dialect.releaseInstaller,
		supported
}

func validateRenderedFilePolicies(files []FileTarget) error {
	for index, file := range files {
		expected := disruptiveFileApplyPolicy()
		if file.Scope == FileScopeWorkstation {
			expected = workstationFileApplyPolicy()
		}
		if file.Apply != expected {
			return validationError(fmt.Sprintf("files[%d].apply", index), "does not match the file apply policy")
		}
	}
	return nil
}
