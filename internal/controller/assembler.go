package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/apply"
	"github.com/janpuc/t3-code-operator/internal/render"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ToolResolver interface {
	Resolve(context.Context, []t3v1alpha1.ToolSpec) ([]render.ResolvedTool, error)
}

type Assembly struct {
	Manifest    render.Manifest
	SecretNames []string
}

type Assembler struct {
	Reader client.Reader
	Tools  ToolResolver
}

func (assembler *Assembler) Assemble(
	ctx context.Context,
	workstation *t3v1alpha1.Workstation,
) (Assembly, error) {
	if assembler.Reader == nil {
		return Assembly{}, errors.New("Kubernetes reader is required")
	}
	if workstation == nil {
		return Assembly{}, errors.New("Workstation is required")
	}

	tools := []render.ResolvedTool(nil)
	if len(workstation.Spec.Tools) != 0 {
		if assembler.Tools == nil {
			return Assembly{}, errors.New("tool resolver is required")
		}
		var err error
		tools, err = assembler.Tools.Resolve(ctx, append([]t3v1alpha1.ToolSpec(nil), workstation.Spec.Tools...))
		if err != nil {
			return Assembly{}, err
		}
	}

	var harnessList t3v1alpha1.HarnessList
	if err := assembler.Reader.List(ctx, &harnessList, client.InNamespace(workstation.Namespace)); err != nil {
		return Assembly{}, err
	}
	var extensionList t3v1alpha1.ExtensionList
	if err := assembler.Reader.List(ctx, &extensionList, client.InNamespace(workstation.Namespace)); err != nil {
		return Assembly{}, err
	}
	var serverList t3v1alpha1.MCPServerList
	if err := assembler.Reader.List(ctx, &serverList, client.InNamespace(workstation.Namespace)); err != nil {
		return Assembly{}, err
	}

	type attachedProvider struct {
		refName  string
		policy   t3v1alpha1.AttachmentPolicy
		resolved render.Harness
	}
	providers := make([]attachedProvider, 0, len(workstation.Spec.Providers)+len(harnessList.Items))
	for _, name := range sortedProviderNames(workstation.Spec.Providers) {
		spec := workstation.Spec.Providers[name]
		resolved, err := convertProvider(workstation.Namespace, name, spec)
		if err != nil {
			return Assembly{}, err
		}
		providers = append(providers, attachedProvider{refName: name, policy: spec.AttachmentPolicy, resolved: resolved})
	}
	for index := range harnessList.Items {
		harness := &harnessList.Items[index]
		if !harness.DeletionTimestamp.IsZero() || !harnessAttachesToWorkstation(harness, workstation.Name) {
			continue
		}
		resolved, err := convertHarness(workstation.Namespace, harness)
		if err != nil {
			return Assembly{}, err
		}
		providers = append(providers, attachedProvider{refName: harness.Name, policy: harness.Spec.AttachmentPolicy, resolved: resolved})
	}

	harnesses := make([]render.Harness, 0, len(providers))
	for _, provider := range providers {
		resolved := provider.resolved
		for index := range extensionList.Items {
			extension := &extensionList.Items[index]
			if !extension.DeletionTimestamp.IsZero() {
				continue
			}
			converted := convertExtension(workstation.Namespace, extension)
			programs := func(driver string) bool {
				return render.ProgramsExtensionSource(driver, converted.Source.Type)
			}
			if attachmentSelects(extension.Spec.HarnessRefs, provider.refName, resolved.Driver, provider.policy.Extensions, programs) {
				resolved.Extensions = append(resolved.Extensions, converted)
			}
		}
		for index := range serverList.Items {
			server := &serverList.Items[index]
			if !server.DeletionTimestamp.IsZero() {
				continue
			}
			if attachmentSelects(server.Spec.HarnessRefs, provider.refName, resolved.Driver, provider.policy.MCPServers, render.ProgramsMCPServers) {
				resolved.MCPServers = append(resolved.MCPServers, convertMCPServer(workstation.Namespace, server))
			}
		}
		harnesses = append(harnesses, resolved)
	}

	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace:   workstation.Namespace,
		Name:        workstation.Name,
		UID:         string(workstation.UID),
		MachineInfo: effectiveMachineInfo(workstation),
		Git:         convertGitConfiguration(workstation.Namespace, workstation.Spec.Git),
		Tools:       tools,
		Harnesses:   harnesses,
	})
	if err != nil {
		return Assembly{}, err
	}
	secretSet := make(map[string]struct{})
	for _, reference := range apply.ReferencedSecrets(manifest) {
		if reference.Namespace == workstation.Namespace {
			secretSet[reference.Name] = struct{}{}
		}
	}
	secretNames := make([]string, 0, len(secretSet))
	for name := range secretSet {
		secretNames = append(secretNames, name)
	}
	sort.Strings(secretNames)
	return Assembly{Manifest: manifest, SecretNames: secretNames}, nil
}

func effectiveMachineInfo(workstation *t3v1alpha1.Workstation) *render.MachineInfo {
	if workstation.Spec.MachineInfo == nil {
		return &render.MachineInfo{PrettyHostname: workstation.Name}
	}
	return &render.MachineInfo{PrettyHostname: workstation.Spec.MachineInfo.PrettyHostname}
}

func convertGitConfiguration(namespace string, input *t3v1alpha1.GitIdentity) *render.GitConfiguration {
	if input == nil {
		return nil
	}
	result := &render.GitConfiguration{
		UserName:            input.UserName,
		UserEmail:           input.UserEmail,
		GitHubUser:          input.GitHubUser,
		CredentialSecretRef: convertOptionalSecretReference(namespace, input.CredentialSecretRef),
	}
	if input.SigningKeySecretRef != nil {
		result.SigningKeySecretRef = &render.GitSigningKeyReference{
			PrivateKey: &render.SecretReference{
				Namespace: namespace,
				Name:      input.SigningKeySecretRef.Name,
				Key:       input.SigningKeySecretRef.PrivateKeyKey,
			},
			PublicKey: &render.SecretReference{
				Namespace: namespace,
				Name:      input.SigningKeySecretRef.Name,
				Key:       input.SigningKeySecretRef.PublicKeyKey,
			},
		}
	}
	return result
}

