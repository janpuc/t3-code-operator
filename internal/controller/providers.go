package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/render"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const inlineProviderSeparator = "/"

type providerTarget struct {
	attachmentName string
	refName        string
	workstations   []string
	driver         string
	driverErr      error
	policy         t3v1alpha1.AttachmentPolicy
	harness        *t3v1alpha1.Harness
	workstation    *t3v1alpha1.Workstation
}

func (target providerTarget) inline() bool {
	return target.harness == nil
}

func inlineProviderAttachmentName(workstation, provider string) string {
	return boundedResourceName(workstation, "") + inlineProviderSeparator + provider
}

func sortedProviderNames(providers map[string]t3v1alpha1.ProviderSpec) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func resolveProviderDriver(name, driver string) (string, error) {
	if driver != "" {
		return driver, nil
	}
	builtIn, known := t3v1alpha1.BuiltInDriverForName(name)
	if !known {
		return "", fmt.Errorf("provider %q needs an explicit driver because it is not a built-in provider name", name)
	}
	return builtIn.Driver, nil
}

func resolveDisplayName(displayName, driver string) string {
	if displayName != "" {
		return displayName
	}
	return t3v1alpha1.DisplayNameForDriver(driver)
}

func harnessInstanceID(harness *t3v1alpha1.Harness) string {
	if harness.Spec.InstanceID != "" {
		return harness.Spec.InstanceID
	}
	return harness.Name
}

func harnessDriver(harness *t3v1alpha1.Harness) (string, error) {
	return resolveProviderDriver(harness.Name, harness.Spec.Driver)
}

func harnessAttachesToWorkstation(harness *t3v1alpha1.Harness, workstationName string) bool {
	return len(harness.Spec.WorkstationRefs) == 0 || referencesName(harness.Spec.WorkstationRefs, workstationName)
}

func attachmentSelects(
	references []t3v1alpha1.LocalObjectReference,
	refName string,
	driver string,
	mode t3v1alpha1.AttachmentPolicyMode,
	programs func(string) bool,
) bool {
	if !attachmentsAllowed(mode) {
		return false
	}
	if len(references) == 0 {
		return programs(driver)
	}
	return referencesName(references, refName)
}

func convertProvider(namespace, name string, spec t3v1alpha1.ProviderSpec) (render.Harness, error) {
	driver, err := resolveProviderDriver(name, spec.Driver)
	if err != nil {
		return render.Harness{}, err
	}
	config, err := providerConfig(spec.Config, spec.Models, "providers."+name)
	if err != nil {
		return render.Harness{}, err
	}
	return render.Harness{
		InstanceID:  name,
		Driver:      driver,
		DisplayName: resolveDisplayName(spec.DisplayName, driver),
		AccentColor: spec.AccentColor,
		Enabled:     spec.Enabled,
		Environment: convertEnvironment(namespace, spec.Environment),
		Config:      config,
	}, nil
}

func providerConfig(config *apiextensionsv1.JSON, models []string, fieldPath string) (json.RawMessage, error) {
	if len(models) == 0 {
		return convertJSON(config), nil
	}
	object := map[string]any{}
	if config != nil && len(config.Raw) != 0 {
		if err := json.Unmarshal(config.Raw, &object); err != nil || object == nil {
			return nil, fmt.Errorf("%s.config must be a JSON object", fieldPath)
		}
	}
	if _, exists := object["customModels"]; exists {
		return nil, fmt.Errorf("%s.models and %s.config.customModels are mutually exclusive", fieldPath, fieldPath)
	}
	object["customModels"] = append([]string(nil), models...)
	raw, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("%s.config: %w", fieldPath, err)
	}
	return raw, nil
}

func inferMCPTransport(config *apiextensionsv1.JSON) string {
	if config == nil || len(config.Raw) == 0 {
		return ""
	}
	object := map[string]any{}
	if err := json.Unmarshal(config.Raw, &object); err != nil {
		return ""
	}
	if _, exists := object["url"]; exists {
		return "http"
	}
	if _, exists := object["command"]; exists {
		return "stdio"
	}
	return ""
}

func listProviderTargets(ctx context.Context, reader client.Reader, namespace string) ([]providerTarget, error) {
	var workstationList t3v1alpha1.WorkstationList
	if err := reader.List(ctx, &workstationList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var harnessList t3v1alpha1.HarnessList
	if err := reader.List(ctx, &harnessList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	workstationNames := make([]string, 0, len(workstationList.Items))
	targets := make([]providerTarget, 0, len(harnessList.Items))
	for index := range workstationList.Items {
		workstation := &workstationList.Items[index]
		if !workstation.DeletionTimestamp.IsZero() {
			continue
		}
		workstationNames = append(workstationNames, workstation.Name)
		for _, name := range sortedProviderNames(workstation.Spec.Providers) {
			spec := workstation.Spec.Providers[name]
			driver, err := resolveProviderDriver(name, spec.Driver)
			targets = append(targets, providerTarget{
				attachmentName: inlineProviderAttachmentName(workstation.Name, name),
				refName:        name,
				workstations:   []string{workstation.Name},
				driver:         driver,
				driverErr:      err,
				policy:         spec.AttachmentPolicy,
				workstation:    workstation,
			})
		}
	}
	sort.Strings(workstationNames)
	for index := range harnessList.Items {
		harness := &harnessList.Items[index]
		if !harness.DeletionTimestamp.IsZero() {
			continue
		}
		names := make([]string, 0, len(workstationNames))
		if len(harness.Spec.WorkstationRefs) == 0 {
			names = append(names, workstationNames...)
		} else {
			for _, reference := range harness.Spec.WorkstationRefs {
				names = append(names, reference.Name)
			}
		}
		driver, err := harnessDriver(harness)
		targets = append(targets, providerTarget{
			attachmentName: harness.Name,
			refName:        harness.Name,
			workstations:   names,
			driver:         driver,
			driverErr:      err,
			policy:         harness.Spec.AttachmentPolicy,
			harness:        harness,
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].attachmentName < targets[j].attachmentName })
	return targets, nil
}

func selectProviderTargets(
	targets []providerTarget,
	references []t3v1alpha1.LocalObjectReference,
	extension bool,
	programs func(string) bool,
) ([]providerTarget, []string) {
	selected := make([]providerTarget, 0, len(targets))
	if len(references) == 0 {
		for _, target := range targets {
			mode := target.policy.MCPServers
			if extension {
				mode = target.policy.Extensions
			}
			if attachmentsAllowed(mode) && programs(target.driver) {
				selected = append(selected, target)
			}
		}
		return selected, nil
	}
	missing := make([]string, 0)
	for _, reference := range uniqueLocalReferences(references) {
		found := false
		for _, target := range targets {
			if target.refName == reference.Name {
				selected = append(selected, target)
				found = true
			}
		}
		if !found {
			missing = append(missing, reference.Name)
		}
	}
	return selected, missing
}

func workstationNamesForTargets(targets []providerTarget) []string {
	set := make(map[string]struct{})
	for _, target := range targets {
		for _, name := range target.workstations {
			set[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func localReferences(names []string) []t3v1alpha1.LocalObjectReference {
	references := make([]t3v1alpha1.LocalObjectReference, 0, len(names))
	for _, name := range names {
		references = append(references, t3v1alpha1.LocalObjectReference{Name: name})
	}
	return references
}
