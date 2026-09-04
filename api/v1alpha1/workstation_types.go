package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type WorkstationSecurityContext struct {
	RunAsNonRoot *bool `json:"runAsNonRoot,omitempty"`

	// +kubebuilder:validation:Minimum=1
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// +kubebuilder:validation:Minimum=1
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`

	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:Minimum=1
	// +listType=set
	SupplementalGroups []int64 `json:"supplementalGroups,omitempty"`

	AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation,omitempty"`

	ReadOnlyRootFilesystem *bool `json:"readOnlyRootFilesystem,omitempty"`

	Capabilities *corev1.Capabilities `json:"capabilities,omitempty"`

	SeccompProfile *corev1.SeccompProfile `json:"seccompProfile,omitempty"`
}

type WorkstationEnvironmentVariable struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// +kubebuilder:validation:MaxLength=32768
	// +kubebuilder:validation:Pattern=`^[^\x00]*$`
	Value string `json:"value"`
}

type MachineInfo struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	PrettyHostname string `json:"prettyHostname"`
}

type ExistingClaimVolumeSource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
}

// +kubebuilder:validation:Enum=Retain;Delete
type ClaimRetentionPolicy string

const (
	ClaimRetentionRetain ClaimRetentionPolicy = "Retain"
	ClaimRetentionDelete ClaimRetentionPolicy = "Delete"
)

type ClaimTemplateMetadata struct {
	// +kubebuilder:validation:MaxProperties=64
	Labels map[string]string `json:"labels,omitempty"`

	// +kubebuilder:validation:MaxProperties=64
	Annotations map[string]string `json:"annotations,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.volumeMode) || self.spec.volumeMode == 'Filesystem'",message="ClaimTemplate volumeMode must be Filesystem"
type ClaimTemplateVolumeSource struct {
	Metadata ClaimTemplateMetadata `json:"metadata,omitempty"`

	Spec corev1.PersistentVolumeClaimSpec `json:"spec,omitempty"`

	// +kubebuilder:default:=Retain
	RetentionPolicy ClaimRetentionPolicy `json:"retentionPolicy,omitempty"`
}

type NFSVolumeSource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[^\x00-\x20\x7F]+$`
	Server string `json:"server"`

	// +kubebuilder:validation:Pattern=`^/[^\x00-\x1F\x7F]*$`
	// +kubebuilder:validation:MaxLength=4096
	ExportPath string `json:"exportPath"`

	ReadOnly bool `json:"readOnly,omitempty"`
}

// +kubebuilder:validation:Enum=ExistingClaim;ClaimTemplate;EmptyDir
type DataVolumeType string

const (
	DataVolumeExistingClaim DataVolumeType = "ExistingClaim"
	DataVolumeClaimTemplate DataVolumeType = "ClaimTemplate"
	DataVolumeEmptyDir      DataVolumeType = "EmptyDir"
)

// +kubebuilder:validation:XValidation:rule="(has(self.existingClaim) ? 1 : 0) + (has(self.claimTemplate) ? 1 : 0) + (has(self.emptyDir) ? 1 : 0) <= 1",message="at most one data volume configuration may be set"
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type == 'ClaimTemplate' ? !has(self.existingClaim) && !has(self.emptyDir) : self.type == 'ExistingClaim' ? has(self.existingClaim) : has(self.emptyDir)",message="data volume configuration must match type"
type DataVolumeSource struct {
	Type DataVolumeType `json:"type,omitempty"`

	ExistingClaim *ExistingClaimVolumeSource `json:"existingClaim,omitempty"`

	ClaimTemplate *ClaimTemplateVolumeSource `json:"claimTemplate,omitempty"`

	EmptyDir *corev1.EmptyDirVolumeSource `json:"emptyDir,omitempty"`
}

// +kubebuilder:validation:Enum=ExistingClaim;ClaimTemplate;NFS;EmptyDir
type WorkspaceVolumeType string

const (
	WorkspaceVolumeExistingClaim WorkspaceVolumeType = "ExistingClaim"
	WorkspaceVolumeClaimTemplate WorkspaceVolumeType = "ClaimTemplate"
	WorkspaceVolumeNFS           WorkspaceVolumeType = "NFS"
	WorkspaceVolumeEmptyDir      WorkspaceVolumeType = "EmptyDir"
)

