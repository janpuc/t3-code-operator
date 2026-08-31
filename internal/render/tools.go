package render

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var (
	toolNamePattern     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
	toolBackendPattern  = regexp.MustCompile(`^(aqua|github|gitlab|http):[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)
	toolPlatformPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	toolSHA256Pattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

func renderTools(input []ResolvedTool) ([]ToolActivation, error) {
	tools := append([]ResolvedTool(nil), input...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	names := make(map[string]struct{}, len(tools))
	backends := make(map[string]struct{}, len(tools))
	result := make([]ToolActivation, 0, len(tools))
	for index, tool := range tools {
		path := fmt.Sprintf("tools[%d]", index)
		if !toolNamePattern.MatchString(tool.Name) {
			return nil, validationError(path+".name", "must be a valid tool name")
		}
		if _, exists := names[tool.Name]; exists {
			return nil, validationError(path+".name", "duplicates tool "+tool.Name)
		}
		names[tool.Name] = struct{}{}
		if !toolBackendPattern.MatchString(tool.Backend) {
			return nil, validationError(path+".backend", "must select a fully lockable mise backend")
		}
		if _, exists := backends[tool.Backend]; exists {
			return nil, validationError(path+".backend", "duplicates mise backend "+tool.Backend)
		}
		backends[tool.Backend] = struct{}{}
		if err := validatePinnedToolVersion(tool.Version); err != nil {
			return nil, validationError(path+".version", err.Error())
		}
		options, err := validateToolOptions(path+".options", tool.Options)
		if err != nil {
			return nil, err
		}
		artifacts, err := validateToolArtifacts(path+".artifacts", tool.Artifacts)
		if err != nil {
			return nil, err
		}
		result = append(result, ToolActivation{
			Name:      tool.Name,
			Backend:   tool.Backend,
			Version:   tool.Version,
			Options:   options,
			Artifacts: artifacts,
			Apply:     toolApplyPolicy(),
		})
	}
	return result, nil
}

func validatePinnedToolVersion(version string) error {
	if version == "" || len(version) > 128 || strings.TrimSpace(version) != version {
		return fmt.Errorf("must be a nonempty pinned version of at most 128 characters")
	}
	lower := strings.ToLower(version)
	for _, selector := range []string{"latest", "lts", "system"} {
		if lower == selector {
			return fmt.Errorf("must not use the unpinned selector %q", selector)
		}
	}
	for _, prefix := range []string{"path:", "prefix:", "ref:"} {
		if strings.HasPrefix(lower, prefix) {
			return fmt.Errorf("must not use the unpinned selector %q", prefix)
		}
	}
	if containsToolControl(version) {
		return fmt.Errorf("must not contain control characters")
	}
	return nil
}

func validateToolOptions(fieldPath string, input map[string]string) (map[string]string, error) {
	if len(input) > 32 {
		return nil, validationError(fieldPath, "must contain at most 32 entries")
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		if !environmentPattern.MatchString(key) || len(key) > 64 {
			return nil, validationError(fieldPath, "contains an invalid option name")
		}
		if value == "" || len(value) > 1024 || containsToolControl(value) {
			return nil, validationError(fieldPath+"."+key, "must be a nonempty single-line value")
		}
		if isSensitiveEnvironmentName(key) {
			return nil, validationError(fieldPath+"."+key, "must not contain sensitive data")
		}
		switch key {
		case "version", "platforms", "url", "checksum", "size":
			return nil, validationError(fieldPath+"."+key, "is reserved for resolved tool data")
		}
		result[key] = value
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func containsToolControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func validateToolArtifacts(fieldPath string, input []ToolArtifact) ([]ToolArtifact, error) {
	if len(input) == 0 || len(input) > 16 {
		return nil, validationError(fieldPath, "must contain between 1 and 16 resolved artifacts")
	}
	result := append([]ToolArtifact(nil), input...)
	sort.Slice(result, func(i, j int) bool { return result[i].Platform < result[j].Platform })
	platforms := make(map[string]struct{}, len(result))
	for index, artifact := range result {
		path := fmt.Sprintf("%s[%d]", fieldPath, index)
		if !toolPlatformPattern.MatchString(artifact.Platform) {
			return nil, validationError(path+".platform", "must be a valid mise platform key")
		}
		if _, exists := platforms[artifact.Platform]; exists {
			return nil, validationError(path+".platform", "duplicates platform "+artifact.Platform)
		}
		platforms[artifact.Platform] = struct{}{}
		parsed, err := url.Parse(artifact.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
			return nil, validationError(path+".url", "must be a credential-free HTTPS artifact URL")
		}
		if !toolSHA256Pattern.MatchString(artifact.SHA256) {
			return nil, validationError(path+".sha256", "must be a lowercase SHA-256 value")
		}
		if artifact.Size < 0 {
			return nil, validationError(path+".size", "must not be negative")
		}
	}
	return result, nil
}

func ValidateToolActivations(tools []ToolActivation) error {
	resolved := make([]ResolvedTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Apply != toolApplyPolicy() {
			return validationError("tools."+tool.Name+".apply", "does not match the tool apply policy")
		}
		resolved = append(resolved, ResolvedTool{
			Name:      tool.Name,
			Backend:   tool.Backend,
			Version:   tool.Version,
			Options:   tool.Options,
			Artifacts: tool.Artifacts,
		})
	}
	_, err := renderTools(resolved)
	return err
}
