package v1alpha1

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const envtestIndexURL = "https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml"

func TestAPIServerValidationAndOpaqueConfigRoundTrip(t *testing.T) {
	if os.Getenv("T3_PHASE1_ENVTEST") != "1" {
		t.Skip("set T3_PHASE1_ENVTEST=1 to run the API server test")
	}

	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	assetsDirectory, err := envtest.SetupEnvtestDefaultBinaryAssetsDirectory()
	if err != nil {
		t.Fatal(err)
	}

	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths:            []string{filepath.Join(repositoryRoot, "config", "crd", "bases")},
		ErrorIfCRDPathMissing:        true,
		DownloadBinaryAssets:         true,
		DownloadBinaryAssetsVersion:  "v1.37.0",
		DownloadBinaryAssetsIndexURL: envtestIndexURL,
		BinaryAssetsDirectory:        assetsDirectory,
		ControlPlaneStartTimeout:     30 * time.Second,
		ControlPlaneStopTimeout:      30 * time.Second,
		AttachControlPlaneOutput:     false,
	}

	configuration, err := testEnvironment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			t.Error(err)
		}
	})

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	apiClient, err := client.New(configuration, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "phase1-api"}}
	if err := apiClient.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}

	assertInvalidMCPServersAreRejected(t, ctx, apiClient, namespace.Name)
	assertInvalidWorkstationsAreRejected(t, ctx, apiClient, namespace.Name)
	assertOpaqueConfigRoundTripsThroughAPIServer(t, ctx, apiClient, namespace.Name)
	assertNFSWorkstationIsAcceptedAndDefaulted(t, ctx, apiClient, namespace.Name)
	assertSMBWorkspaceSharingIsAcceptedAndDefaulted(t, ctx, apiClient, namespace.Name)
	assertMinimalWorkstationIsDefaulted(t, ctx, apiClient, namespace.Name)
	assertProvidersRequireExplicitEnabled(t, ctx, apiClient, namespace.Name)
	assertMinimalContentObjectsAreAccepted(t, ctx, apiClient, namespace.Name)
}

