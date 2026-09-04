package render

import (
	"fmt"
	pathpkg "path"
	"strings"
)

type driverAdapter interface {
	supportLevel() SupportLevel
	programsMCPServers() bool
	extensionDialect(string) (extensionDialect, bool)
	render(string, Harness, map[string]any, []normalizedMCPServer) (adapterResult, error)
}

type adapterResult struct {
	config      map[string]any
	environment []ProviderEnvironment
	files       []FileTarget
	extensions  []ExtensionActivation
	warnings    []Issue
}

type codexAdapter struct{}
type claudeAdapter struct{}
type openCodeAdapter struct{}
type alphaAdapter struct {
	driver string
}

func adapterFor(driver string) (driverAdapter, error) {
	switch driver {
	case "codex":
		return codexAdapter{}, nil
	case "claudeAgent":
		return claudeAdapter{}, nil
	case "opencode":
		return openCodeAdapter{}, nil
	case "cursor", "grok", "antigravity":
		return alphaAdapter{driver: driver}, nil
	default:
		return nil, fmt.Errorf("driver %q has no renderer adapter", driver)
	}
}

func (codexAdapter) supportLevel() SupportLevel {
	return SupportLevelSupported
}

func (codexAdapter) programsMCPServers() bool {
	return true
}

func (codexAdapter) extensionDialect(instanceID string) (extensionDialect, bool) {
	return extensionDialect{
		skillRoot:            managedHarnessPath(instanceID, "codex") + "/skills",
		marketplaceInstaller: InstallerCodexMarketplace,
		releaseInstaller:     InstallerCodexReleaseBundle,
		reload:               ReloadWatch,
	}, true
}

func (adapter codexAdapter) render(
	namespace string,
	harness Harness,
	config map[string]any,
	mcpServers []normalizedMCPServer,
) (adapterResult, error) {
	config = cloneObject(config)
	fileConfig, err := takeAdapterFileConfig(config, "harnesses."+harness.InstanceID+".config.file")
	if err != nil {
		return adapterResult{}, err
	}
	if _, exists := fileConfig["mcp_servers"]; exists {
		return adapterResult{}, validationError(
			"harnesses."+harness.InstanceID+".config.file.mcp_servers",
			"is adapter-owned; use MCPServer attachments",
		)
	}
	for _, key := range []string{"marketplaces", "plugins"} {
		if _, exists := fileConfig[key]; exists {
			return adapterResult{}, validationError(
				"harnesses."+harness.InstanceID+".config.file."+key,
				"is installer-owned; use Extension attachments",
			)
		}
	}
	homePath := managedHarnessPath(harness.InstanceID, "codex")
	if err := setManagedHomePath(config, homePath, harness.InstanceID); err != nil {
		return adapterResult{}, err
	}
	if err := validateOptionalManagedPath(config, "shadowHomePath", harness.InstanceID); err != nil {
		return adapterResult{}, err
	}

	content, environment, ownedPaths, err := renderCodexMCP(namespace, harness.InstanceID, fileConfig, mcpServers)
	if err != nil {
		return adapterResult{}, err
	}
	result := adapterResult{config: config, environment: environment}
	dialect, _ := adapter.extensionDialect(harness.InstanceID)
	result.extensions, result.warnings, err = renderExtensions(namespace, harness, dialect)
	if err != nil {
		return adapterResult{}, err
	}
	if content != "" {
		result.files = []FileTarget{{
			InstanceID: harness.InstanceID,
			Path:       homePath + "/config.toml",
			Mode:       WriteModeMerge,
			Format:     FileFormatTOML,
			Content:    content,
			OwnedPaths: ownedPaths,
			Apply:      disruptiveFileApplyPolicy(),
		}}
	}
	return result, nil
}

func (claudeAdapter) supportLevel() SupportLevel {
	return SupportLevelSupported
}

func (claudeAdapter) programsMCPServers() bool {
	return true
}

