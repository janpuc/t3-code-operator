package controller

import (
	"strings"
	"testing"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func minimalWorkstation() *t3v1alpha1.Workstation {
	return &t3v1alpha1.Workstation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "t3", UID: types.UID("minimal-uid")},
		Spec: t3v1alpha1.WorkstationSpec{
			Providers:        map[string]t3v1alpha1.ProviderSpec{"codex": {Enabled: true}},
			WorkspaceSharing: &t3v1alpha1.WorkspaceSharing{SMB: &t3v1alpha1.SMBWorkspaceShare{}},
		},
	}
}

func TestWorkloadResourcesApplyOpinionatedDefaults(t *testing.T) {
	workstation := minimalWorkstation()
	images := testWorkloadImages()
	images.Workstation = "ghcr.io/janpuc/t3-code-runtime@sha256:" + strings.Repeat("d", 64)
	resources, err := BuildWorkloadResources(workstation, controllerTestManifest(t, workstation), nil, images)
	if err != nil {
		t.Fatal(err)
	}
	for _, container := range resources.Deployment.Spec.Template.Spec.Containers[:2] {
		if container.Image != images.Workstation {
			t.Fatalf("container %s does not run the default runtime image: %s", container.Name, container.Image)
		}
	}
	if len(resources.Claims) != 2 {
		t.Fatalf("expected data and workspace claims, got %#v", resources.Claims)
	}
	for _, claim := range resources.Claims {
		size := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		want := defaultDataClaimSize
		if claim.Name == "t3-workspace" {
			want = defaultWorkspaceClaimSize
		}
		if size.String() != want {
			t.Fatalf("claim %s requests %s, want %s", claim.Name, size.String(), want)
		}
		if len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
			t.Fatalf("claim %s has unexpected access modes: %#v", claim.Name, claim.Spec.AccessModes)
		}
		if claim.Annotations[claimRetentionAnnotation] != string(t3v1alpha1.ClaimRetentionRetain) {
			t.Fatalf("claim %s is not retained by default: %#v", claim.Name, claim.Annotations)
		}
	}
	var credentials *corev1.SecretVolumeSource
	for _, volume := range resources.Deployment.Spec.Template.Spec.Volumes {
		if volume.Name == "smb-credentials" {
			credentials = volume.Secret
		}
	}
	if credentials == nil || credentials.SecretName != "t3-smb" || credentials.Items[0].Key != defaultSMBPasswordKey {
		t.Fatalf("SMB password secret was not defaulted: %#v", credentials)
	}
	if resources.SMBService == nil || resources.SMBService.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("SMB service was not defaulted: %#v", resources.SMBService)
	}
}

func TestWorkloadResourcesPreferTheWorkstationImage(t *testing.T) {
	workstation := minimalWorkstation()
	workstation.Spec.Image = "registry.example/custom@sha256:" + strings.Repeat("e", 64)
	images := testWorkloadImages()
	images.Workstation = "ghcr.io/janpuc/t3-code-runtime@sha256:" + strings.Repeat("d", 64)
	resources, err := BuildWorkloadResources(workstation, controllerTestManifest(t, workstation), nil, images)
	if err != nil {
		t.Fatal(err)
	}
	if got := resources.Deployment.Spec.Template.Spec.Containers[0].Image; got != workstation.Spec.Image {
		t.Fatalf("spec.image was overridden by the default: %s", got)
	}
}

func TestWorkloadResourcesRequireAnImageSource(t *testing.T) {
	workstation := minimalWorkstation()
	_, err := BuildWorkloadResources(workstation, controllerTestManifest(t, workstation), nil, testWorkloadImages())
	if err == nil || !strings.Contains(err.Error(), "--default-workstation-image") {
		t.Fatalf("expected the missing image to be reported, got %v", err)
	}
}