func assertMinimalWorkstationIsDefaulted(t *testing.T, ctx context.Context, apiClient client.Client, namespace string) {
	t.Helper()
	workstation := &Workstation{
		ObjectMeta: metav1.ObjectMeta{Name: "minimal", Namespace: namespace},
		Spec: WorkstationSpec{
			Providers: map[string]ProviderSpec{"codex": {Enabled: true}, "grok": {Enabled: false}},
		},
	}
	if err := apiClient.Create(ctx, workstation); err != nil {
		t.Fatal(err)
	}
	persisted := &Workstation{}
	if err := apiClient.Get(ctx, client.ObjectKeyFromObject(workstation), persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Spec.Storage.Data.Type != DataVolumeClaimTemplate || persisted.Spec.Storage.Workspace.Type != WorkspaceVolumeClaimTemplate {
		t.Fatalf("storage defaults were not applied: %#v", persisted.Spec.Storage)
	}
	if persisted.Spec.Drain == nil || persisted.Spec.Drain.Policy != DrainPolicyWaitForIdle {
		t.Fatalf("drain defaults were not applied: %#v", persisted.Spec.Drain)
	}
	if persisted.Spec.Providers["codex"].AttachmentPolicy.MCPServers != AttachmentPolicySameNamespace {
		t.Fatalf("provider attachment policy default was not applied: %#v", persisted.Spec.Providers)
	}
	if persisted.Spec.Providers["grok"].Enabled {
		t.Fatal("an explicit enabled=false provider was persisted as enabled")
	}

	shared := &Workstation{
		ObjectMeta: metav1.ObjectMeta{Name: "minimal-smb", Namespace: namespace},
		Spec: WorkstationSpec{
			Providers:        map[string]ProviderSpec{"codex": {Enabled: true}},
			WorkspaceSharing: &WorkspaceSharing{SMB: &SMBWorkspaceShare{}},
		},
	}
	if err := apiClient.Create(ctx, shared); err != nil {
		t.Fatalf("SMB sharing without a password reference was rejected: %v", err)
	}
}

func assertProvidersRequireExplicitEnabled(t *testing.T, ctx context.Context, apiClient client.Client, namespace string) {
	t.Helper()
	for name, providers := range map[string]map[string]any{
		"implicit-provider": {"codex": map[string]any{}},
		"invalid-provider":  {"Codex_Main": map[string]any{"enabled": true}},
	} {
		object := &unstructured.Unstructured{}
		object.SetAPIVersion(GroupVersion.String())
		object.SetKind("Workstation")
		object.SetNamespace(namespace)
		object.SetName(name)
		object.Object["spec"] = map[string]any{"providers": providers}
		err := apiClient.Create(ctx, object)
		if err == nil {
			t.Fatalf("invalid Workstation %q was accepted", name)
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("invalid Workstation %q returned %T: %v", name, err, err)
		}
	}
}

func assertMinimalContentObjectsAreAccepted(t *testing.T, ctx context.Context, apiClient client.Client, namespace string) {
	t.Helper()
	harness := &Harness{ObjectMeta: metav1.ObjectMeta{Name: "claude", Namespace: namespace}}
	if err := apiClient.Create(ctx, harness); err != nil {
		t.Fatalf("a Harness without instanceId, driver, or workstationRefs was rejected: %v", err)
	}
	server := &MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: namespace},
		Spec: MCPServerSpec{
			Config:               &apiextensionsv1.JSON{Raw: []byte(`{"url":"https://mcp.example.test/gateway"}`)},
			BearerTokenSecretRef: &SecretKeyReference{Name: "gateway", Key: "token"},
		},
	}
	if err := apiClient.Create(ctx, server); err != nil {
		t.Fatalf("an MCPServer with only config and a bearer token was rejected: %v", err)
	}
	extension := &Extension{
		ObjectMeta: metav1.ObjectMeta{Name: "skills", Namespace: namespace},
		Spec: ExtensionSpec{Source: ExtensionSource{
			Type: ExtensionSourceGit,
			Git:  &GitExtensionSource{URL: "https://example.test/skills.git", Commit: strings.Repeat("b", 40)},
		}},
	}
	if err := apiClient.Create(ctx, extension); err != nil {
		t.Fatalf("an Extension without harnessRefs was rejected: %v", err)
	}

	inline := "inline"
	conflicting := &MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "conflicting-auth", Namespace: namespace},
		Spec: MCPServerSpec{
			Transport:            "http",
			BearerTokenSecretRef: &SecretKeyReference{Name: "gateway", Key: "token"},
			Headers:              []HTTPHeader{{Name: "Authorization", Value: &inline}},
		},
	}
	err := apiClient.Create(ctx, conflicting)
	if err == nil {
		t.Fatal("the API server accepted a bearer token beside an Authorization header")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("conflicting MCPServer returned %T: %v", err, err)
	}
}

