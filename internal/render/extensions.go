package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	pathpkg "path"
	"regexp"
	"sort"
	"strings"
)

var (
	commitPattern    = regexp.MustCompile(`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	ociDigestPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+._-]*:[0-9a-fA-F]{32,}$`)
	sha256Pattern    = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	safePathSegment  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,252}$`)
)

type extensionDialect struct {
	skillRoot            string
	marketplaceInstaller InstallerKind
	releaseInstaller     InstallerKind
	reload               ReloadMechanism
}

func (dialect extensionDialect) programsSource(sourceType ExtensionSourceType) bool {
	switch sourceType {
	case ExtensionSourceGit, ExtensionSourceOCI:
		return dialect.skillRoot != ""
	case ExtensionSourceMarketplace:
		return dialect.marketplaceInstaller != ""
	case ExtensionSourceGitHubRelease:
		return dialect.releaseInstaller != ""
	default:
		return false
	}
}

func renderExtensions(
	namespace string,
	harness Harness,
	dialect extensionDialect,
) ([]ExtensionActivation, []Issue, error) {
	extensions := append([]Extension(nil), harness.Extensions...)
	sort.Slice(extensions, func(i, j int) bool { return extensions[i].Name < extensions[j].Name })
	if err := validateExtensions(namespace, extensions); err != nil {
		return nil, nil, err
	}

	result := make([]ExtensionActivation, 0, len(extensions))
	warnings := make([]Issue, 0)
	for _, extension := range extensions {
		source := cloneExtensionSource(extension.Source)
		cacheKey, err := ExtensionCacheKey(source)
		if err != nil {
			return nil, nil, fmt.Errorf("render Extension %q cache key: %w", extension.Name, err)
		}
		activation := ExtensionActivation{
			InstanceID: harness.InstanceID,
			Name:       extension.Name,
			Source:     source,
			CacheKey:   cacheKey,
			Apply:      extensionApplyPolicy(dialect.reload),
		}
		switch source.Type {
		case ExtensionSourceGit, ExtensionSourceOCI:
			activation.Destinations = renderSkillDestinations(extension.Name, source, dialect)
		case ExtensionSourceMarketplace:
			if dialect.marketplaceInstaller == "" {
				warnings = append(warnings, Issue{
					Code:       IssueUnsupportedExtensionSource,
					InstanceID: harness.InstanceID,
					Resource:   extension.Name,
					Message:    "the adapter cannot install Marketplace extensions from a pinned repository",
				})
				continue
			}
			activation.Installer = &ExtensionInstaller{
				Kind:        dialect.marketplaceInstaller,
				Marketplace: source.Marketplace.Marketplace,
				Extension:   source.Marketplace.Extension,
			}
		case ExtensionSourceGitHubRelease:
			if dialect.releaseInstaller == "" {
				warnings = append(warnings, Issue{
					Code:       IssueUnsupportedExtensionSource,
					InstanceID: harness.InstanceID,
					Resource:   extension.Name,
					Message:    "the adapter cannot install GitHubRelease extension bundles",
				})
				continue
			}
			activation.Installer = &ExtensionInstaller{
				Kind:        dialect.releaseInstaller,
				Marketplace: repositoryOwner(source.GitHubRelease.Repository),
				Extension:   extension.Name,
			}
		}
		result = append(result, activation)
	}
	return result, warnings, nil
}

func validateExtensions(namespace string, extensions []Extension) error {
	names := make(map[string]struct{}, len(extensions))
	for index, extension := range extensions {
		fieldPath := fmt.Sprintf("extensions[%d]", index)
		if !safePathSegment.MatchString(extension.Name) || extension.Name == "." || extension.Name == ".." {
			return validationError(fieldPath+".name", "must be a safe path segment")
		}
		if _, exists := names[extension.Name]; exists {
			return validationError(fieldPath+".name", "duplicates Extension "+extension.Name)
		}
		names[extension.Name] = struct{}{}
		if err := validateExtensionSource(namespace, fieldPath+".source", extension.Source); err != nil {
			return err
		}
	}
	return nil
}

func validateExtensionSource(namespace, fieldPath string, source ExtensionSource) error {
	configured := 0
	for _, present := range []bool{
		source.Git != nil,
		source.OCI != nil,
		source.Marketplace != nil,
		source.GitHubRelease != nil,
	} {
		if present {
			configured++
		}
	}
	if configured != 1 {
		return validationError(fieldPath, "exactly one source configuration is required")
	}
	for index, include := range source.Include {
		if !safePathSegment.MatchString(include) || include == "." || include == ".." {
			return validationError(fmt.Sprintf("%s.include[%d]", fieldPath, index), "must be a safe single path segment")
		}
	}

	switch source.Type {
	case ExtensionSourceGit:
		if source.Git == nil {
			return validationError(fieldPath+".git", "is required for a Git source")
		}
		if err := validateRepositoryURL(source.Git.URL, fieldPath+".git.url"); err != nil {
			return err
		}
		if !commitPattern.MatchString(source.Git.Commit) {
			return validationError(fieldPath+".git.commit", "must be a full 40-byte or 64-byte hexadecimal commit")
		}
		if source.Git.Path != "" && !safeRelativePath(source.Git.Path) {
			return validationError(fieldPath+".git.path", "must be a safe relative path")
		}
		return validateOptionalSecretReference(namespace, fieldPath+".git.credentialSecretRef", source.Git.CredentialSecretRef)
	case ExtensionSourceOCI:
		if source.OCI == nil {
			return validationError(fieldPath+".oci", "is required for an OCI source")
		}
		if source.OCI.Repository == "" || !ociDigestPattern.MatchString(source.OCI.Digest) {
			return validationError(fieldPath+".oci", "repository and a pinned digest are required")
		}
		return validateOptionalSecretReference(namespace, fieldPath+".oci.credentialSecretRef", source.OCI.CredentialSecretRef)
	case ExtensionSourceMarketplace:
		if source.Marketplace == nil {
			return validationError(fieldPath+".marketplace", "is required for a Marketplace source")
		}
		marketplace := source.Marketplace
		if !safePathSegment.MatchString(marketplace.Marketplace) ||
			!safePathSegment.MatchString(marketplace.Extension) {
			return validationError(fieldPath+".marketplace", "marketplace and extension must be safe plugin identifiers")
		}
		if err := validateRepositoryURL(marketplace.RepositoryURL, fieldPath+".marketplace.repositoryUrl"); err != nil {
			return err
		}
		if !commitPattern.MatchString(marketplace.Commit) {
			return validationError(fieldPath+".marketplace.commit", "must be a full 40-byte or 64-byte hexadecimal commit")
		}
		return validateOptionalSecretReference(namespace, fieldPath+".marketplace.credentialSecretRef", marketplace.CredentialSecretRef)
	case ExtensionSourceGitHubRelease:
		if source.GitHubRelease == nil {
			return validationError(fieldPath+".githubRelease", "is required for a GitHubRelease source")
		}
		release := source.GitHubRelease
		if !strings.Contains(release.Repository, "/") || release.Tag == "" || release.Asset == "" || !sha256Pattern.MatchString(release.SHA256) {
			return validationError(fieldPath+".githubRelease", "repository, tag, asset, and SHA-256 are required")
		}
		return validateOptionalSecretReference(namespace, fieldPath+".githubRelease.credentialSecretRef", release.CredentialSecretRef)
	default:
		return validationError(fieldPath+".type", fmt.Sprintf("source type %q is not implemented", source.Type))
	}
}

func validateOptionalSecretReference(namespace, fieldPath string, reference *SecretReference) error {
	if reference == nil {
		return nil
	}
	return validateSecretReference(namespace, fieldPath, *reference)
}

func validateRepositoryURL(rawURL, fieldPath string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "ssh") || parsed.Host == "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return validationError(fieldPath, "must be an absolute HTTPS or SSH URL")
	}
	if parsed.User != nil && parsed.Scheme == "https" {
		return validationError(fieldPath, "must not contain credentials")
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return validationError(fieldPath, "must not contain a password")
		}
		if parsed.User.Username() == "" {
			return validationError(fieldPath, "has an empty SSH username")
		}
	}
	return nil
}

func safeRelativePath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && pathpkg.Clean(value) == value && value != "." && value != ".." && !strings.HasPrefix(value, "../")
}

func renderSkillDestinations(name string, source ExtensionSource, dialect extensionDialect) []ExtensionDestination {
	include := append([]string(nil), source.Include...)
	sort.Strings(include)
	basePath := ""
	if source.Git != nil {
		basePath = source.Git.Path
	}
	if len(include) == 0 {
		sourcePath := basePath
		if sourcePath == "" {
			sourcePath = "."
		}
		return []ExtensionDestination{{
			SourcePath: sourcePath,
			Path:       dialect.skillRoot + "/" + name,
			Mode:       WriteModeReplace,
			Apply:      extensionApplyPolicy(dialect.reload),
		}}
	}
	result := make([]ExtensionDestination, 0, len(include))
	for _, selected := range include {
		sourcePath := selected
		if basePath != "" {
			sourcePath = pathpkg.Join(basePath, selected)
		}
		result = append(result, ExtensionDestination{
			SourcePath: sourcePath,
			Path:       dialect.skillRoot + "/" + selected,
			Mode:       WriteModeReplace,
			Apply:      extensionApplyPolicy(dialect.reload),
		})
	}
	return result
}

func ExtensionCacheKey(source ExtensionSource) (string, error) {
	source = cloneExtensionSource(source)
	if source.Git != nil {
		source.Git.CredentialSecretRef = nil
	}
	if source.OCI != nil {
		source.OCI.CredentialSecretRef = nil
	}
	if source.Marketplace != nil {
		source.Marketplace.CredentialSecretRef = nil
	}
	if source.GitHubRelease != nil {
		source.GitHubRelease.CredentialSecretRef = nil
	}
	sort.Strings(source.Include)
	raw, err := canonicalJSON(source)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func cloneExtensionSource(source ExtensionSource) ExtensionSource {
	result := source
	result.Include = append([]string(nil), source.Include...)
	sort.Strings(result.Include)
	if source.Git != nil {
		copy := *source.Git
		copy.CredentialSecretRef = cloneSecretReference(source.Git.CredentialSecretRef)
		result.Git = &copy
	}
	if source.OCI != nil {
		copy := *source.OCI
		copy.CredentialSecretRef = cloneSecretReference(source.OCI.CredentialSecretRef)
		result.OCI = &copy
	}
	if source.Marketplace != nil {
		copy := *source.Marketplace
		copy.CredentialSecretRef = cloneSecretReference(source.Marketplace.CredentialSecretRef)
		result.Marketplace = &copy
	}
	if source.GitHubRelease != nil {
		copy := *source.GitHubRelease
		copy.CredentialSecretRef = cloneSecretReference(source.GitHubRelease.CredentialSecretRef)
		result.GitHubRelease = &copy
	}
	return result
}

func validateExtensionDestinations(activations []ExtensionActivation) error {
	owners := make(map[string]string)
	installers := make(map[string]string)
	for _, activation := range activations {
		for _, destination := range activation.Destinations {
			key := activation.InstanceID + "\x00" + destination.Path
			if owner, exists := owners[key]; exists {
				return validationError("extensions", fmt.Sprintf("destination collision at %s between %s and %s", destination.Path, owner, activation.Name))
			}
			owners[key] = activation.Name
		}
		if activation.Installer != nil {
			installer := activation.Installer
			key := activation.InstanceID + "\x00" + string(installer.Kind) + "\x00" + installer.Marketplace + "\x00" + installer.Extension
			if owner, exists := installers[key]; exists {
				return validationError("extensions", fmt.Sprintf("installer collision between %s and %s", owner, activation.Name))
			}
			installers[key] = activation.Name
		}
	}
	return nil
}

func extensionApplyPolicy(mechanism ReloadMechanism) ApplyPolicy {
	return ApplyPolicy{Class: ChangeClassDisruptive, When: ApplyWhenIdle, Mechanism: mechanism}
}

func repositoryOwner(repository string) string {
	owner, _, found := strings.Cut(repository, "/")
	if !found {
		return repository
	}
	return owner
}
