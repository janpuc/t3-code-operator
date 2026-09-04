package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAssemblerExpandsInlineProvidersWithDefaults(t *testing.T) {
	scheme := controllerTestScheme(t)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	workstation.Spec.MachineInfo = nil
	token := t3v1alpha1.EnvironmentValueSource{SecretKeyRef: t3v1alpha1.SecretKeyReference{Name: "provider-token", Key: "token"}}
	workstation.Spec.Providers = map[string]t3v1alpha1.ProviderSpec{
		"claude":   {Enabled: true, Environment: []t3v1alpha1.EnvironmentVariable{{Name: "CLAUDE_CODE_OAUTH_TOKEN", ValueFrom: &token}}},
		"opencode": {Enabled: true, Models: []string{"litellm/minimax"}},
		"grok":     {Enabled: false},
		"review":   {Enabled: true, Driver: "codex", DisplayName: "Review"},
	}
	server := &t3v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "kubectl"},
		Spec: t3v1alpha1.MCPServerSpec{
			Config:               jsonObject(`{"url":"https://mcp.example.test/kubectl"}`),
			BearerTokenSecretRef: &t3v1alpha1.SecretKeyReference{Name: "gateway-token", Key: "token"},
		},
	}
	extension := &t3v1alpha1.Extension{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "skills"},
		Spec: t3v1alpha1.ExtensionSpec{Source: t3v1alpha1.ExtensionSource{
			Type: t3v1alpha1.ExtensionSourceGit,
			Git:  &t3v1alpha1.GitExtensionSource{URL: "https://example.test/skills.git", Commit: strings.Repeat("b", 40)},
		}},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workstation, server, extension).Build()

	assembly, err := (&Assembler{Reader: kube}).Assemble(context.Background(), workstation)
	if err != nil {
		t.Fatal(err)
	}
	manifest := assembly.Manifest
	if len(manifest.ProviderInstances) != 4 {
		t.Fatalf("unexpected provider instances: %#v", manifest.ProviderInstances)
	}
	claude := manifest.ProviderInstances["claude"]
	if claude.Driver != "claudeAgent" || claude.DisplayName != "Claude" || !claude.Enabled {
		t.Fatalf("claude defaults were not applied: %#v", claude)
	}
	if manifest.ProviderInstances["grok"].Enabled {
		t.Fatal("a provider with enabled=false rendered as enabled")
	}
	review := manifest.ProviderInstances["review"]
	if review.Driver != "codex" || review.DisplayName != "Review" {
		t.Fatalf("explicit driver and display name were not preserved: %#v", review)
	}
	var opencodeConfig map[string]any
	if err := json.Unmarshal(manifest.ProviderInstances["opencode"].Config, &opencodeConfig); err != nil {
		t.Fatal(err)
	}
	if models, _ := opencodeConfig["customModels"].([]any); len(models) != 1 || models[0] != "litellm/minimax" {
		t.Fatalf("models shorthand did not reach customModels: %#v", opencodeConfig)
	}

	attached := map[string]bool{}
	for _, file := range manifest.Files {
		if strings.Contains(file.Content, "mcp.example.test/kubectl") {
			attached[file.InstanceID] = true
			if !strings.Contains(file.Content, "Authorization") && !strings.Contains(file.Content, "bearer_token_env_var") {
				t.Fatalf("bearer shorthand did not render bearer auth for %s: %s", file.InstanceID, file.Content)
			}
		}
	}
	activated := map[string]bool{}
	for _, activation := range manifest.Extensions {
		activated[activation.InstanceID] = true
	}
	for _, instanceID := range []string{"claude", "opencode", "review"} {
		if !attached[instanceID] {
			t.Fatalf("MCP server was not attached to %s by default", instanceID)
		}
		if !activated[instanceID] {
			t.Fatalf("Extension was not attached to %s by default", instanceID)
		}
	}
	if attached["grok"] || activated["grok"] {
		t.Fatal("content attached to an alpha provider that cannot program it")
	}

	machineInfo := ""
	for _, file := range manifest.Files {
		if file.Path == "/data/t3-coded/machine-info" {
			machineInfo = file.Content
		}
	}
	if machineInfo != "PRETTY_HOSTNAME=\"primary\"\n" {
		t.Fatalf("the Workstation name did not become the environment label: %q", machineInfo)
	}
	for _, secret := range []string{"provider-token", "gateway-token"} {
		if !containsString(assembly.SecretNames, secret) {
			t.Fatalf("secret %s is missing from the assembly: %#v", secret, assembly.SecretNames)
		}
	}
}