func referencesName(references []t3v1alpha1.LocalObjectReference, name string) bool {
	for _, reference := range references {
		if reference.Name == name {
			return true
		}
	}
	return false
}

func attachmentsAllowed(mode t3v1alpha1.AttachmentPolicyMode) bool {
	return mode == "" || mode == t3v1alpha1.AttachmentPolicySameNamespace
}

func convertHarness(namespace string, input *t3v1alpha1.Harness) (render.Harness, error) {
	enabled := true
	if input.Spec.Enabled != nil {
		enabled = *input.Spec.Enabled
	}
	driver, err := harnessDriver(input)
	if err != nil {
		return render.Harness{}, err
	}
	return render.Harness{
		InstanceID:  harnessInstanceID(input),
		Driver:      driver,
		DisplayName: resolveDisplayName(input.Spec.DisplayName, driver),
		AccentColor: input.Spec.AccentColor,
		Enabled:     enabled,
		Environment: convertEnvironment(namespace, input.Spec.Environment),
		Config:      convertJSON(input.Spec.Config),
	}, nil
}

func convertEnvironment(namespace string, input []t3v1alpha1.EnvironmentVariable) []render.EnvironmentVariable {
	result := make([]render.EnvironmentVariable, 0, len(input))
	for _, variable := range input {
		converted := render.EnvironmentVariable{Name: variable.Name}
		if variable.Value != nil {
			value := *variable.Value
			converted.Value = &value
		}
		if variable.ValueFrom != nil {
			converted.ValueFrom = convertSecretReference(namespace, variable.ValueFrom.SecretKeyRef)
		}
		result = append(result, converted)
	}
	return result
}

func convertMCPServer(namespace string, input *t3v1alpha1.MCPServer) render.MCPServer {
	transport := input.Spec.Transport
	if transport == "" {
		transport = inferMCPTransport(input.Spec.Config)
	}
	result := render.MCPServer{
		Name:        input.Name,
		Transport:   transport,
		Config:      convertJSON(input.Spec.Config),
		Environment: convertEnvironment(namespace, input.Spec.Environment),
	}
	if input.Spec.BearerTokenSecretRef != nil {
		result.Headers = append(result.Headers, render.Header{
			Name:      "Authorization",
			Prefix:    "Bearer ",
			ValueFrom: convertSecretReference(namespace, *input.Spec.BearerTokenSecretRef),
		})
	}
	for _, header := range input.Spec.Headers {
		converted := render.Header{Name: header.Name, Prefix: header.Prefix}
		if header.Value != nil {
			value := *header.Value
			converted.Value = &value
		}
		if header.ValueFrom != nil {
			converted.ValueFrom = convertSecretReference(namespace, header.ValueFrom.SecretKeyRef)
		}
		result.Headers = append(result.Headers, converted)
	}
	return result
}

func convertExtension(namespace string, input *t3v1alpha1.Extension) render.Extension {
	source := input.Spec.Source
	converted := render.ExtensionSource{
		Type:    render.ExtensionSourceType(source.Type),
		Include: append([]string(nil), source.Include...),
	}
	if source.Git != nil {
		converted.Git = &render.GitExtensionSource{
			URL:                 source.Git.URL,
			Commit:              source.Git.Commit,
			Path:                source.Git.Path,
			CredentialSecretRef: convertOptionalSecretReference(namespace, source.Git.CredentialSecretRef),
		}
	}
	if source.OCI != nil {
		converted.OCI = &render.OCIExtensionSource{
			Repository:          source.OCI.Repository,
			Digest:              source.OCI.Digest,
			CredentialSecretRef: convertOptionalSecretReference(namespace, source.OCI.CredentialSecretRef),
		}
	}
	if source.Marketplace != nil {
		converted.Marketplace = &render.MarketplaceExtensionSource{
			Marketplace:         source.Marketplace.Marketplace,
			Extension:           source.Marketplace.Extension,
			RepositoryURL:       source.Marketplace.RepositoryURL,
			Commit:              source.Marketplace.Commit,
			CredentialSecretRef: convertOptionalSecretReference(namespace, source.Marketplace.CredentialSecretRef),
		}
	}
	if source.GitHubRelease != nil {
		converted.GitHubRelease = &render.GitHubReleaseExtensionSource{
			Repository:          source.GitHubRelease.Repository,
			Tag:                 source.GitHubRelease.Tag,
			Asset:               source.GitHubRelease.Asset,
			SHA256:              source.GitHubRelease.SHA256,
			CredentialSecretRef: convertOptionalSecretReference(namespace, source.GitHubRelease.CredentialSecretRef),
		}
	}
	return render.Extension{Name: input.Name, Source: converted}
}

func convertSecretReference(namespace string, input t3v1alpha1.SecretKeyReference) *render.SecretReference {
	return &render.SecretReference{Namespace: namespace, Name: input.Name, Key: input.Key}
}

func convertOptionalSecretReference(
	namespace string,
	input *t3v1alpha1.SecretKeyReference,
) *render.SecretReference {
	if input == nil {
		return nil
	}
	return convertSecretReference(namespace, *input)
}

func convertJSON(input *apiextensionsv1.JSON) json.RawMessage {
	if input == nil {
		return nil
	}
	return append(json.RawMessage(nil), input.Raw...)
}
