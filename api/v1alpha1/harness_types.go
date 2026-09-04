package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type HarnessSpec struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9_-]{0,63}$`
	InstanceID string `json:"instanceId,omitempty"`

	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9_-]{0,63}$`
	Driver string `json:"driver,omitempty"`

	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`

	// +kubebuilder:validation:MaxLength=64
	AccentColor string `json:"accentColor,omitempty"`

	// +kubebuilder:default:=true
	Enabled *bool `json:"enabled,omitempty"`

	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=name
	Environment []EnvironmentVariable `json:"environment,omitempty"`

	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	Config *apiextensionsv1.JSON `json:"config,omitempty"`

	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	WorkstationRefs []LocalObjectReference `json:"workstationRefs,omitempty"`

	// +kubebuilder:default:={}
	AttachmentPolicy AttachmentPolicy `json:"attachmentPolicy,omitempty"`
}

type HarnessStatus struct {
	ReconciledStatus `json:",inline"`

	AdapterSupport AdapterSupportLevel `json:"adapterSupport,omitempty"`

	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=targetName
	Attachments []AttachmentStatus `json:"attachments,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hrn
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.spec.driver`
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=`.spec.instanceId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Harness struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HarnessSpec   `json:"spec"`
	Status HarnessStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type HarnessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Harness `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Harness{}, &HarnessList{})
}