// +kubebuilder:validation:XValidation:rule="(has(self.existingClaim) ? 1 : 0) + (has(self.claimTemplate) ? 1 : 0) + (has(self.nfs) ? 1 : 0) + (has(self.emptyDir) ? 1 : 0) <= 1",message="at most one workspace volume configuration may be set"
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type == 'ClaimTemplate' ? !has(self.existingClaim) && !has(self.nfs) && !has(self.emptyDir) : self.type == 'ExistingClaim' ? has(self.existingClaim) : self.type == 'NFS' ? has(self.nfs) : has(self.emptyDir)",message="workspace volume configuration must match type"
type WorkspaceVolumeSource struct {
	Type WorkspaceVolumeType `json:"type,omitempty"`

	ExistingClaim *ExistingClaimVolumeSource `json:"existingClaim,omitempty"`

	ClaimTemplate *ClaimTemplateVolumeSource `json:"claimTemplate,omitempty"`

	NFS *NFSVolumeSource `json:"nfs,omitempty"`

	EmptyDir *corev1.EmptyDirVolumeSource `json:"emptyDir,omitempty"`
}

type WorkstationStorage struct {
	// +kubebuilder:default:={}
	Data DataVolumeSource `json:"data,omitempty"`

	// +kubebuilder:default:={}
	Workspace WorkspaceVolumeSource `json:"workspace,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.externalTrafficPolicy) || self.type in ['NodePort', 'LoadBalancer']",message="externalTrafficPolicy requires a NodePort or LoadBalancer Service"
// +kubebuilder:validation:XValidation:rule="!has(self.loadBalancerSourceRanges) || self.type == 'LoadBalancer'",message="loadBalancerSourceRanges requires a LoadBalancer Service"
// +kubebuilder:validation:XValidation:rule="!has(self.loadBalancerSourceRanges) || self.loadBalancerSourceRanges.all(r, isCIDR(r))",message="loadBalancerSourceRanges entries must be CIDRs"
type SMBServiceSpec struct {
	// +kubebuilder:default:=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	Type corev1.ServiceType `json:"type,omitempty"`

	// +kubebuilder:validation:MaxProperties=64
	Annotations map[string]string `json:"annotations,omitempty"`

	// +kubebuilder:validation:Enum=Cluster;Local
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicyType `json:"externalTrafficPolicy,omitempty"`

	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=43
	// +listType=set
	LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`
}

type SMBWorkspaceShare struct {
	// +kubebuilder:default:=t3
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]*$`
	Username string `json:"username,omitempty"`

	// +kubebuilder:default:=workspace
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_-]*$`
	ShareName string `json:"shareName,omitempty"`

	PasswordSecretRef *SecretKeyReference `json:"passwordSecretRef,omitempty"`

	ReadOnly bool `json:"readOnly,omitempty"`

	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	Service *SMBServiceSpec `json:"service,omitempty"`
}

type WorkspaceSharing struct {
	SMB *SMBWorkspaceShare `json:"smb,omitempty"`
}

type GitSigningKeyReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._A-Za-z0-9]+$`
	PrivateKeyKey string `json:"privateKeyKey"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._A-Za-z0-9]+$`
	PublicKeyKey string `json:"publicKeyKey"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.credentialSecretRef) || (has(self.githubUser) && size(self.githubUser) > 0)",message="githubUser is required with credentialSecretRef"
// +kubebuilder:validation:XValidation:rule="!has(self.githubUser) || has(self.credentialSecretRef)",message="githubUser requires credentialSecretRef"
// +kubebuilder:validation:XValidation:rule="!has(self.signingKeySecretRef) || (has(self.userEmail) && size(self.userEmail) > 0)",message="userEmail is required with signingKeySecretRef"
type GitIdentity struct {
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]*$`
	UserName string `json:"userName,omitempty"`

	// +kubebuilder:validation:MaxLength=254
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]*$`
	UserEmail string `json:"userEmail,omitempty"`

	// +kubebuilder:validation:MaxLength=39
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`
	GitHubUser string `json:"githubUser,omitempty"`

	CredentialSecretRef *SecretKeyReference `json:"credentialSecretRef,omitempty"`

	SigningKeySecretRef *GitSigningKeyReference `json:"signingKeySecretRef,omitempty"`
}

type ToolArtifactSpec struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9._-]{0,63}$`
	Platform string `json:"platform"`

	// +kubebuilder:validation:Pattern=`^https://[^[:space:]]+$`
	// +kubebuilder:validation:MaxLength=4096
	URL string `json:"url"`

	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	SHA256 string `json:"sha256"`

	// +kubebuilder:validation:Minimum=0
	Size int64 `json:"size,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.version != 'latest'",message="tool version must be pinned"
// +kubebuilder:validation:XValidation:rule="!self.backend.startsWith('http:') || (has(self.artifacts) && size(self.artifacts) > 0)",message="http tools require pinned artifacts"
type ToolSpec struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`
	Name string `json:"name"`

	// +kubebuilder:validation:Pattern=`^(aqua|github|gitlab|http):[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`
	// +kubebuilder:validation:MaxLength=262
	Backend string `json:"backend"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Version string `json:"version"`

	// +kubebuilder:validation:MaxProperties=32
	Options map[string]string `json:"options,omitempty"`

	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=platform
	Artifacts []ToolArtifactSpec `json:"artifacts,omitempty"`
}

// +kubebuilder:validation:Enum=WaitForIdle
type DrainPolicyMode string

const DrainPolicyWaitForIdle DrainPolicyMode = "WaitForIdle"

// +kubebuilder:validation:Enum=Block;Force
type DrainTimeoutAction string

const (
	DrainTimeoutBlock DrainTimeoutAction = "Block"
	DrainTimeoutForce DrainTimeoutAction = "Force"
)

// +kubebuilder:validation:XValidation:rule="!has(self.timeout) || duration(self.timeout) > duration('0s')",message="drain timeout must be positive"
type DrainPolicy struct {
	// +kubebuilder:default:=WaitForIdle
	Policy DrainPolicyMode `json:"policy,omitempty"`

	// +kubebuilder:default:="30m"
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// +kubebuilder:default:=Block
	TimeoutAction DrainTimeoutAction `json:"timeoutAction,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.disposable || !has(self.storage) || !has(self.storage.data) || !has(self.storage.data.type) || self.storage.data.type != 'EmptyDir'",message="data EmptyDir requires disposable=true"
