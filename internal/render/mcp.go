package render

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

type normalizedMCPServer struct {
	name        string
	transport   string
	config      map[string]any
	headers     []Header
	environment []EnvironmentVariable
}

func normalizeMCPServers(namespace string, harness Harness) ([]normalizedMCPServer, error) {
	servers := append([]MCPServer(nil), harness.MCPServers...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })

	result := make([]normalizedMCPServer, 0, len(servers))
	names := make(map[string]struct{}, len(servers))
	for serverIndex, server := range servers {
		fieldPath := fmt.Sprintf("harnesses.%s.mcpServers[%d]", harness.InstanceID, serverIndex)
		if server.Name == "" {
			return nil, validationError(fieldPath+".name", "is required")
		}
		if _, exists := names[server.Name]; exists {
			return nil, validationError(fieldPath+".name", "duplicates MCP server "+server.Name)
		}
		names[server.Name] = struct{}{}

		config, err := decodeObject(server.Config, fieldPath+".config")
		if err != nil {
			return nil, err
		}
		if err := rejectInlineSecretFields(config, fieldPath+".config"); err != nil {
			return nil, err
		}
		if err := validateTransportConfig(server.Transport, config, fieldPath); err != nil {
			return nil, err
		}
		headers, err := normalizeHeaders(namespace, fieldPath, server.Headers)
		if err != nil {
			return nil, err
		}
		environment, err := normalizeMCPEnvironment(namespace, fieldPath, server.Environment)
		if err != nil {
			return nil, err
		}
		if server.Transport == "http" && len(environment) != 0 {
			return nil, validationError(fieldPath+".environment", "is valid only for stdio transport")
		}
		if server.Transport == "stdio" && len(headers) != 0 {
			return nil, validationError(fieldPath+".headers", "is valid only for http transport")
		}

		result = append(result, normalizedMCPServer{
			name:        server.Name,
			transport:   server.Transport,
			config:      config,
			headers:     headers,
			environment: environment,
		})
	}
	return result, nil
}

func validateTransportConfig(transport string, config map[string]any, fieldPath string) error {
	switch transport {
	case "http":
		rawURL, ok := config["url"].(string)
		if !ok || rawURL == "" {
			return validationError(fieldPath+".config.url", "is required and must be a string")
		}
		parsed, err := url.Parse(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return validationError(fieldPath+".config.url", "must be an absolute HTTP or HTTPS URL")
		}
		if parsed.User != nil {
			return validationError(fieldPath+".config.url", "must not contain user information")
		}
		if parsed.Fragment != "" {
			return validationError(fieldPath+".config.url", "must not contain a fragment")
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			return validationError(fieldPath+".config.url", "contains an invalid query")
		}
		for name := range query {
			if _, sensitive := inlineSecretFieldNames[normalizeSecretName(name)]; sensitive {
				return validationError(fieldPath+".config.url", "must not contain a sensitive query parameter")
			}
		}
	case "stdio":
		command, ok := config["command"].(string)
		if !ok || command == "" {
			return validationError(fieldPath+".config.command", "is required and must be a string")
		}
		if args, exists := config["args"]; exists {
			values, ok := args.([]any)
			if !ok {
				return validationError(fieldPath+".config.args", "must be an array of strings")
			}
			for _, value := range values {
				if _, ok := value.(string); !ok {
					return validationError(fieldPath+".config.args", "must be an array of strings")
				}
			}
		}
		if cwd, exists := config["cwd"]; exists {
			if _, ok := cwd.(string); !ok {
				return validationError(fieldPath+".config.cwd", "must be a string")
			}
		}
	default:
		return validationError(fieldPath+".transport", fmt.Sprintf("transport %q is not implemented", transport))
	}
	return nil
}

func normalizeHeaders(namespace, fieldPath string, headers []Header) ([]Header, error) {
	result := append([]Header(nil), headers...)
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	names := make(map[string]struct{}, len(result))
	for index, header := range result {
		path := fmt.Sprintf("%s.headers[%d]", fieldPath, index)
		if !headerPattern.MatchString(header.Name) {
			return nil, validationError(path+".name", "is not a valid HTTP header name")
		}
		name := strings.ToLower(header.Name)
		if _, exists := names[name]; exists {
			return nil, validationError(path+".name", "duplicates an HTTP header name case-insensitively")
		}
		names[name] = struct{}{}
		if len(header.Prefix) > 1024 || strings.ContainsAny(header.Prefix, "\x00\r\n") {
			return nil, validationError(path+".prefix", "must be a single-line value of at most 1024 bytes")
		}
		if (header.Value == nil) == (header.ValueFrom == nil) {
			return nil, validationError(path, "exactly one of value or valueFrom is required")
		}
		if header.Value != nil && isSensitiveHeaderName(header.Name) {
			return nil, validationError(path+".value", "a sensitive HTTP header must use valueFrom")
		}
		if header.Value != nil && strings.ContainsAny(*header.Value, "\x00\r\n") {
			return nil, validationError(path+".value", "must be a single-line value")
		}
		if header.ValueFrom != nil {
			if err := validateSecretReference(namespace, path+".valueFrom", *header.ValueFrom); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func normalizeMCPEnvironment(namespace, fieldPath string, environment []EnvironmentVariable) ([]EnvironmentVariable, error) {
	result := append([]EnvironmentVariable(nil), environment...)
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	names := make(map[string]struct{}, len(result))
	for index, variable := range result {
		path := fmt.Sprintf("%s.environment[%d]", fieldPath, index)
		if _, exists := names[variable.Name]; exists {
			return nil, validationError(path+".name", "duplicates environment variable "+variable.Name)
		}
		names[variable.Name] = struct{}{}
		if _, err := renderEnvironmentVariable(namespace, path, variable); err != nil {
			return nil, err
		}
	}
	return result, nil
}
