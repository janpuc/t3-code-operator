package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:validation:Enum=Git;OCI;Marketplace;GitHubRelease
type ExtensionSourceType string

const (
	ExtensionSourceGit           ExtensionSourceType = "Git"
	ExtensionSourceOCI           ExtensionSourceType = "OCI"
	ExtensionSourceMarketplace   ExtensionSourceType = "Marketplace"
	ExtensionSourceGitHubRelease ExtensionSourceType = "GitHubRelease"
)

type GitExtensionSource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`

	// +kubebuilder:validation:Pattern=`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`
	Commit string `json:"commit"`

	// +kubebuilder:validation:MaxLength=1024
	Path string `json:"path,omitempty"`

	CredentialSecretRef *SecretKeyReference `json:"credentialSecretRef,omitempty"`
}

type OCIExtensionSource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Repository string `json:"repository"`

	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9+._-]*:[0-9a-fA-F]{32,}$`
	Digest string `json:"digest"`

	CredentialSecretRef *SecretKeyReference `json:"credentialSecretRef,omitempty"`
}

type MarketplaceExtensionSource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Marketplace string `json:"marketplace"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Extension string `json:"extension"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	RepositoryURL string `json:"repositoryUrl"`

	// +kubebuilder:validation:Pattern=`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`
	Commit string `json:"commit"`

	CredentialSecretRef *SecretKeyReference `json:"credentialSecretRef,omitempty"`
}

type GitHubReleaseExtensionSource struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	Repository string `json:"repository"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Tag string `json:"tag"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Asset string `json:"asset"`

	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{64}$`
	SHA256 string `json:"sha256"`

	CredentialSecretRef *SecretKeyReference `json:"credentialSecretRef,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.git) ? 1 : 0) + (has(self.oci) ? 1 : 0) + (has(self.marketplace) ? 1 : 0) + (has(self.githubRelease) ? 1 : 0) == 1",message="exactly one source configuration must be set"
// +kubebuilder:validation:XValidation:rule="self.type == 'Git' ? has(self.git) : self.type == 'OCI' ? has(self.oci) : self.type == 'Marketplace' ? has(self.marketplace) : has(self.githubRelease)",message="source configuration must match type"
type ExtensionSource struct {
	Type ExtensionSourceType `json:"type"`

	Git *GitExtensionSource `json:"git,omitempty"`

	OCI *OCIExtensionSource `json:"oci,omitempty"`

	Marketplace *MarketplaceExtensionSource `json:"marketplace,omitempty"`

	GitHubRelease *GitHubReleaseExtensionSource `json:"githubRelease,omitempty"`

	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]{0,252}$`
	// +listType=set
	Include []string `json:"include,omitempty"`
}

type ExtensionSpec struct {
	Source ExtensionSource `json:"source"`

	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	HarnessRefs []LocalObjectReference `json:"harnessRefs,omitempty"`
}

type ExtensionStatus struct {
	ReconciledStatus `json:",inline"`

	ResolvedSource string `json:"resolvedSource,omitempty"`

	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=targetName
	Attachments []AttachmentStatus `json:"attachments,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ext
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Extension struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ExtensionSpec   `json:"spec"`
	Status ExtensionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ExtensionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Extension `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Extension{}, &ExtensionList{})
}