func assertInvalidWorkstationsAreRejected(t *testing.T, ctx context.Context, apiClient client.Client, namespace string) {
	t.Helper()

	zeroTimeout := metav1.Duration{}
	invalidWorkstations := []*Workstation{
		apiTestWorkstation(namespace, "invalid-service-account", func(workstation *Workstation) {
			workstation.Spec.ServiceAccountName = "INVALID"
		}),
		apiTestWorkstation(namespace, "invalid-drain-timeout", func(workstation *Workstation) {
			workstation.Spec.Drain = &DrainPolicy{Timeout: &zeroTimeout}
		}),
		apiTestWorkstation(namespace, "invalid-nfs-server", func(workstation *Workstation) {
			workstation.Spec.Storage.Workspace = WorkspaceVolumeSource{
				Type: WorkspaceVolumeNFS,
				NFS:  &NFSVolumeSource{Server: "nas internal", ExportPath: "/workspace"},
			}
		}),
		apiTestWorkstation(namespace, "invalid-nfs-path", func(workstation *Workstation) {
			workstation.Spec.Storage.Workspace = WorkspaceVolumeSource{
				Type: WorkspaceVolumeNFS,
				NFS:  &NFSVolumeSource{Server: "nas.internal", ExportPath: "/workspace\x00other"},
			}
		}),
		apiTestWorkstation(namespace, "invalid-image", func(workstation *Workstation) {
			workstation.Spec.Image = "ghcr.io/example/t3 code@sha256:" + strings.Repeat("a", 64)
		}),
		apiTestWorkstation(namespace, "invalid-signing-secret", func(workstation *Workstation) {
			workstation.Spec.Git = &GitIdentity{
				UserEmail: "agent@example.test",
				SigningKeySecretRef: &GitSigningKeyReference{
					Name:          "INVALID",
					PrivateKeyKey: "private",
					PublicKeyKey:  "public",
				},
			}
		}),
		apiTestWorkstation(namespace, "invalid-machine-info", func(workstation *Workstation) {
			workstation.Spec.MachineInfo = &MachineInfo{PrettyHostname: "primary\tworkstation"}
		}),
		apiTestWorkstation(namespace, "invalid-git-identity", func(workstation *Workstation) {
			workstation.Spec.Git = &GitIdentity{UserName: "Agent\tUser"}
		}),
		apiTestWorkstation(namespace, "invalid-smb-username", func(workstation *Workstation) {
			workstation.Spec.WorkspaceSharing = &WorkspaceSharing{SMB: &SMBWorkspaceShare{
				Username:          "agent user",
				PasswordSecretRef: &SecretKeyReference{Name: "workspace-smb", Key: "password"},
			}}
		}),
		apiTestWorkstation(namespace, "smb-on-nfs", func(workstation *Workstation) {
			workstation.Spec.Storage.Workspace = WorkspaceVolumeSource{
				Type: WorkspaceVolumeNFS,
				NFS:  &NFSVolumeSource{Server: "nas.internal", ExportPath: "/workspace"},
			}
			workstation.Spec.WorkspaceSharing = &WorkspaceSharing{SMB: &SMBWorkspaceShare{
				PasswordSecretRef: &SecretKeyReference{Name: "workspace-smb", Key: "password"},
			}}
		}),
		apiTestWorkstation(namespace, "invalid-smb-service-policy", func(workstation *Workstation) {
			workstation.Spec.WorkspaceSharing = &WorkspaceSharing{SMB: &SMBWorkspaceShare{
				PasswordSecretRef: &SecretKeyReference{Name: "workspace-smb", Key: "password"},
				Service: &SMBServiceSpec{
					Type:                  corev1.ServiceTypeClusterIP,
					ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
				},
			}}
		}),
		apiTestWorkstation(namespace, "invalid-smb-source-range", func(workstation *Workstation) {
			workstation.Spec.WorkspaceSharing = &WorkspaceSharing{SMB: &SMBWorkspaceShare{
				PasswordSecretRef: &SecretKeyReference{Name: "workspace-smb", Key: "password"},
				Service: &SMBServiceSpec{
					Type:                     corev1.ServiceTypeLoadBalancer,
					LoadBalancerSourceRanges: []string{"not-a-cidr"},
				},
			}}
		}),
		apiTestWorkstation(namespace, "invalid-block-workspace", func(workstation *Workstation) {
			block := corev1.PersistentVolumeBlock
			workstation.Spec.Storage.Workspace = WorkspaceVolumeSource{
				Type: WorkspaceVolumeClaimTemplate,
				ClaimTemplate: &ClaimTemplateVolumeSource{Spec: corev1.PersistentVolumeClaimSpec{
					VolumeMode:  &block,
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				}},
			}
		}),
		apiTestWorkstation(namespace, "smb-exposes-data-claim", func(workstation *Workstation) {
			workstation.Spec.Storage.Workspace = WorkspaceVolumeSource{
				Type:          WorkspaceVolumeExistingClaim,
				ExistingClaim: &ExistingClaimVolumeSource{Name: "t3-data"},
			}
			workstation.Spec.WorkspaceSharing = &WorkspaceSharing{SMB: &SMBWorkspaceShare{
				PasswordSecretRef: &SecretKeyReference{Name: "workspace-smb", Key: "password"},
			}}
		}),
	}
	for _, workstation := range invalidWorkstations {
		err := apiClient.Create(ctx, workstation)
		if err == nil {
			t.Fatalf("invalid Workstation %q was accepted", workstation.Name)
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("invalid Workstation %q returned %T: %v", workstation.Name, err, err)
		}
	}
}

