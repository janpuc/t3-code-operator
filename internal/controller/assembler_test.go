package controller

import (
	"context"
	"strings"
	"testing"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/render"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAssemblerResolvesAttachmentsPoliciesSecretsAndTools(t *testing.T) {
	scheme := controllerTestScheme(t)
	workstation := controllerTestWorkstation()
	workstation.Spec.MachineInfo = &t3v1alpha1.MachineInfo{PrettyHostname: "Cluster"}
	workstation.Spec.Git = &t3v1alpha1.GitIdentity{
		UserName:            "Agent User",
		UserEmail:           "agent@example.test",
		GitHubUser:          "agent-user",
		CredentialSecretRef: &t3v1alpha1.SecretKeyReference{Name: "git-token", Key: "token"},
		SigningKeySecretRef: &t3v1alpha1.GitSigningKeyReference{
			Name:          "git-signing",
			PrivateKeyKey: "private",
			PublicKeyKey:  "public",
		},
	}
	providerValue := t3v1alpha1.EnvironmentValueSource{SecretKeyRef: t3v1alpha1.SecretKeyReference{
		Name: "provider-token",
		Key:  "token",
	}}
	codex := &t3v1alpha1.Harness{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "codex"},
		Spec: t3v1alpha1.HarnessSpec{
			InstanceID:      "Codex_Main",
			Driver:          "codex",
			Config:          jsonObject(`{"homePath":"/data/harnesses/Codex_Main/codex"}`),
			Environment:     []t3v1alpha1.EnvironmentVariable{{Name: "OPENAI_API_KEY", ValueFrom: &providerValue}},
			WorkstationRefs: []t3v1alpha1.LocalObjectReference{{Name: "primary"}},
		},
	}
	claude := &t3v1alpha1.Harness{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "claude"},
		Spec: t3v1alpha1.HarnessSpec{
			InstanceID:      "claude",
			Driver:          "claudeAgent",
			WorkstationRefs: []t3v1alpha1.LocalObjectReference{{Name: "primary"}},
			AttachmentPolicy: t3v1alpha1.AttachmentPolicy{
				Extensions: t3v1alpha1.AttachmentPolicyNone,
				MCPServers: t3v1alpha1.AttachmentPolicyNone,
			},
		},
	}
	extension := &t3v1alpha1.Extension{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "agent-kit"},
		Spec: t3v1alpha1.ExtensionSpec{
			Source: t3v1alpha1.ExtensionSource{
				Type: t3v1alpha1.ExtensionSourceGit,
				Git: &t3v1alpha1.GitExtensionSource{
					URL:                 "https://example.test/agent-kit.git",
					Commit:              strings.Repeat("a", 40),
					CredentialSecretRef: &t3v1alpha1.SecretKeyReference{Name: "source-token", Key: "token"},
				},
			},
			HarnessRefs: []t3v1alpha1.LocalObjectReference{{Name: "codex"}, {Name: "claude"}},
		},
	}
	server := &t3v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "remote"},
		Spec: t3v1alpha1.MCPServerSpec{
			Transport: "http",
			Config:    jsonObject(`{"url":"https://mcp.example.test"}`),
			Headers: []t3v1alpha1.HTTPHeader{{
				Name:      "Authorization",
				Prefix:    "Bearer ",
				ValueFrom: &t3v1alpha1.EnvironmentValueSource{SecretKeyRef: t3v1alpha1.SecretKeyReference{Name: "mcp-token", Key: "token"}},
			}},
			HarnessRefs: []t3v1alpha1.LocalObjectReference{{Name: "codex"}, {Name: "claude"}},
		},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workstation, codex, claude, extension, server).Build()
	toolResolver := &fakeToolResolver{resolved: []render.ResolvedTool{{
		Name:    "rg",
		Backend: "aqua:BurntSushi/ripgrep",
		Version: "14.1.1",
		Artifacts: []render.ToolArtifact{{
			Platform: "linux-x64",
			URL:      "https://example.test/rg.tar.gz",
			SHA256:   "sha256:" + strings.Repeat("b", 64),
		}},
	}}}

	assembly, err := (&Assembler{Reader: reader, Tools: toolResolver}).Assemble(context.Background(), workstation)
	if err != nil {
		t.Fatal(err)
	}
	if len(toolResolver.requested) != 1 || len(assembly.Manifest.Tools) != 1 {
		t.Fatalf("tools were not resolved: request=%#v manifest=%#v", toolResolver.requested, assembly.Manifest.Tools)
	}
	if len(assembly.Manifest.ProviderInstances) != 2 || !assembly.Manifest.ProviderInstances["Codex_Main"].Enabled {
		t.Fatalf("attached Harnesses were not assembled: %#v", assembly.Manifest.ProviderInstances)
	}
	if len(assembly.Manifest.Extensions) != 1 || assembly.Manifest.Extensions[0].InstanceID != "Codex_Main" {
		t.Fatalf("Extension policy was not applied: %#v", assembly.Manifest.Extensions)
	}
	if got, want := strings.Join(assembly.SecretNames, ","), "git-signing,git-token,mcp-token,provider-token,source-token"; got != want {
		t.Fatalf("Secret set mismatch: got=%q want=%q", got, want)
	}
	if len(assembly.Manifest.Files) < 6 {
		t.Fatalf("Workstation files were not assembled: %#v", assembly.Manifest.Files)
	}
}

func TestAssemblerRejectsDuplicateProviderInstanceIDs(t *testing.T) {
	scheme := controllerTestScheme(t)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	objects := []runtime.Object{workstation}
	for _, name := range []string{"first", "second"} {
		objects = append(objects, &t3v1alpha1.Harness{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: name},
			Spec: t3v1alpha1.HarnessSpec{
				InstanceID:      "same",
				Driver:          "codex",
				WorkstationRefs: []t3v1alpha1.LocalObjectReference{{Name: "primary"}},
			},
		})
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	_, err := (&Assembler{Reader: reader}).Assemble(context.Background(), workstation)
	if err == nil || !strings.Contains(err.Error(), "duplicates provider instance") {
		t.Fatalf("expected duplicate instance rejection, got %v", err)
	}
}

type fakeToolResolver struct {
	requested []t3v1alpha1.ToolSpec
	resolved  []render.ResolvedTool
	err       error
}

func (resolver *fakeToolResolver) Resolve(
	_ context.Context,
	requested []t3v1alpha1.ToolSpec,
) ([]render.ResolvedTool, error) {
	resolver.requested = append([]t3v1alpha1.ToolSpec(nil), requested...)
	return append([]render.ResolvedTool(nil), resolver.resolved...), resolver.err
}

func controllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := t3v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func controllerTestWorkstation() *t3v1alpha1.Workstation {
	return &t3v1alpha1.Workstation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "agents",
			Name:      "primary",
			UID:       types.UID("workstation-uid"),
		},
		Spec: t3v1alpha1.WorkstationSpec{
			Image: "registry.example/t3@sha256:" + strings.Repeat("c", 64),
			Storage: t3v1alpha1.WorkstationStorage{
				Data: t3v1alpha1.DataVolumeSource{
					Type:     t3v1alpha1.DataVolumeEmptyDir,
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
				Workspace: t3v1alpha1.WorkspaceVolumeSource{
					Type:     t3v1alpha1.WorkspaceVolumeEmptyDir,
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
			Disposable: true,
			Tools: []t3v1alpha1.ToolSpec{{
				Name: "rg", Backend: "aqua:BurntSushi/ripgrep", Version: "14.1.1",
			}},
		},
	}
}

func jsonObject(value string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(value)}
}
