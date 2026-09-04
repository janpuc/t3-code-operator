package controller

import (
	"regexp"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	defaultDataClaimSize      = "20Gi"
	defaultWorkspaceClaimSize = "50Gi"
	defaultSMBPasswordKey     = "password"
)

func effectiveStorage(workstation *t3v1alpha1.Workstation) t3v1alpha1.WorkstationStorage {
	storage := *workstation.Spec.Storage.DeepCopy()
	if storage.Data.Type == "" {
		storage.Data.Type = t3v1alpha1.DataVolumeClaimTemplate
	}
	if storage.Data.Type == t3v1alpha1.DataVolumeClaimTemplate {
		storage.Data.ClaimTemplate = defaultedClaimTemplate(storage.Data.ClaimTemplate, defaultDataClaimSize)
	}
	if storage.Workspace.Type == "" {
		storage.Workspace.Type = t3v1alpha1.WorkspaceVolumeClaimTemplate
	}
	if storage.Workspace.Type == t3v1alpha1.WorkspaceVolumeClaimTemplate {
		storage.Workspace.ClaimTemplate = defaultedClaimTemplate(storage.Workspace.ClaimTemplate, defaultWorkspaceClaimSize)
	}
	return storage
}

func defaultedClaimTemplate(template *t3v1alpha1.ClaimTemplateVolumeSource, size string) *t3v1alpha1.ClaimTemplateVolumeSource {
	result := &t3v1alpha1.ClaimTemplateVolumeSource{}
	if template != nil {
		result = template.DeepCopy()
	}
	if len(result.Spec.AccessModes) == 0 {
		result.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}
	if result.Spec.Resources.Requests == nil {
		result.Spec.Resources.Requests = corev1.ResourceList{}
	}
	if _, exists := result.Spec.Resources.Requests[corev1.ResourceStorage]; !exists {
		result.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(size)
	}
	if result.RetentionPolicy == "" {
		result.RetentionPolicy = t3v1alpha1.ClaimRetentionRetain
	}
	return result
}

func effectiveSMBPasswordSecretRef(workstation *t3v1alpha1.Workstation, share *t3v1alpha1.SMBWorkspaceShare) t3v1alpha1.SecretKeyReference {
	if share.PasswordSecretRef != nil {
		return *share.PasswordSecretRef
	}
	return t3v1alpha1.SecretKeyReference{
		Name: NamesForWorkstation(workstation.Name).SMBService,
		Key:  defaultSMBPasswordKey,
	}
}

// DigestPinnedImagePattern is the image reference shape a Workstation accepts.
var DigestPinnedImagePattern = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
