package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

var bareTOMLKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func renderClaudeMCP(
	namespace string,
	instanceID string,
	servers []normalizedMCPServer,
) (string, []ProviderEnvironment, []string, error) {
	if len(servers) == 0 {
		return "", nil, nil, nil
	}
	entries := make(map[string]any, len(servers))
	environment := make([]ProviderEnvironment, 0)
	for _, server := range servers {
		entry := cloneObject(server.config)
		if err := rejectReservedMCPFields(instanceID, server, entry, "type", "headers", "env"); err != nil {
			return "", nil, nil, err
		}
		switch server.transport {
		case "http":
			entry["type"] = "http"
			headers, bindings := renderInterpolatedHeaders(namespace, instanceID, server, "${", "}")
			if len(headers) != 0 {
				entry["headers"] = headers
			}
			environment = append(environment, bindings...)
		case "stdio":
			entry["type"] = "stdio"
			values, bindings := renderInterpolatedStdioEnvironment(namespace, instanceID, server, "${", "}")
			if len(values) != 0 {
				entry["env"] = values
			}
			environment = append(environment, bindings...)
		}
		entries[server.name] = entry
	}
	root := map[string]any{"mcpServers": entries}
	raw, err := canonicalJSON(root)
	if err != nil {
		return "", nil, nil, fmt.Errorf("render Claude config: %w", err)
	}
	return string(raw), environment, []string{"/mcpServers"}, nil
}

func renderJSONFileConfig(fileConfig map[string]any, description string) (string, []string, error) {
	if len(fileConfig) == 0 {
		return "", nil, nil
	}
	raw, err := canonicalJSON(fileConfig)
	if err != nil {
		return "", nil, fmt.Errorf("render %s: %w", description, err)
	}
	ownedPaths := make([]string, 0, len(fileConfig))
	for _, key := range sortedMapKeys(fileConfig) {
		ownedPaths = append(ownedPaths, "/"+escapeJSONPointerToken(key))
	}
	return string(raw), ownedPaths, nil
}

func renderOpenCodeMCP(
	namespace string,
	instanceID string,
	fileConfig map[string]any,
	servers []normalizedMCPServer,
) (string, []ProviderEnvironment, []string, error) {
	if len(servers) == 0 && len(fileConfig) == 0 {
		return "", nil, nil, nil
	}
	entries := make(map[string]any, len(servers))
	environment := make([]ProviderEnvironment, 0)
	for _, server := range servers {
		entry := cloneObject(server.config)
		if err := rejectReservedMCPFields(instanceID, server, entry, "type", "headers", "environment"); err != nil {
			return "", nil, nil, err
		}
		switch server.transport {
		case "http":
			entry["type"] = "remote"
			headers, bindings := renderInterpolatedHeaders(namespace, instanceID, server, "{env:", "}")
			if len(headers) != 0 {
				entry["headers"] = headers
			}
			environment = append(environment, bindings...)
		case "stdio":
			command := []any{entry["command"]}
			if args, exists := entry["args"].([]any); exists {
				command = append(command, args...)
			}
			delete(entry, "args")
			entry["command"] = command
			entry["type"] = "local"
			values, bindings := renderInterpolatedStdioEnvironment(namespace, instanceID, server, "{env:", "}")
			if len(values) != 0 {
				entry["environment"] = values
			}
			environment = append(environment, bindings...)
		}
		entries[server.name] = entry
	}
	root := cloneObject(fileConfig)
	if len(entries) != 0 {
		root["mcp"] = entries
	}
	raw, err := canonicalJSON(root)
	if err != nil {
		return "", nil, nil, fmt.Errorf("render OpenCode config: %w", err)
	}
	ownedPaths := make([]string, 0, len(root))
	for _, key := range sortedMapKeys(root) {
		ownedPaths = append(ownedPaths, "/"+escapeJSONPointerToken(key))
	}
	return string(raw), environment, ownedPaths, nil
}