// +kubebuilder:validation:XValidation:rule="!has(self.env) || self.env.all(e, !['HOME', 'PATH', 'T3CODE_HOME', 'XDG_CACHE_HOME', 'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'GH_CONFIG_DIR', 'GH_HOST', 'GH_GIT_PROTOCOL', 'GH_TOKEN', 'GITHUB_TOKEN'].exists(n, n == e.name))",message="env contains a reserved runtime variable"
// +kubebuilder:validation:XValidation:rule="!has(self.workspaceSharing) || !has(self.workspaceSharing.smb) || !has(self.storage) || !has(self.storage.workspace) || !has(self.storage.workspace.type) || self.storage.workspace.type in ['ExistingClaim', 'ClaimTemplate']",message="SMB workspace sharing requires a claim-backed workspace"
// +kubebuilder:validation:XValidation:rule="!has(self.workspaceSharing) || !has(self.workspaceSharing.smb) || !has(self.storage) || !has(self.storage.data) || !has(self.storage.workspace) || !has(self.storage.data.existingClaim) || !has(self.storage.workspace.existingClaim) || self.storage.data.existingClaim.name != self.storage.workspace.existingClaim.name",message="SMB workspace and data must use different claims"
// +kubebuilder:validation:XValidation:rule="!has(self.providers) || self.providers.all(name, name.matches('^[a-z]([-a-z0-9]*[a-z0-9])?$') && size(name) <= 63)",message="provider names must be lowercase DNS labels that start with a letter"
type WorkstationSpec struct {
	// +kubebuilder:validation:Pattern=`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image,omitempty"`

	// +kubebuilder:validation:MaxProperties=32
	Providers map[string]ProviderSpec `json:"providers,omitempty"`

	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	SecurityContext *WorkstationSecurityContext `json:"securityContext,omitempty"`

	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=name
	Environment []WorkstationEnvironmentVariable `json:"env,omitempty"`

	MachineInfo *MachineInfo `json:"machineInfo,omitempty"`

	Disposable bool `json:"disposable,omitempty"`

	// +kubebuilder:default:={}
	Storage WorkstationStorage `json:"storage,omitempty"`

	WorkspaceSharing *WorkspaceSharing `json:"workspaceSharing,omitempty"`

	Git *GitIdentity `json:"git,omitempty"`

	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Tools []ToolSpec `json:"tools,omitempty"`

	// +kubebuilder:default:={}
	Drain *DrainPolicy `json:"drain,omitempty"`
}

type WorkstationStatus struct {
	ReconciledStatus `json:",inline"`

	DataClaimName string `json:"dataClaimName,omitempty"`

	WorkspaceClaimName string `json:"workspaceClaimName,omitempty"`

	PodRevision string `json:"podRevision,omitempty"`

	PendingPodRevision string `json:"pendingPodRevision,omitempty"`

	DrainStartedAt *metav1.Time `json:"drainStartedAt,omitempty"`

	LiveImage string `json:"liveImage,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ws
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.status.desiredRevision`
// +kubebuilder:printcolumn:name="Live",type=string,JSONPath=`.status.liveRevision`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Workstation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkstationSpec   `json:"spec"`
	Status WorkstationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type WorkstationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workstation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workstation{}, &WorkstationList{})
}