func assertSMBWorkspaceSharingIsAcceptedAndDefaulted(t *testing.T, ctx context.Context, apiClient client.Client, namespace string) {
	t.Helper()
	workstation := apiTestWorkstation(namespace, "smb-workspace", func(workstation *Workstation) {
		workstation.Spec.WorkspaceSharing = &WorkspaceSharing{SMB: &SMBWorkspaceShare{
			PasswordSecretRef: &SecretKeyReference{Name: "workspace-smb", Key: "password"},
			Service:           &SMBServiceSpec{},
		}}
	})
	if err := apiClient.Create(ctx, workstation); err != nil {
		t.Fatal(err)
	}
	persisted := &Workstation{}
	if err := apiClient.Get(ctx, client.ObjectKeyFromObject(workstation), persisted); err != nil {
		t.Fatal(err)
	}
	share := persisted.Spec.WorkspaceSharing.SMB
	if share == nil || share.Username != "t3" || share.ShareName != "workspace" ||
		share.Service == nil || share.Service.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("SMB defaults were not applied: %#v", persisted.Spec.WorkspaceSharing)
	}
}

func apiTestWorkstation(namespace, name string, mutate func(*Workstation)) *Workstation {
	workstation := &Workstation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: WorkstationSpec{
			Image: "ghcr.io/janpuc/t3-code@sha256:" + strings.Repeat("a", 64),
			Storage: WorkstationStorage{
				Data: DataVolumeSource{
					Type:          DataVolumeExistingClaim,
					ExistingClaim: &ExistingClaimVolumeSource{Name: "t3-data"},
				},
				Workspace: WorkspaceVolumeSource{
					Type:          WorkspaceVolumeExistingClaim,
					ExistingClaim: &ExistingClaimVolumeSource{Name: "workspace"},
				},
			},
		},
	}
	mutate(workstation)
	return workstation
}

