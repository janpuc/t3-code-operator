package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type LocalObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
}

type SecretKeyReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._A-Za-z0-9]+$`
	Key string `json:"key"`
}

type EnvironmentValueSource struct {
	SecretKeyRef SecretKeyReference `json:"secretKeyRef"`
}

// +kubebuilder:validation:XValidation:rule="has(self.value) != has(self.valueFrom)",message="exactly one of value or valueFrom must be set"
type EnvironmentVariable struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// +kubebuilder:validation:MaxLength=32768
	// +kubebuilder:validation:Pattern=`^[^\x00]*$`
	Value *string `json:"value,omitempty"`

	ValueFrom *EnvironmentValueSource `json:"valueFrom,omitempty"`
}

// +kubebuilder:validation:Enum=SameNamespace;None
type AttachmentPolicyMode string

const (
	AttachmentPolicySameNamespace AttachmentPolicyMode = "SameNamespace"
	AttachmentPolicyNone          AttachmentPolicyMode = "None"
)

type AttachmentPolicy struct {
	// +kubebuilder:default:=SameNamespace
	Extensions AttachmentPolicyMode `json:"extensions,omitempty"`

	// +kubebuilder:default:=SameNamespace
	MCPServers AttachmentPolicyMode `json:"mcpServers,omitempty"`
}

type ReconciledStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	DesiredRevision string `json:"desiredRevision,omitempty"`

	LiveRevision string `json:"liveRevision,omitempty"`

	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type AttachmentStatus struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	TargetName string `json:"targetName"`

	DesiredRevision string `json:"desiredRevision,omitempty"`

	LiveRevision string `json:"liveRevision,omitempty"`

	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:validation:Enum=Supported;Alpha;Unsupported
type AdapterSupportLevel string

const (
	AdapterSupported   AdapterSupportLevel = "Supported"
	AdapterAlpha       AdapterSupportLevel = "Alpha"
	AdapterUnsupported AdapterSupportLevel = "Unsupported"
)