func renderCodexMCP(
	namespace string,
	instanceID string,
	fileConfig map[string]any,
	servers []normalizedMCPServer,
) (string, []ProviderEnvironment, []string, error) {
	if len(servers) == 0 && len(fileConfig) == 0 {
		return "", nil, nil, nil
	}
	var output strings.Builder
	environment := make([]ProviderEnvironment, 0)
	for _, key := range sortedMapKeys(fileConfig) {
		value, err := tomlValue(fileConfig[key])
		if err != nil {
			return "", nil, nil, validationError("harnesses."+instanceID+".config.file."+key, err.Error())
		}
		output.WriteString(tomlKey(key))
		output.WriteString(" = ")
		output.WriteString(value)
		output.WriteByte('\n')
	}
	for serverIndex, server := range servers {
		if err := rejectReservedMCPFields(
			instanceID,
			server,
			server.config,
			"bearer_token_env_var",
			"env",
			"env_http_headers",
			"env_vars",
			"http_headers",
		); err != nil {
			return "", nil, nil, err
		}
		if serverIndex != 0 || len(fileConfig) != 0 {
			output.WriteByte('\n')
		}
		table := "mcp_servers." + tomlKey(server.name)
		output.WriteString("[")
		output.WriteString(table)
		output.WriteString("]\n")

		keys := sortedMapKeys(server.config)
		for _, key := range keys {
			value, err := tomlValue(server.config[key])
			if err != nil {
				return "", nil, nil, validationError(
					"harnesses."+instanceID+".mcpServers."+server.name+".config."+key,
					err.Error(),
				)
			}
			output.WriteString(tomlKey(key))
			output.WriteString(" = ")
			output.WriteString(value)
			output.WriteByte('\n')
		}

		staticHeaders := make(map[string]string)
		environmentHeaders := make(map[string]string)
		for _, header := range server.headers {
			if header.Value != nil {
				staticHeaders[header.Name] = header.Prefix + *header.Value
				continue
			}
			name := internalMCPEnvironmentName(namespace, instanceID, server.name, "header-"+strings.ToLower(header.Name))
			binding := ProviderEnvironment{
				Name:      name,
				ValueFrom: cloneSecretReference(header.ValueFrom),
				Sensitive: true,
			}
			if strings.EqualFold(header.Name, "Authorization") && header.Prefix == "Bearer " {
				output.WriteString("bearer_token_env_var = ")
				output.WriteString(strconv.Quote(name))
				output.WriteByte('\n')
			} else {
				binding.SecretPrefix = header.Prefix
				environmentHeaders[header.Name] = name
			}
			environment = append(environment, binding)
		}

		literalEnvironment := make(map[string]string)
		forwardEnvironment := make([]string, 0)
		for _, variable := range server.environment {
			if variable.Value != nil {
				literalEnvironment[variable.Name] = *variable.Value
				continue
			}
			forwardEnvironment = append(forwardEnvironment, variable.Name)
			environment = append(environment, ProviderEnvironment{
				Name:      variable.Name,
				ValueFrom: cloneSecretReference(variable.ValueFrom),
				Sensitive: true,
			})
		}
		if len(forwardEnvironment) != 0 {
			sort.Strings(forwardEnvironment)
			value, _ := tomlValue(stringSliceToAny(forwardEnvironment))
			output.WriteString("env_vars = ")
			output.WriteString(value)
			output.WriteByte('\n')
		}
		writeTOMLStringTable(&output, table+".http_headers", staticHeaders)
		writeTOMLStringTable(&output, table+".env_http_headers", environmentHeaders)
		writeTOMLStringTable(&output, table+".env", literalEnvironment)
	}
	ownedPaths := make([]string, 0, len(fileConfig)+1)
	for _, key := range sortedMapKeys(fileConfig) {
		ownedPaths = append(ownedPaths, "/"+escapeJSONPointerToken(key))
	}
	if len(servers) != 0 {
		ownedPaths = append(ownedPaths, "/mcp_servers")
	}
	sort.Strings(ownedPaths)
	return output.String(), environment, ownedPaths, nil
}

func renderInterpolatedHeaders(
	namespace string,
	instanceID string,
	server normalizedMCPServer,
	open string,
	close string,
) (map[string]string, []ProviderEnvironment) {
	headers := make(map[string]string, len(server.headers))
	environment := make([]ProviderEnvironment, 0, len(server.headers))
	for _, header := range server.headers {
		if header.Value != nil {
			headers[header.Name] = header.Prefix + *header.Value
			continue
		}
		name := internalMCPEnvironmentName(namespace, instanceID, server.name, "header-"+strings.ToLower(header.Name))
		headers[header.Name] = header.Prefix + open + name + close
		environment = append(environment, ProviderEnvironment{
			Name:      name,
			ValueFrom: cloneSecretReference(header.ValueFrom),
			Sensitive: true,
		})
	}
	return headers, environment
}

