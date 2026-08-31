package apply

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/janpuc/t3-code-operator/internal/render"
)

type materializedManifest struct {
	settings                ManagedSettings
	files                   []render.FileTarget
	secrets                 map[render.SecretReference]SecretValue
	materializationRevision string
}

func cloneManagedSettings(settings ManagedSettings) ManagedSettings {
	result := ManagedSettings{
		EnableProviderUpdateChecks: settings.EnableProviderUpdateChecks,
		ProviderInstances:          make(map[string]ProviderInstance, len(settings.ProviderInstances)),
	}
	for instanceID, instance := range settings.ProviderInstances {
		instance.Environment = append([]ProviderEnvironment(nil), instance.Environment...)
		instance.Config = append([]byte(nil), instance.Config...)
		result.ProviderInstances[instanceID] = instance
	}
	return result
}

func materializeManifest(
	ctx context.Context,
	resolver SecretResolver,
	manifest render.Manifest,
) (materializedManifest, error) {
	references := collectSecretReferences(manifest)
	values := make(map[render.SecretReference]SecretValue, len(references))
	for _, reference := range references {
		value, err := resolver.Resolve(ctx, reference)
		if err != nil {
			return materializedManifest{}, fmt.Errorf(
				"resolve Secret %s/%s key %s: %w",
				reference.Namespace,
				reference.Name,
				reference.Key,
				err,
			)
		}
		if value.Version == "" {
			return materializedManifest{}, fmt.Errorf(
				"resolve Secret %s/%s key %s: resource version is empty",
				reference.Namespace,
				reference.Name,
				reference.Key,
			)
		}
		values[reference] = value
	}

	providers := make(map[string]ProviderInstance, len(manifest.ProviderInstances))
	for instanceID, desired := range manifest.ProviderInstances {
		environment := make([]ProviderEnvironment, 0, len(desired.Environment))
		for _, variable := range desired.Environment {
			materialized := ProviderEnvironment{
				Name:      variable.Name,
				Value:     variable.Value,
				Sensitive: variable.Sensitive,
			}
			if variable.ValueFrom != nil {
				value, exists := values[*variable.ValueFrom]
				if !exists {
					return materializedManifest{}, fmt.Errorf("provider %s has an unresolved Secret reference", instanceID)
				}
				materialized.Value = variable.SecretPrefix + value.Value
				materialized.Sensitive = true
			}
			environment = append(environment, materialized)
		}
		sort.Slice(environment, func(i, j int) bool { return environment[i].Name < environment[j].Name })
		providers[instanceID] = ProviderInstance{
			Driver:      desired.Driver,
			DisplayName: desired.DisplayName,
			AccentColor: desired.AccentColor,
			Enabled:     desired.Enabled,
			Environment: environment,
			Config:      append([]byte(nil), desired.Config...),
		}
	}
	files, err := materializeFileTargets(manifest.Files, values)
	if err != nil {
		return materializedManifest{}, err
	}

	return materializedManifest{
		settings: ManagedSettings{
			EnableProviderUpdateChecks: manifest.ServerSettings.EnableProviderUpdateChecks,
			ProviderInstances:          providers,
		},
		files:                   files,
		secrets:                 values,
		materializationRevision: materializationRevision(manifest.DesiredRevision, references, values),
	}, nil
}

func materializeFileTargets(
	targets []render.FileTarget,
	values map[render.SecretReference]SecretValue,
) ([]render.FileTarget, error) {
	result := make([]render.FileTarget, 0, len(targets))
	for _, target := range targets {
		materialized := target
		materialized.OwnedPaths = append([]string(nil), target.OwnedPaths...)
		materialized.Values = append([]render.FileValueSource(nil), target.Values...)
		if target.SecretContent != nil {
			value, exists := values[target.SecretContent.ValueFrom]
			if !exists {
				return nil, fmt.Errorf("file %s has an unresolved Secret reference", target.Path)
			}
			transformed, err := transformSecretValue(value.Value, target.SecretContent.Transform)
			if err != nil {
				return nil, fmt.Errorf("materialize secret content for %s: %w", target.Path, err)
			}
			materialized.Content = target.SecretContent.Prefix + transformed + target.SecretContent.Suffix
			materialized.SecretContent = nil
		}
		if len(target.Values) != 0 {
			object, err := decodeManagedObject(target.Format, []byte(target.Content))
			if err != nil {
				return nil, fmt.Errorf("decode secret-backed file %s: %w", target.Path, err)
			}
			for _, source := range target.Values {
				value, exists := values[source.ValueFrom]
				if !exists {
					return nil, fmt.Errorf("file %s has an unresolved Secret reference", target.Path)
				}
				transformed, err := transformSecretValue(value.Value, source.Transform)
				if err != nil {
					return nil, fmt.Errorf("materialize secret value for %s: %w", target.Path, err)
				}
				if err := setJSONPointer(object, source.Path, transformed); err != nil {
					return nil, fmt.Errorf("materialize secret value for %s: %w", target.Path, err)
				}
			}
			encoded, err := encodeManagedObject(target.Format, object)
			if err != nil {
				return nil, fmt.Errorf("encode secret-backed file %s: %w", target.Path, err)
			}
			materialized.Content = string(encoded)
			materialized.Values = nil
		}
		result = append(result, materialized)
	}
	return result, nil
}