func (claudeAdapter) extensionDialect(instanceID string) (extensionDialect, bool) {
	return extensionDialect{
		skillRoot:            managedHarnessPath(instanceID, "claude") + "/skills",
		marketplaceInstaller: InstallerClaudeMarketplace,
		releaseInstaller:     InstallerClaudeReleaseBundle,
		reload:               ReloadNextSession,
	}, true
}

func (adapter claudeAdapter) render(
	namespace string,
	harness Harness,
	config map[string]any,
	mcpServers []normalizedMCPServer,
) (adapterResult, error) {
	config = cloneObject(config)
	fileConfig, err := takeAdapterFileConfig(config, "harnesses."+harness.InstanceID+".config.file")
	if err != nil {
		return adapterResult{}, err
	}
	if _, exists := fileConfig["mcpServers"]; exists {
		return adapterResult{}, validationError(
			"harnesses."+harness.InstanceID+".config.file.mcpServers",
			"is adapter-owned; use MCPServer attachments",
		)
	}
	homePath := managedHarnessPath(harness.InstanceID, "claude")
	if err := setManagedHomePath(config, homePath, harness.InstanceID); err != nil {
		return adapterResult{}, err
	}
	for _, key := range []string{"enabledPlugins", "extraKnownMarketplaces"} {
		if _, exists := fileConfig[key]; exists {
			return adapterResult{}, validationError(
				"harnesses."+harness.InstanceID+".config.file."+key,
				"is installer-owned; use Extension attachments",
			)
		}
	}
	mcpContent, environment, mcpOwnedPaths, err := renderClaudeMCP(namespace, harness.InstanceID, mcpServers)
	if err != nil {
		return adapterResult{}, err
	}
	settingsContent, settingsOwnedPaths, err := renderJSONFileConfig(fileConfig, "Claude settings")
	if err != nil {
		return adapterResult{}, err
	}
	result := adapterResult{config: config, environment: environment}
	dialect, _ := adapter.extensionDialect(harness.InstanceID)
	result.extensions, result.warnings, err = renderExtensions(namespace, harness, dialect)
	if err != nil {
		return adapterResult{}, err
	}
	if mcpContent != "" {
		result.files = append(result.files, FileTarget{
			InstanceID: harness.InstanceID,
			Path:       homePath + "/.claude.json",
			Mode:       WriteModeMerge,
			Format:     FileFormatJSON,
			Content:    mcpContent,
			OwnedPaths: mcpOwnedPaths,
			Apply:      disruptiveFileApplyPolicy(),
		})
	}
	if settingsContent != "" {
		result.files = append(result.files, FileTarget{
			InstanceID: harness.InstanceID,
			Path:       homePath + "/settings.json",
			Mode:       WriteModeMerge,
			Format:     FileFormatJSON,
			Content:    settingsContent,
			OwnedPaths: settingsOwnedPaths,
			Apply:      disruptiveFileApplyPolicy(),
		})
	}
	return result, nil
}

func (openCodeAdapter) supportLevel() SupportLevel {
	return SupportLevelSupported
}

func (openCodeAdapter) programsMCPServers() bool {
	return true
}

func (openCodeAdapter) extensionDialect(instanceID string) (extensionDialect, bool) {
	return extensionDialect{
		skillRoot: managedHarnessPath(instanceID, "opencode") + "/skills",
		reload:    ReloadNextSession,
	}, true
}

