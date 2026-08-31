package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	slugPattern        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	environmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
)

const MaxRenderedManifestBytes = 900 * 1024

type ValidationError struct {
	Path    string
	Message string
}

func (err *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", err.Path, err.Message)
}

func Render(input ResolvedWorkstation) (Manifest, error) {
	if input.Namespace == "" || input.Name == "" || input.UID == "" {
		return Manifest{}, validationError("workstation", "namespace, name, and UID are required")
	}

	harnesses := append([]Harness(nil), input.Harnesses...)
	sort.Slice(harnesses, func(i, j int) bool {
		return harnesses[i].InstanceID < harnesses[j].InstanceID
	})
	if err := validateHarnessSet(harnesses); err != nil {
		return Manifest{}, err
	}

	manifest := Manifest{
		APIVersion: ProtocolVersion,
		Kind:       "RenderedWorkstation",
		Workstation: WorkstationIdentity{
			Namespace: input.Namespace,
			Name:      input.Name,
			UID:       input.UID,
		},
		ServerSettings: ManagedServerSettings{
			EnableProviderUpdateChecks: false,
		},
		ProviderInstances: make(map[string]ProviderInstance, len(harnesses)),
	}
	workstationFiles, err := renderWorkstationFiles(input)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Files = append(manifest.Files, workstationFiles...)
	tools, err := renderTools(input.Tools)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Tools = tools

	for _, harness := range harnesses {
		adapter, err := adapterFor(harness.Driver)
		if err != nil {
			return Manifest{}, validationError("harnesses."+harness.InstanceID+".driver", err.Error())
		}
		config, err := decodeObject(harness.Config, "harnesses."+harness.InstanceID+".config")
		if err != nil {
			return Manifest{}, err
		}
		if err := rejectInlineSecretFields(config, "harnesses."+harness.InstanceID+".config"); err != nil {
			return Manifest{}, err
		}
		environment, err := renderHarnessEnvironment(input.Namespace, harness)
		if err != nil {
			return Manifest{}, err
		}
		mcpServers, err := normalizeMCPServers(input.Namespace, harness)
		if err != nil {
			return Manifest{}, err
		}

		result, err := adapter.render(input.Namespace, harness, config, mcpServers)
		if err != nil {
			return Manifest{}, err
		}
		environment, err = mergeProviderEnvironment(
			"harnesses."+harness.InstanceID+".environment",
			environment,
			result.environment,
		)
		if err != nil {
			return Manifest{}, err
		}
		configJSON, err := canonicalJSON(result.config)
		if err != nil {
			return Manifest{}, fmt.Errorf("render provider config for %q: %w", harness.InstanceID, err)
		}

		manifest.ProviderInstances[harness.InstanceID] = ProviderInstance{
			Driver:       harness.Driver,
			DisplayName:  harness.DisplayName,
			AccentColor:  harness.AccentColor,
			Enabled:      harness.Enabled,
			SupportLevel: adapter.supportLevel(),
			Environment:  environment,
			Config:       configJSON,
			Apply:        providerApplyPolicy(),
		}
		manifest.Files = append(manifest.Files, result.files...)
		manifest.Extensions = append(manifest.Extensions, result.extensions...)
		manifest.Warnings = append(manifest.Warnings, result.warnings...)
	}
	if err := validateExtensionDestinations(manifest.Extensions); err != nil {
		return Manifest{}, err
	}
	if err := validateFileTargets(input.Namespace, manifest.ProviderInstances, manifest.Files); err != nil {
		return Manifest{}, err
	}

	sortManifest(&manifest)
	revision, err := manifestRevision(manifest)
	if err != nil {
		return Manifest{}, err
	}
	manifest.DesiredRevision = revision
	raw, err := json.Marshal(manifest)
	if err != nil {
		return Manifest{}, fmt.Errorf("serialize rendered manifest: %w", err)
	}
	if len(raw) > MaxRenderedManifestBytes {
		return Manifest{}, validationError(
			"manifest",
			fmt.Sprintf("rendered size %d exceeds the %d-byte limit", len(raw), MaxRenderedManifestBytes),
		)
	}
	return manifest, nil
}

func validateHarnessSet(harnesses []Harness) error {
	instanceIDs := make(map[string]struct{}, len(harnesses))
	driverCounts := make(map[string]int)
	for index, harness := range harnesses {
		path := fmt.Sprintf("harnesses[%d]", index)
		if !slugPattern.MatchString(harness.InstanceID) {
			return validationError(path+".instanceId", "must match the upstream provider instance slug")
		}
		if !slugPattern.MatchString(harness.Driver) {
			return validationError(path+".driver", "must match the upstream driver slug")
		}
		if _, exists := instanceIDs[harness.InstanceID]; exists {
			return validationError(path+".instanceId", "duplicates provider instance "+harness.InstanceID)
		}
		instanceIDs[harness.InstanceID] = struct{}{}
		driverCounts[harness.Driver]++
	}
	for _, driver := range []string{"cursor", "grok", "opencode"} {
		if driverCounts[driver] > 1 {
			return validationError(
				"harnesses",
				fmt.Sprintf("driver %q cannot have multiple instances until its state isolation is verified", driver),
			)
		}
	}
	return nil
}