func renderInterpolatedStdioEnvironment(
	namespace string,
	instanceID string,
	server normalizedMCPServer,
	open string,
	close string,
) (map[string]string, []ProviderEnvironment) {
	values := make(map[string]string, len(server.environment))
	environment := make([]ProviderEnvironment, 0, len(server.environment))
	for _, variable := range server.environment {
		if variable.Value != nil {
			values[variable.Name] = *variable.Value
			continue
		}
		name := internalMCPEnvironmentName(namespace, instanceID, server.name, "env-"+variable.Name)
		values[variable.Name] = open + name + close
		environment = append(environment, ProviderEnvironment{
			Name:      name,
			ValueFrom: cloneSecretReference(variable.ValueFrom),
			Sensitive: true,
		})
	}
	return values, environment
}

func internalMCPEnvironmentName(namespace, instanceID, serverName, use string) string {
	identity := namespace + "\x00" + instanceID + "\x00" + serverName + "\x00" + use
	digest := sha256.Sum256([]byte(identity))
	readable := sanitizeEnvironmentPart(serverName)
	if len(readable) > 24 {
		readable = readable[:24]
	}
	return "T3CODE_MCP_" + readable + "_" + strings.ToUpper(hex.EncodeToString(digest[:8]))
}

func sanitizeEnvironmentPart(value string) string {
	var result strings.Builder
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			result.WriteRune(unicode.ToUpper(character))
		} else {
			result.WriteByte('_')
		}
	}
	if result.Len() == 0 {
		return "SERVER"
	}
	return result.String()
}

func rejectReservedMCPFields(instanceID string, server normalizedMCPServer, config map[string]any, fields ...string) error {
	for _, field := range fields {
		if _, exists := config[field]; exists {
			return validationError(
				"harnesses."+instanceID+".mcpServers."+server.name+".config."+field,
				"is adapter-owned; use the typed MCPServer field",
			)
		}
	}
	return nil
}

func writeTOMLStringTable(output *strings.Builder, table string, values map[string]string) {
	if len(values) == 0 {
		return
	}
	output.WriteByte('\n')
	output.WriteString("[")
	output.WriteString(table)
	output.WriteString("]\n")
	for _, key := range sortedMapKeys(values) {
		output.WriteString(tomlKey(key))
		output.WriteString(" = ")
		output.WriteString(strconv.Quote(values[key]))
		output.WriteByte('\n')
	}
}

func tomlKey(key string) string {
	if bareTOMLKeyPattern.MatchString(key) {
		return key
	}
	return strconv.Quote(key)
}

func tomlValue(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return strconv.Quote(typed), nil
	case bool:
		return strconv.FormatBool(typed), nil
	case json.Number:
		if _, err := typed.Float64(); err != nil {
			return "", fmt.Errorf("contains an invalid number")
		}
		return typed.String(), nil
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64), nil
	case []any:
		values := make([]string, len(typed))
		for index, item := range typed {
			value, err := tomlValue(item)
			if err != nil {
				return "", err
			}
			values[index] = value
		}
		return "[" + strings.Join(values, ", ") + "]", nil
	case map[string]any:
		values := make([]string, 0, len(typed))
		for _, key := range sortedMapKeys(typed) {
			value, err := tomlValue(typed[key])
			if err != nil {
				return "", err
			}
			values = append(values, tomlKey(key)+" = "+value)
		}
		return "{ " + strings.Join(values, ", ") + " }", nil
	case nil:
		return "", fmt.Errorf("contains null, which TOML cannot represent")
	default:
		return "", fmt.Errorf("contains unsupported JSON type %T", value)
	}
}

func sortedMapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func stringSliceToAny(values []string) []any {
	result := make([]any, len(values))
	for index := range values {
		result[index] = values[index]
	}
	return result
}

func escapeJSONPointerToken(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}