func assertInvalidMCPServersAreRejected(t *testing.T, ctx context.Context, apiClient client.Client, namespace string) {
	t.Helper()

	value := "inline"
	multilineValue := "safe\r\nInjected: true"
	invalidServers := []*MCPServer{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "two-header-values", Namespace: namespace},
			Spec: MCPServerSpec{
				Transport: "http",
				Headers: []HTTPHeader{{
					Name:  "Authorization",
					Value: &value,
					ValueFrom: &EnvironmentValueSource{SecretKeyRef: SecretKeyReference{
						Name: "credentials",
						Key:  "token",
					}},
				}},
				HarnessRefs: []LocalObjectReference{{Name: "codex"}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "duplicate-headers", Namespace: namespace},
			Spec: MCPServerSpec{
				Transport: "http",
				Headers: []HTTPHeader{
					{Name: "Authorization", Value: &value},
					{Name: "authorization", Value: &value},
				},
				HarnessRefs: []LocalObjectReference{{Name: "codex"}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "multiline-prefix", Namespace: namespace},
			Spec: MCPServerSpec{
				Transport: "http",
				Headers: []HTTPHeader{{
					Name:   "Authorization",
					Prefix: "Bearer\nInjected: true",
					Value:  &value,
				}},
				HarnessRefs: []LocalObjectReference{{Name: "codex"}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "multiline-value", Namespace: namespace},
			Spec: MCPServerSpec{
				Transport: "http",
				Headers: []HTTPHeader{{
					Name:  "Authorization",
					Value: &multilineValue,
				}},
				HarnessRefs: []LocalObjectReference{{Name: "codex"}},
			},
		},
	}

	for _, server := range invalidServers {
		err := apiClient.Create(ctx, server)
		if err == nil {
			t.Fatalf("invalid MCPServer %q was accepted", server.Name)
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("invalid MCPServer %q returned %T: %v", server.Name, err, err)
		}
	}
}

func assertOpaqueConfigRoundTripsThroughAPIServer(t *testing.T, ctx context.Context, apiClient client.Client, namespace string) {
	t.Helper()

	harnessConfig := []byte(`{"future":{"nested":{"enabled":true,"weights":[1,2,3]}}}`)
	harness := &Harness{
		ObjectMeta: metav1.ObjectMeta{Name: "future-driver", Namespace: namespace},
		Spec: HarnessSpec{
			InstanceID:      "future_driver",
			Driver:          "futureDriver",
			Config:          &apiextensionsv1.JSON{Raw: harnessConfig},
			WorkstationRefs: []LocalObjectReference{{Name: "primary"}},
		},
	}
	if err := apiClient.Create(ctx, harness); err != nil {
		t.Fatal(err)
	}
	harness.Spec.DisplayName = "Future driver"
	if err := apiClient.Update(ctx, harness); err != nil {
		t.Fatal(err)
	}
	persistedHarness := &Harness{}
	if err := apiClient.Get(ctx, client.ObjectKeyFromObject(harness), persistedHarness); err != nil {
		t.Fatal(err)
	}
	if persistedHarness.Spec.Config == nil {
		t.Fatal("Harness config was removed")
	}
	assertNestedJSONEqual(t, harnessConfig, persistedHarness.Spec.Config.Raw)

	serverConfig := []byte(`{"future":{"nested":{"enabled":true,"weights":[1,2,3]}}}`)
	server := &MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "future-transport", Namespace: namespace},
		Spec: MCPServerSpec{
			Transport:   "futureTransport",
			Config:      &apiextensionsv1.JSON{Raw: serverConfig},
			HarnessRefs: []LocalObjectReference{{Name: "codex"}},
		},
	}
	if err := apiClient.Create(ctx, server); err != nil {
		t.Fatal(err)
	}
	server.Spec.Headers = []HTTPHeader{}
	if err := apiClient.Update(ctx, server); err != nil {
		t.Fatal(err)
	}
	persistedServer := &MCPServer{}
	if err := apiClient.Get(ctx, client.ObjectKeyFromObject(server), persistedServer); err != nil {
		t.Fatal(err)
	}
	if persistedServer.Spec.Config == nil {
		t.Fatal("MCPServer config was removed")
	}
	assertNestedJSONEqual(t, serverConfig, persistedServer.Spec.Config.Raw)
}