func renderHarnessEnvironment(namespace string, harness Harness) ([]ProviderEnvironment, error) {
	result := make([]ProviderEnvironment, 0, len(harness.Environment))
	for index, variable := range harness.Environment {
		path := fmt.Sprintf("harnesses.%s.environment[%d]", harness.InstanceID, index)
		rendered, err := renderEnvironmentVariable(namespace, path, variable)
		if err != nil {
			return nil, err
		}
		result = append(result, rendered)
	}
	return mergeProviderEnvironment("harnesses."+harness.InstanceID+".environment", nil, result)
}

func renderEnvironmentVariable(namespace, fieldPath string, variable EnvironmentVariable) (ProviderEnvironment, error) {
	if !environmentPattern.MatchString(variable.Name) {
		return ProviderEnvironment{}, validationError(fieldPath+".name", "is not a valid environment variable name")
	}
	if (variable.Value == nil) == (variable.ValueFrom == nil) {
		return ProviderEnvironment{}, validationError(fieldPath, "exactly one of value or valueFrom is required")
	}
	if variable.Value != nil && isSensitiveEnvironmentName(variable.Name) {
		return ProviderEnvironment{}, validationError(fieldPath+".value", "a sensitive environment variable must use valueFrom")
	}
	if variable.Value != nil && strings.ContainsRune(*variable.Value, '\x00') {
		return ProviderEnvironment{}, validationError(fieldPath+".value", "must not contain NUL")
	}
	if variable.ValueFrom != nil {
		if err := validateSecretReference(namespace, fieldPath+".valueFrom", *variable.ValueFrom); err != nil {
			return ProviderEnvironment{}, err
		}
		return ProviderEnvironment{
			Name:      variable.Name,
			ValueFrom: cloneSecretReference(variable.ValueFrom),
			Sensitive: true,
		}, nil
	}
	return ProviderEnvironment{Name: variable.Name, Value: *variable.Value}, nil
}

func mergeProviderEnvironment(fieldPath string, groups ...[]ProviderEnvironment) ([]ProviderEnvironment, error) {
	byName := make(map[string]ProviderEnvironment)
	for _, group := range groups {
		for _, variable := range group {
			if existing, exists := byName[variable.Name]; exists {
				if providerEnvironmentEqual(existing, variable) {
					continue
				}
				return nil, validationError(fieldPath, "environment variable "+variable.Name+" has conflicting definitions")
			}
			byName[variable.Name] = variable
		}
	}
	result := make([]ProviderEnvironment, 0, len(byName))
	for _, variable := range byName {
		result = append(result, variable)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func providerEnvironmentEqual(left, right ProviderEnvironment) bool {
	if left.Name != right.Name || left.Value != right.Value || left.SecretPrefix != right.SecretPrefix || left.Sensitive != right.Sensitive {
		return false
	}
	if left.ValueFrom == nil || right.ValueFrom == nil {
		return left.ValueFrom == nil && right.ValueFrom == nil
	}
	return *left.ValueFrom == *right.ValueFrom
}

func validateSecretReference(namespace, fieldPath string, reference SecretReference) error {
	if reference.Namespace == "" || reference.Name == "" || reference.Key == "" {
		return validationError(fieldPath, "namespace, name, and key are required")
	}
	if reference.Namespace != namespace {
		return validationError(fieldPath+".namespace", "must match the Workstation namespace")
	}
	return nil
}

func cloneSecretReference(reference *SecretReference) *SecretReference {
	if reference == nil {
		return nil
	}
	copy := *reference
	return &copy
}

func decodeObject(raw json.RawMessage, fieldPath string) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, validationError(fieldPath, "must be a JSON object: "+err.Error())
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, validationError(fieldPath, "must contain one JSON object")
	}
	if object == nil {
		return nil, validationError(fieldPath, "must be a JSON object")
	}
	if err := normalizeJSONNumbers(object); err != nil {
		return nil, validationError(fieldPath, err.Error())
	}
	return object, nil
}

