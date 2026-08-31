package render

import "encoding/json"

const ProtocolVersion = "t3code.janpuc.com/rendered/v1alpha1"

type ResolvedWorkstation struct {
	Namespace   string
	Name        string
	UID         string
	MachineInfo *MachineInfo
	Git         *GitConfiguration
	Tools       []ResolvedTool
	Harnesses   []Harness
}

type MachineInfo struct {
	PrettyHostname string
}

type GitConfiguration struct {
	UserName            string
	UserEmail           string
	GitHubUser          string
	CredentialSecretRef *SecretReference
	SigningKeySecretRef *GitSigningKeyReference
}

type GitSigningKeyReference struct {
	PrivateKey *SecretReference
	PublicKey  *SecretReference
}

type ResolvedTool struct {
	Name      string
	Backend   string
	Version   string
	Options   map[string]string
	Artifacts []ToolArtifact
}

type Harness struct {
	InstanceID  string
	Driver      string
	DisplayName string
	AccentColor string
	Enabled     bool
	Environment []EnvironmentVariable
	Config      json.RawMessage
	MCPServers  []MCPServer
	Extensions  []Extension
}

type EnvironmentVariable struct {
	Name      string
	Value     *string
	ValueFrom *SecretReference
}

type MCPServer struct {
	Name        string
	Transport   string
	Config      json.RawMessage
	Headers     []Header
	Environment []EnvironmentVariable
}

type Header struct {
	Name      string
	Prefix    string
	Value     *string
	ValueFrom *SecretReference
}

type SecretReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}

type Extension struct {
	Name   string
	Source ExtensionSource
}

type ExtensionSource struct {
	Type          ExtensionSourceType           `json:"type"`
	Git           *GitExtensionSource           `json:"git,omitempty"`
	OCI           *OCIExtensionSource           `json:"oci,omitempty"`
	Marketplace   *MarketplaceExtensionSource   `json:"marketplace,omitempty"`
	GitHubRelease *GitHubReleaseExtensionSource `json:"githubRelease,omitempty"`
	Include       []string                      `json:"include,omitempty"`
}

type ExtensionSourceType string

const (
	ExtensionSourceGit           ExtensionSourceType = "Git"
	ExtensionSourceOCI           ExtensionSourceType = "OCI"
	ExtensionSourceMarketplace   ExtensionSourceType = "Marketplace"
	ExtensionSourceGitHubRelease ExtensionSourceType = "GitHubRelease"
)

type GitExtensionSource struct {
	URL                 string           `json:"url"`
	Commit              string           `json:"commit"`
	Path                string           `json:"path,omitempty"`
	CredentialSecretRef *SecretReference `json:"credentialSecretRef,omitempty"`
}

type OCIExtensionSource struct {
	Repository          string           `json:"repository"`
	Digest              string           `json:"digest"`
	CredentialSecretRef *SecretReference `json:"credentialSecretRef,omitempty"`
}

type MarketplaceExtensionSource struct {
	Marketplace         string           `json:"marketplace"`
	Extension           string           `json:"extension"`
	RepositoryURL       string           `json:"repositoryUrl"`
	Commit              string           `json:"commit"`
	CredentialSecretRef *SecretReference `json:"credentialSecretRef,omitempty"`
}

type GitHubReleaseExtensionSource struct {
	Repository          string           `json:"repository"`
	Tag                 string           `json:"tag"`
	Asset               string           `json:"asset"`
	SHA256              string           `json:"sha256"`
	CredentialSecretRef *SecretReference `json:"credentialSecretRef,omitempty"`
}

type Manifest struct {
	APIVersion        string                      `json:"apiVersion"`
	Kind              string                      `json:"kind"`
	Workstation       WorkstationIdentity         `json:"workstation"`
	DesiredRevision   string                      `json:"desiredRevision,omitempty"`
	ServerSettings    ManagedServerSettings       `json:"serverSettings"`
	ProviderInstances map[string]ProviderInstance `json:"providerInstances"`
	Files             []FileTarget                `json:"files,omitempty"`
	Extensions        []ExtensionActivation       `json:"extensions,omitempty"`
	Tools             []ToolActivation            `json:"tools,omitempty"`
	Warnings          []Issue                     `json:"warnings,omitempty"`
}

type ManagedServerSettings struct {
	EnableProviderUpdateChecks bool `json:"enableProviderUpdateChecks"`
}

type WorkstationIdentity struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type ProviderInstance struct {
	Driver       string                `json:"driver"`
	DisplayName  string                `json:"displayName,omitempty"`
	AccentColor  string                `json:"accentColor,omitempty"`
	Enabled      bool                  `json:"enabled"`
	SupportLevel SupportLevel          `json:"supportLevel"`
	Environment  []ProviderEnvironment `json:"environment,omitempty"`
	Config       json.RawMessage       `json:"config,omitempty"`
	Apply        ApplyPolicy           `json:"apply"`
}

type ProviderEnvironment struct {
	Name         string           `json:"name"`
	Value        string           `json:"value,omitempty"`
	ValueFrom    *SecretReference `json:"valueFrom,omitempty"`
	SecretPrefix string           `json:"secretPrefix,omitempty"`
	Sensitive    bool             `json:"sensitive,omitempty"`
}