func assertNFSWorkstationIsAcceptedAndDefaulted(t *testing.T, ctx context.Context, apiClient client.Client, namespace string) {
	t.Helper()

	workstation := &Workstation{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs-workspace", Namespace: namespace},
		Spec: WorkstationSpec{
			Image: "ghcr.io/janpuc/t3-code@sha256:" + strings.Repeat("a", 64),
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
			Environment: []WorkstationEnvironmentVariable{{Name: "TZ", Value: "Etc/UTC"}},
			MachineInfo: &MachineInfo{PrettyHostname: "Primary t3 workstation"},
			Storage: WorkstationStorage{
				Data: DataVolumeSource{
					Type:          DataVolumeExistingClaim,
					ExistingClaim: &ExistingClaimVolumeSource{Name: "t3-data"},
				},
				Workspace: WorkspaceVolumeSource{
					Type: WorkspaceVolumeNFS,
					NFS: &NFSVolumeSource{
						Server:     "nas.internal",
						ExportPath: "/workspace",
					},
				},
			},
		},
	}
	if err := apiClient.Create(ctx, workstation); err != nil {
		t.Fatal(err)
	}
	persisted := &Workstation{}
	if err := apiClient.Get(ctx, client.ObjectKeyFromObject(workstation), persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Spec.Storage.Workspace.NFS == nil || persisted.Spec.Storage.Workspace.NFS.ExportPath != "/workspace" {
		t.Fatalf("NFS workspace was not preserved: %#v", persisted.Spec.Storage.Workspace)
	}
	if persisted.Spec.Resources == nil || persisted.Spec.Resources.Requests.Cpu().String() != "500m" {
		t.Fatalf("resources were not preserved: %#v", persisted.Spec.Resources)
	}
	if len(persisted.Spec.Environment) != 1 || persisted.Spec.Environment[0].Name != "TZ" {
		t.Fatalf("startup environment was not preserved: %#v", persisted.Spec.Environment)
	}
	if persisted.Spec.MachineInfo == nil || persisted.Spec.MachineInfo.PrettyHostname != "Primary t3 workstation" {
		t.Fatalf("machine info was not preserved: %#v", persisted.Spec.MachineInfo)
	}
	if persisted.Spec.Drain == nil {
		t.Fatal("drain defaults were not applied")
	}
	if persisted.Spec.Drain.Policy != DrainPolicyWaitForIdle {
		t.Fatalf("unexpected drain policy default %q", persisted.Spec.Drain.Policy)
	}
	if persisted.Spec.Drain.Timeout == nil || persisted.Spec.Drain.Timeout.Duration != 30*time.Minute {
		t.Fatalf("unexpected drain timeout default %#v", persisted.Spec.Drain.Timeout)
	}
	if persisted.Spec.Drain.TimeoutAction != DrainTimeoutBlock {
		t.Fatalf("unexpected drain timeout action default %q", persisted.Spec.Drain.TimeoutAction)
	}

	invalid := workstation.DeepCopy()
	invalid.ResourceVersion = ""
	invalid.UID = ""
	invalid.Name = "non-disposable-data"
	invalid.Spec.Storage.Data = DataVolumeSource{
		Type:     DataVolumeEmptyDir,
		EmptyDir: &corev1.EmptyDirVolumeSource{},
	}
	err := apiClient.Create(ctx, invalid)
	if err == nil {
		t.Fatal("the API server accepted data EmptyDir without disposable=true")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("invalid Workstation returned %T: %v", err, err)
	}

	invalidSigning := workstation.DeepCopy()
	invalidSigning.ResourceVersion = ""
	invalidSigning.UID = ""
	invalidSigning.Name = "signing-without-email"
	invalidSigning.Spec.Git = &GitIdentity{
		SigningKeySecretRef: &GitSigningKeyReference{
			Name:          "git-signing",
			PrivateKeyKey: "private",
			PublicKeyKey:  "public",
		},
	}
	err = apiClient.Create(ctx, invalidSigning)
	if err == nil {
		t.Fatal("the API server accepted a signing key without a Git email")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("invalid signing Workstation returned %T: %v", err, err)
	}

	invalidGitHubCredential := workstation.DeepCopy()
	invalidGitHubCredential.ResourceVersion = ""
	invalidGitHubCredential.UID = ""
	invalidGitHubCredential.Name = "credential-without-user"
	invalidGitHubCredential.Spec.Git = &GitIdentity{
		CredentialSecretRef: &SecretKeyReference{Name: "github-token", Key: "token"},
	}
	err = apiClient.Create(ctx, invalidGitHubCredential)
	if err == nil {
		t.Fatal("the API server accepted a GitHub credential without a GitHub user")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("invalid GitHub credential Workstation returned %T: %v", err, err)
	}

	invalidEnvironment := workstation.DeepCopy()
	invalidEnvironment.ResourceVersion = ""
	invalidEnvironment.UID = ""
	invalidEnvironment.Name = "reserved-environment"
	invalidEnvironment.Spec.Environment = []WorkstationEnvironmentVariable{{Name: "PATH", Value: "/tmp/bin"}}
	err = apiClient.Create(ctx, invalidEnvironment)
	if err == nil {
		t.Fatal("the API server accepted a reserved runtime environment variable")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("invalid runtime environment Workstation returned %T: %v", err, err)
	}
}