func TestAssemblerRejectsUnresolvableProviders(t *testing.T) {
	scheme := controllerTestScheme(t)
	for name, providers := range map[string]map[string]t3v1alpha1.ProviderSpec{
		"needs an explicit driver": {"mystery": {Enabled: true}},
		"mutually exclusive":       {"codex": {Enabled: true, Models: []string{"a"}, Config: jsonObject(`{"customModels":["b"]}`)}},
	} {
		workstation := controllerTestWorkstation()
		workstation.Spec.Tools = nil
		workstation.Spec.Providers = providers
		kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workstation).Build()
		_, err := (&Assembler{Reader: kube}).Assemble(context.Background(), workstation)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("expected an error containing %q, got %v", name, err)
		}
	}
}

func TestAssemblerDefaultsHarnessIdentityAndAttachment(t *testing.T) {
	scheme := controllerTestScheme(t)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	harness := &t3v1alpha1.Harness{ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "claude"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workstation, harness).Build()

	assembly, err := (&Assembler{Reader: kube}).Assemble(context.Background(), workstation)
	if err != nil {
		t.Fatal(err)
	}
	instance, exists := assembly.Manifest.ProviderInstances["claude"]
	if !exists || instance.Driver != "claudeAgent" || instance.DisplayName != "Claude" || !instance.Enabled {
		t.Fatalf("Harness defaults were not applied: %#v", assembly.Manifest.ProviderInstances)
	}
}

func TestProviderContentChangeDoesNotUpdateTheDeployment(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	workstation.Spec.Providers = map[string]t3v1alpha1.ProviderSpec{"codex": {Enabled: true}}
	kube, reconciler := newWorkstationTestReconciler(t, now, workstation)
	reconcileWorkstation(t, reconciler)

	beforeDeployment := getControllerDeployment(t, kube)
	beforeManifest := readControllerManifest(t, kube)
	stored := getWorkstation(t, kube)
	stored.Spec.Providers = map[string]t3v1alpha1.ProviderSpec{
		"codex":  {Enabled: true, DisplayName: "Changed", Models: []string{"gpt-5.6-sol"}},
		"claude": {Enabled: true},
	}
	if err := kube.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	reconcileWorkstation(t, reconciler)

	afterDeployment := getControllerDeployment(t, kube)
	afterManifest := readControllerManifest(t, kube)
	if beforeManifest.DesiredRevision == afterManifest.DesiredRevision {
		t.Fatal("test did not change the rendered revision")
	}
	if beforeDeployment.Annotations[podRevisionAnnotation] != afterDeployment.Annotations[podRevisionAnnotation] ||
		!reflect.DeepEqual(beforeDeployment.Spec.Template, afterDeployment.Spec.Template) {
		t.Fatal("an inline provider change rolled the Workstation pod")
	}
}

func TestMCPServerAttachesToInlineProvidersByDefault(t *testing.T) {
	scheme := controllerTestScheme(t)
	revision := "sha256:" + strings.Repeat("a", 64)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	workstation.Spec.Providers = map[string]t3v1alpha1.ProviderSpec{
		"codex": {Enabled: true},
		"grok":  {Enabled: true},
	}
	workstation.Status.DesiredRevision = revision
	workstation.Status.LiveRevision = revision
	workstation.Status.Conditions = []metav1.Condition{{Type: conditionReady, Status: metav1.ConditionTrue, Reason: "RuntimeReady"}}
	server := &t3v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "remote", Generation: 1},
		Spec:       t3v1alpha1.MCPServerSpec{Config: jsonObject(`{"url":"https://mcp.example.test"}`)},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&t3v1alpha1.Workstation{}, &t3v1alpha1.MCPServer{}).
		WithObjects(workstation, server).
		Build()

	if _, err := (&MCPServerReconciler{Client: kube}).Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agents", Name: "remote"}}); err != nil {
		t.Fatal(err)
	}
	stored := &t3v1alpha1.MCPServer{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: "remote"}, stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Status.Attachments) != 1 || stored.Status.Attachments[0].TargetName != "primary/codex" {
		t.Fatalf("unexpected attachments: %#v", stored.Status.Attachments)
	}
	if !conditionIsTrue(stored.Status.Attachments[0].Conditions, conditionReady) || !conditionIsTrue(stored.Status.Conditions, conditionReady) {
		t.Fatalf("implicit attachment is not ready: %#v", stored.Status)
	}
}