type SupportLevel string

const (
	SupportLevelSupported SupportLevel = "Supported"
	SupportLevelAlpha     SupportLevel = "Alpha"
)

type FileTarget struct {
	Scope                      FileScope            `json:"scope,omitempty"`
	InstanceID                 string               `json:"instanceId,omitempty"`
	Path                       string               `json:"path"`
	Mode                       WriteMode            `json:"mode"`
	Format                     FileFormat           `json:"format"`
	Content                    string               `json:"content,omitempty"`
	OwnedPaths                 []string             `json:"ownedPaths,omitempty"`
	Values                     []FileValueSource    `json:"values,omitempty"`
	SecretContent              *SecretContentSource `json:"secretContent,omitempty"`
	DiscoverGitSafeDirectories bool                 `json:"discoverGitSafeDirectories,omitempty"`
	Apply                      ApplyPolicy          `json:"apply"`
}

type FileScope string

const (
	FileScopeHarness     FileScope = "Harness"
	FileScopeWorkstation FileScope = "Workstation"
)

type FileValueSource struct {
	Path      string          `json:"path"`
	ValueFrom SecretReference `json:"valueFrom"`
	Transform SecretTransform `json:"transform,omitempty"`
}

type SecretContentSource struct {
	ValueFrom SecretReference `json:"valueFrom"`
	Transform SecretTransform `json:"transform,omitempty"`
	Prefix    string          `json:"prefix,omitempty"`
	Suffix    string          `json:"suffix,omitempty"`
}

type SecretTransform string

const (
	SecretTransformNone              SecretTransform = ""
	SecretTransformTrimSpace         SecretTransform = "TrimSpace"
	SecretTransformOpenSSHPrivateKey SecretTransform = "OpenSSHPrivateKey"
)

type ExtensionActivation struct {
	InstanceID   string                 `json:"instanceId"`
	Name         string                 `json:"name"`
	Source       ExtensionSource        `json:"source"`
	CacheKey     string                 `json:"cacheKey"`
	Destinations []ExtensionDestination `json:"destinations,omitempty"`
	Installer    *ExtensionInstaller    `json:"installer,omitempty"`
	Apply        ApplyPolicy            `json:"apply"`
}

type ExtensionDestination struct {
	SourcePath string      `json:"sourcePath"`
	Path       string      `json:"path"`
	Mode       WriteMode   `json:"mode"`
	Apply      ApplyPolicy `json:"apply"`
}

type ExtensionInstaller struct {
	Kind        InstallerKind `json:"kind"`
	Marketplace string        `json:"marketplace,omitempty"`
	Extension   string        `json:"extension"`
}

type ToolActivation struct {
	Name      string            `json:"name"`
	Backend   string            `json:"backend"`
	Version   string            `json:"version"`
	Options   map[string]string `json:"options,omitempty"`
	Artifacts []ToolArtifact    `json:"artifacts"`
	Apply     ApplyPolicy       `json:"apply"`
}

type ToolArtifact struct {
	Platform string `json:"platform"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size,omitempty"`
}

type InstallerKind string

const (
	InstallerCodexMarketplace    InstallerKind = "CodexMarketplace"
	InstallerClaudeMarketplace   InstallerKind = "ClaudeMarketplace"
	InstallerOpenCodePackage     InstallerKind = "OpenCodePackage"
	InstallerCodexReleaseBundle  InstallerKind = "CodexReleaseBundle"
	InstallerClaudeReleaseBundle InstallerKind = "ClaudeReleaseBundle"
)

type WriteMode string

const (
	WriteModeReplace      WriteMode = "Replace"
	WriteModeSeedIfAbsent WriteMode = "SeedIfAbsent"
	WriteModeMerge        WriteMode = "Merge"
	WriteModeManagedBlock WriteMode = "ManagedBlock"
)

type FileFormat string

const (
	FileFormatJSON FileFormat = "JSON"
	FileFormatTOML FileFormat = "TOML"
	FileFormatYAML FileFormat = "YAML"
	FileFormatText FileFormat = "Text"
)

type ApplyPolicy struct {
	Class     ChangeClass     `json:"class"`
	When      ApplyWhen       `json:"when"`
	Mechanism ReloadMechanism `json:"mechanism"`
}

type ChangeClass string

const (
	ChangeClassAdditive   ChangeClass = "Additive"
	ChangeClassDisruptive ChangeClass = "Disruptive"
)

type ApplyWhen string

const (
	ApplyWhenImmediate ApplyWhen = "Immediate"
	ApplyWhenIdle      ApplyWhen = "Idle"
)

type ReloadMechanism string

const (
	ReloadProviderRebuild ReloadMechanism = "ProviderRebuild"
	ReloadWatch           ReloadMechanism = "Watch"
	ReloadNextSession     ReloadMechanism = "NextSession"
)

type IssueCode string

const (
	IssueAlphaDialect               IssueCode = "AlphaDialect"
	IssueUnsupportedExtensionSource IssueCode = "UnsupportedExtensionSource"
)

type Issue struct {
	Code       IssueCode `json:"code"`
	InstanceID string    `json:"instanceId,omitempty"`
	Resource   string    `json:"resource,omitempty"`
	Message    string    `json:"message"`
}
