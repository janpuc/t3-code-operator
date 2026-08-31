package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:XValidation:rule="has(self.value) != has(self.valueFrom)",message="exactly one of value or valueFrom must be set"
type HTTPHeader struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9-]*$`
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`

	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[^\r\n\x00]*$`
	Prefix string `json:"prefix,omitempty"`

	// +kubebuilder:validation:MaxLength=32768
	// +kubebuilder:validation:Pattern=`^[^\r\n\x00]*$`
	Value *string `json:"value,omitempty"`

	ValueFrom *EnvironmentValueSource `json:"valueFrom,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.headers) || self.headers.all(header, self.headers.exists_one(candidate, candidate.name.lowerAscii() == header.name.lowerAscii()))",message="header names must be unique when compared case-insensitively"
type MCPServerSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Transport string `json:"transport"`

	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	Config *apiextensionsv1.JSON `json:"config,omitempty"`

	// +kubebuilder:validation:MaxItems=64
	Headers []HTTPHeader `json:"headers,omitempty"`

	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=name
	Environment []EnvironmentVariable `json:"environment,omitempty"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	HarnessRefs []LocalObjectReference `json:"harnessRefs"`
}

type MCPServerStatus struct {
	ReconciledStatus `json:",inline"`

	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=targetName
	Attachments []AttachmentStatus `json:"attachments,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcp
// +kubebuilder:printcolumn:name="Transport",type=string,JSONPath=`.spec.transport`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type MCPServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MCPServerSpec   `json:"spec"`
	Status MCPServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MCPServer{}, &MCPServerList{})
}
