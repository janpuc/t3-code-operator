package v1alpha1

import apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

// ProviderSpec configures one upstream t3 provider instance inline on a
// Workstation. The map key is the provider instance ID.
type ProviderSpec struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9_-]{0,63}$`
	Driver string `json:"driver,omitempty"`

	Enabled bool `json:"enabled"`

	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`

	// +kubebuilder:validation:MaxLength=64
	AccentColor string `json:"accentColor,omitempty"`

	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=name
	Environment []EnvironmentVariable `json:"environment,omitempty"`

	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	// +listType=set
	Models []string `json:"models,omitempty"`

	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	Config *apiextensionsv1.JSON `json:"config,omitempty"`

	// +kubebuilder:default:={}
	AttachmentPolicy AttachmentPolicy `json:"attachmentPolicy,omitempty"`
}