func (adapter openCodeAdapter) render(
	namespace string,
	harness Harness,
	config map[string]any,
	mcpServers []normalizedMCPServer,
) (adapterResult, error) {
	config = cloneObject(config)
	fileConfig, err := takeAdapterFileConfig(config, "harnesses."+harness.InstanceID+".config.file")
	if err != nil {
		return adapterResult{}, err
	}
	if _, exists := fileConfig["mcp"]; exists {
		return adapterResult{}, validationError(
			"harnesses."+harness.InstanceID+".config.file.mcp",
			"is adapter-owned; use MCPServer attachments",
		)
	}
	if _, exists := config["serverPassword"]; exists {
		return adapterResult{}, validationError(
			"harnesses."+harness.InstanceID+".config.serverPassword",
			"serverPassword is sensitive; use a Harness environment Secret reference",
		)
	}
	configPath := managedHarnessPath(harness.InstanceID, "opencode") + "/opencode.jsonc"
	instanceRoot := "/data/harnesses/" + harness.InstanceID
	content, environment, ownedPaths, err := renderOpenCodeMCP(namespace, harness.InstanceID, fileConfig, mcpServers)
	if err != nil {
		return adapterResult{}, err
	}
	environment = append(environment,
		ProviderEnvironment{Name: "OPENCODE_CONFIG", Value: configPath},
		ProviderEnvironment{Name: "XDG_CONFIG_HOME", Value: instanceRoot},
		ProviderEnvironment{Name: "XDG_DATA_HOME", Value: instanceRoot + "/share"},
		ProviderEnvironment{Name: "XDG_STATE_HOME", Value: instanceRoot + "/state"},
	)
	result := adapterResult{config: config, environment: environment}
	dialect, _ := adapter.extensionDialect(harness.InstanceID)
	result.extensions, result.warnings, err = renderExtensions(namespace, harness, dialect)
	if err != nil {
		return adapterResult{}, err
	}
	if content != "" {
		result.files = []FileTarget{{
			InstanceID: harness.InstanceID,
			Path:       configPath,
			Mode:       WriteModeMerge,
			Format:     FileFormatJSON,
			Content:    content,
			OwnedPaths: ownedPaths,
			Apply:      disruptiveFileApplyPolicy(),
		}}
	}
	return result, nil
}

func takeAdapterFileConfig(config map[string]any, fieldPath string) (map[string]any, error) {
	value, exists := config["file"]
	if !exists {
		return map[string]any{}, nil
	}
	delete(config, "file")
	object, ok := value.(map[string]any)
	if !ok {
		return nil, validationError(fieldPath, "must be a JSON object")
	}
	return cloneObject(object), nil
}

func (adapter alphaAdapter) supportLevel() SupportLevel {
	return SupportLevelAlpha
}

func (alphaAdapter) programsMCPServers() bool {
	return false
}

func (alphaAdapter) extensionDialect(string) (extensionDialect, bool) {
	return extensionDialect{}, false
}

func (adapter alphaAdapter) render(
	namespace string,
	harness Harness,
	config map[string]any,
	_ []normalizedMCPServer,
) (adapterResult, error) {
	if err := validateExtensions(namespace, harness.Extensions); err != nil {
		return adapterResult{}, err
	}
	return adapterResult{
		config: cloneObject(config),
		warnings: []Issue{{
			Code:       IssueAlphaDialect,
			InstanceID: harness.InstanceID,
			Message:    fmt.Sprintf("the %s adapter is alpha; MCP and Extension dialects are not programmed", adapter.driver),
		}},
	}, nil
}

func managedHarnessPath(instanceID, driverDirectory string) string {
	return "/data/harnesses/" + instanceID + "/" + driverDirectory
}

func setManagedHomePath(config map[string]any, expected, instanceID string) error {
	if value, exists := config["homePath"]; exists {
		path, ok := value.(string)
		if !ok || path != expected {
			return validationError(
				"harnesses."+instanceID+".config.homePath",
				"must equal the adapter-managed path "+expected,
			)
		}
	}
	config["homePath"] = expected
	return nil
}

func validateOptionalManagedPath(config map[string]any, key, instanceID string) error {
	value, exists := config[key]
	if !exists {
		return nil
	}
	path, ok := value.(string)
	prefix := "/data/harnesses/" + instanceID + "/"
	if !ok || pathpkg.Clean(path) != path || !strings.HasPrefix(path, prefix) || path == prefix {
		return validationError(
			"harnesses."+instanceID+".config."+key,
			"must stay under "+prefix,
		)
	}
	return nil
}

func cloneObject(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = cloneJSONValue(value)
	}
	return result
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneObject(typed)
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = cloneJSONValue(typed[index])
		}
		return result
	default:
		return typed
	}
}

func disruptiveFileApplyPolicy() ApplyPolicy {
	return ApplyPolicy{
		Class:     ChangeClassDisruptive,
		When:      ApplyWhenIdle,
		Mechanism: ReloadProviderRebuild,
	}
}