func transformSecretValue(value string, transform render.SecretTransform) (string, error) {
	switch transform {
	case render.SecretTransformNone:
		return value, nil
	case render.SecretTransformTrimSpace:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return "", fmt.Errorf("Secret value is empty")
		}
		return trimmed, nil
	case render.SecretTransformOpenSSHPrivateKey:
		return normalizeOpenSSHPrivateKey(value)
	default:
		return "", fmt.Errorf("unsupported Secret transform %q", transform)
	}
}

func normalizeOpenSSHPrivateKey(value string) (string, error) {
	const (
		header = "-----BEGIN OPENSSH PRIVATE KEY-----"
		footer = "-----END OPENSSH PRIVATE KEY-----"
		width  = 70
	)
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, header) || !strings.HasSuffix(trimmed, footer) {
		return "", fmt.Errorf("Secret value is not an OpenSSH private key")
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, header), footer))
	body = strings.Join(strings.Fields(body), "")
	decoded, err := base64.StdEncoding.DecodeString(body)
	if err != nil || !bytes.HasPrefix(decoded, []byte("openssh-key-v1\x00")) {
		return "", fmt.Errorf("Secret value is not a valid OpenSSH private key")
	}
	var result strings.Builder
	result.WriteString(header)
	result.WriteByte('\n')
	for len(body) > width {
		result.WriteString(body[:width])
		result.WriteByte('\n')
		body = body[width:]
	}
	result.WriteString(body)
	result.WriteByte('\n')
	result.WriteString(footer)
	result.WriteByte('\n')
	return result.String(), nil
}

func collectSecretReferences(manifest render.Manifest) []render.SecretReference {
	references := make(map[render.SecretReference]struct{})
	for _, instance := range manifest.ProviderInstances {
		for _, variable := range instance.Environment {
			if variable.ValueFrom != nil {
				references[*variable.ValueFrom] = struct{}{}
			}
		}
	}
	for _, activation := range manifest.Extensions {
		source := activation.Source
		for _, reference := range []*render.SecretReference{
			secretReferenceFromGit(source.Git),
			secretReferenceFromOCI(source.OCI),
			secretReferenceFromMarketplace(source.Marketplace),
			secretReferenceFromRelease(source.GitHubRelease),
		} {
			if reference != nil {
				references[*reference] = struct{}{}
			}
		}
	}
	for _, target := range manifest.Files {
		if target.SecretContent != nil {
			references[target.SecretContent.ValueFrom] = struct{}{}
		}
		for _, source := range target.Values {
			references[source.ValueFrom] = struct{}{}
		}
	}
	result := make([]render.SecretReference, 0, len(references))
	for reference := range references {
		result = append(result, reference)
	}
	sort.Slice(result, func(i, j int) bool {
		left := result[i]
		right := result[j]
		if left.Namespace != right.Namespace {
			return left.Namespace < right.Namespace
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.Key < right.Key
	})
	return result
}

func ReferencedSecrets(manifest render.Manifest) []render.SecretReference {
	return collectSecretReferences(manifest)
}

func materializationRevision(
	desiredRevision string,
	references []render.SecretReference,
	values map[render.SecretReference]SecretValue,
) string {
	var identity strings.Builder
	identity.WriteString(desiredRevision)
	for _, reference := range references {
		identity.WriteByte('\x00')
		identity.WriteString(reference.Namespace)
		identity.WriteByte('\x00')
		identity.WriteString(reference.Name)
		identity.WriteByte('\x00')
		identity.WriteString(reference.Key)
		identity.WriteByte('\x00')
		identity.WriteString(values[reference].Version)
	}
	digest := sha256.Sum256([]byte(identity.String()))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func secretReferenceFromGit(source *render.GitExtensionSource) *render.SecretReference {
	if source == nil {
		return nil
	}
	return source.CredentialSecretRef
}

func secretReferenceFromOCI(source *render.OCIExtensionSource) *render.SecretReference {
	if source == nil {
		return nil
	}
	return source.CredentialSecretRef
}

func secretReferenceFromMarketplace(source *render.MarketplaceExtensionSource) *render.SecretReference {
	if source == nil {
		return nil
	}
	return source.CredentialSecretRef
}

func secretReferenceFromRelease(source *render.GitHubReleaseExtensionSource) *render.SecretReference {
	if source == nil {
		return nil
	}
	return source.CredentialSecretRef
}
