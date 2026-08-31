package render

import (
	"fmt"
	"strings"
	"unicode"
)

var inlineSecretFieldNames = map[string]struct{}{
	"accesstoken":    {},
	"apikey":         {},
	"authtoken":      {},
	"bearertoken":    {},
	"clientsecret":   {},
	"password":       {},
	"privatekey":     {},
	"secret":         {},
	"serverpassword": {},
	"token":          {},
}

var sensitiveHeaderNames = map[string]struct{}{
	"api-key":             {},
	"authorization":       {},
	"cookie":              {},
	"proxy-authorization": {},
	"x-access-token":      {},
	"x-api-key":           {},
	"x-auth-token":        {},
	"x-github-token":      {},
}

func rejectInlineSecretFields(value any, fieldPath string) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if _, sensitive := inlineSecretFieldNames[normalizeSecretName(key)]; sensitive {
				if reference, ok := child.(string); !ok || !isEnvironmentReference(reference) {
					return validationError(fieldPath+"."+key, "a known secret field must use an environment Secret reference")
				}
			}
			if err := rejectInlineSecretFields(child, fieldPath+"."+key); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := rejectInlineSecretFields(child, fmt.Sprintf("%s[%d]", fieldPath, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func isEnvironmentReference(value string) bool {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		return environmentPattern.MatchString(strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}"))
	}
	if strings.HasPrefix(value, "{env:") && strings.HasSuffix(value, "}") {
		return environmentPattern.MatchString(strings.TrimSuffix(strings.TrimPrefix(value, "{env:"), "}"))
	}
	return false
}

func isSensitiveEnvironmentName(name string) bool {
	normalized := strings.ToUpper(name)
	for _, suffix := range []string{
		"API_KEY",
		"ACCESS_KEY",
		"AUTH_TOKEN",
		"PASSWORD",
		"PRIVATE_KEY",
		"SECRET",
		"TOKEN",
	} {
		if normalized == suffix || strings.HasSuffix(normalized, "_"+suffix) {
			return true
		}
	}
	return false
}

func isSensitiveHeaderName(name string) bool {
	_, sensitive := sensitiveHeaderNames[strings.ToLower(name)]
	return sensitive
}

func normalizeSecretName(name string) string {
	var result strings.Builder
	for _, character := range name {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			result.WriteRune(unicode.ToLower(character))
		}
	}
	return result.String()
}