func normalizeJSONNumbers(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if number, ok := child.(json.Number); ok {
				normalized, err := normalizeJSONNumber(number)
				if err != nil {
					return err
				}
				typed[key] = normalized
				continue
			}
			if err := normalizeJSONNumbers(child); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if number, ok := child.(json.Number); ok {
				normalized, err := normalizeJSONNumber(number)
				if err != nil {
					return err
				}
				typed[index] = normalized
				continue
			}
			if err := normalizeJSONNumbers(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeJSONNumber(number json.Number) (json.Number, error) {
	text := number.String()
	if !strings.ContainsAny(text, ".eE") {
		integer := new(big.Int)
		if _, ok := integer.SetString(text, 10); !ok {
			return "", fmt.Errorf("contains invalid JSON number %q", text)
		}
		return json.Number(integer.String()), nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return "", fmt.Errorf("contains a JSON number outside the supported range")
	}
	return json.Number(strconv.FormatFloat(value, 'g', -1, 64)), nil
}

func canonicalJSON(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

func sortManifest(manifest *Manifest) {
	sortFileTargets(manifest.Files)
	for instanceID, instance := range manifest.ProviderInstances {
		sort.Slice(instance.Environment, func(i, j int) bool {
			return instance.Environment[i].Name < instance.Environment[j].Name
		})
		manifest.ProviderInstances[instanceID] = instance
	}
	sort.Slice(manifest.Files, func(i, j int) bool {
		if manifest.Files[i].InstanceID == manifest.Files[j].InstanceID {
			return manifest.Files[i].Path < manifest.Files[j].Path
		}
		return manifest.Files[i].InstanceID < manifest.Files[j].InstanceID
	})
	for index := range manifest.Extensions {
		sort.Slice(manifest.Extensions[index].Destinations, func(i, j int) bool {
			return manifest.Extensions[index].Destinations[i].Path < manifest.Extensions[index].Destinations[j].Path
		})
	}
	sort.Slice(manifest.Extensions, func(i, j int) bool {
		if manifest.Extensions[i].InstanceID == manifest.Extensions[j].InstanceID {
			return manifest.Extensions[i].Name < manifest.Extensions[j].Name
		}
		return manifest.Extensions[i].InstanceID < manifest.Extensions[j].InstanceID
	})
	for index := range manifest.Tools {
		sort.Slice(manifest.Tools[index].Artifacts, func(i, j int) bool {
			return manifest.Tools[index].Artifacts[i].Platform < manifest.Tools[index].Artifacts[j].Platform
		})
	}
	sort.Slice(manifest.Tools, func(i, j int) bool {
		return manifest.Tools[i].Name < manifest.Tools[j].Name
	})
	sort.Slice(manifest.Warnings, func(i, j int) bool {
		left := manifest.Warnings[i]
		right := manifest.Warnings[j]
		if left.InstanceID != right.InstanceID {
			return left.InstanceID < right.InstanceID
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if left.Resource != right.Resource {
			return left.Resource < right.Resource
		}
		return left.Message < right.Message
	})
}

func manifestRevision(manifest Manifest) (string, error) {
	manifest.DesiredRevision = ""
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("serialize rendered manifest: %w", err)
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func VerifyManifest(manifest Manifest) error {
	if manifest.APIVersion != ProtocolVersion {
		return validationError("apiVersion", fmt.Sprintf("unsupported protocol %q", manifest.APIVersion))
	}
	if manifest.Kind != "RenderedWorkstation" {
		return validationError("kind", fmt.Sprintf("unsupported manifest kind %q", manifest.Kind))
	}
	if manifest.DesiredRevision == "" {
		return validationError("desiredRevision", "is required")
	}
	if manifest.Workstation.Namespace == "" || manifest.Workstation.Name == "" || manifest.Workstation.UID == "" {
		return validationError("workstation", "namespace, name, and UID are required")
	}
	if manifest.ProviderInstances == nil {
		return validationError("providerInstances", "must be an object")
	}
	if manifest.ServerSettings.EnableProviderUpdateChecks {
		return validationError("serverSettings.enableProviderUpdateChecks", "must be false")
	}
	if err := validateRenderedProviders(manifest.Workstation.Namespace, manifest.ProviderInstances); err != nil {
		return err
	}
	if err := validateFileTargets(manifest.Workstation.Namespace, manifest.ProviderInstances, manifest.Files); err != nil {
		return err
	}
	if err := validateRenderedFilePolicies(manifest.Files); err != nil {
		return err
	}
	if err := validateRenderedExtensions(manifest.Workstation.Namespace, manifest.ProviderInstances, manifest.Extensions); err != nil {
		return err
	}
	if err := ValidateToolActivations(manifest.Tools); err != nil {
		return err
	}
	expected, err := manifestRevision(manifest)
	if err != nil {
		return err
	}
	if manifest.DesiredRevision != expected {
		return validationError("desiredRevision", "does not match the manifest content")
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("serialize rendered manifest: %w", err)
	}
	if len(raw) > MaxRenderedManifestBytes {
		return validationError(
			"manifest",
			fmt.Sprintf("rendered size %d exceeds the %d-byte limit", len(raw), MaxRenderedManifestBytes),
		)
	}
	return nil
}

func validationError(fieldPath, message string) error {
	return &ValidationError{Path: fieldPath, Message: strings.TrimSpace(message)}
}

func providerApplyPolicy() ApplyPolicy {
	return ApplyPolicy{
		Class:     ChangeClassDisruptive,
		When:      ApplyWhenIdle,
		Mechanism: ReloadProviderRebuild,
	}
}

func toolApplyPolicy() ApplyPolicy {
	return ApplyPolicy{
		Class:     ChangeClassDisruptive,
		When:      ApplyWhenIdle,
		Mechanism: ReloadNextSession,
	}
}
