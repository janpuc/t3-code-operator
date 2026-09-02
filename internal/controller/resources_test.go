package controller

import (
	"reflect"
	"strings"
	"testing"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/render"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestWorkloadResourcesUseFirstClassNFSAndExactSidecarRBAC(t *testing.T) {
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	workstation.Spec.Storage.Workspace = t3v1alpha1.WorkspaceVolumeSource{
		Type: t3v1alpha1.WorkspaceVolumeNFS,
		NFS: &t3v1alpha1.NFSVolumeSource{
			Server:     "nas.internal",
			ExportPath: "/volume/workspace",
			ReadOnly:   true,
		},
	}
	resources, err := BuildWorkloadResources(workstation, controllerTestManifest(t, workstation), []string{"z-token", "a-token", "a-token"}, testWorkloadImages())
	if err != nil {
		t.Fatal(err)
	}
	var workspace corev1.Volume
	for _, volume := range resources.Deployment.Spec.Template.Spec.Volumes {
		if volume.Name == "workspace" {
			workspace = volume
		}
	}
	if workspace.NFS == nil || workspace.NFS.Server != "nas.internal" || workspace.NFS.Path != "/volume/workspace" || !workspace.NFS.ReadOnly {
		t.Fatalf("NFS workspace was not rendered: %#v", workspace)
	}
	for _, container := range resources.Deployment.Spec.Template.Spec.Containers {
		for _, mount := range container.VolumeMounts {
			if mount.Name == "workspace" && !mount.ReadOnly {
				t.Fatalf("container %s received a writable NFS mount", container.Name)
			}
		}
	}
	if len(resources.Role.Rules) != 3 {
		t.Fatalf("unexpected sidecar Role: %#v", resources.Role.Rules)
	}
	secretRule := resources.Role.Rules[2]
	if got, want := strings.Join(secretRule.ResourceNames, ","), "a-token,z-token"; got != want {
		t.Fatalf("Secret permissions mismatch: got=%q want=%q", got, want)
	}
	for _, rule := range resources.Role.Rules {
		for _, verb := range rule.Verbs {
			if verb == "list" {
				t.Fatalf("sidecar Role grants list: %#v", resources.Role.Rules)
			}
		}
	}
	containers := resources.Deployment.Spec.Template.Spec.Containers
	if len(containers) != 2 || containers[0].Name != "t3-code" || containers[1].Name != "t3-coded" {
		t.Fatalf("unexpected containers: %#v", containers)
	}
	for _, container := range containers {
		if !reflect.DeepEqual(container.Command, []string{"/usr/bin/tini", "--"}) {
			t.Fatalf("container %s bypasses tini: %#v", container.Name, container.Command)
		}
	}
	if len(containers[0].Args) == 0 || containers[0].Args[0] != "t3" ||
		len(containers[1].Args) == 0 || containers[1].Args[0] != "/usr/local/bin/t3-coded" {
		t.Fatalf("container processes are invalid: t3=%#v sidecar=%#v", containers[0].Args, containers[1].Args)
	}
	if !sliceContainsSequence(containers[1].Args, "--t3-url", "http://127.0.0.1:3773") {
		t.Fatalf("sidecar does not use the Service port: %#v", containers[1].Args)
	}
	podRevision := resources.Deployment.Annotations[podRevisionAnnotation]
	if podRevision == "" || resources.Deployment.Spec.Template.Annotations[podRevisionAnnotation] != podRevision {
		t.Fatalf("Pod revision identity is missing: %#v", resources.Deployment.Annotations)
	}
	if len(containers[1].Env) != 1 || containers[1].Env[0].Name != "T3_CODE_POD_REVISION" ||
		containers[1].Env[0].ValueFrom == nil || containers[1].Env[0].ValueFrom.FieldRef == nil ||
		containers[1].Env[0].ValueFrom.FieldRef.FieldPath != "metadata.annotations['"+podRevisionAnnotation+"']" {
		t.Fatalf("sidecar does not receive the Pod revision: %#v", containers[1].Env)
	}
	for _, container := range containers {
		if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil ||
			!*container.SecurityContext.ReadOnlyRootFilesystem || container.SecurityContext.Capabilities == nil ||
			!containsCapability(container.SecurityContext.Capabilities.Drop, "ALL") {
			t.Fatalf("container %s lacks the secure defaults: %#v", container.Name, container.SecurityContext)
		}
		for _, path := range []string{"/config", "/tmp"} {
			if !hasMountPath(container.VolumeMounts, path) {
				t.Fatalf("container %s lacks mount %s", container.Name, path)
			}
		}
	}
	if containers[1].ReadinessProbe == nil || containers[1].ReadinessProbe.HTTPGet == nil ||
		containers[1].ReadinessProbe.HTTPGet.Path != "/readyz" ||
		containers[1].ReadinessProbe.HTTPGet.Port.StrVal != "sidecar-health" {
		t.Fatalf("sidecar readiness probe is missing: %#v", containers[1].ReadinessProbe)
	}
	if containers[1].LivenessProbe == nil || containers[1].LivenessProbe.HTTPGet == nil ||
		containers[1].LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("sidecar liveness probe is missing: %#v", containers[1].LivenessProbe)
	}
}

func TestContentChangesCannotChangeThePodTemplate(t *testing.T) {
	baseline := controllerTestWorkstation()
	manifest := controllerTestManifest(t, baseline)
	baselineResources, err := BuildWorkloadResources(baseline, manifest, []string{"old-token"}, testWorkloadImages())
	if err != nil {
		t.Fatal(err)
	}

	content := baseline.DeepCopy()
	content.Spec.Tools = append(content.Spec.Tools, t3v1alpha1.ToolSpec{Name: "jq", Backend: "aqua:jqlang/jq", Version: "1.8.1"})
	content.Spec.MachineInfo = &t3v1alpha1.MachineInfo{PrettyHostname: "new-name"}
	content.Spec.Git = &t3v1alpha1.GitIdentity{UserName: "Agent", UserEmail: "agent@example.test"}
	content.Spec.Drain = &t3v1alpha1.DrainPolicy{TimeoutAction: t3v1alpha1.DrainTimeoutForce}
	changedManifest := manifest
	changedManifest.DesiredRevision = "sha256:" + strings.Repeat("d", 64)
	contentResources, err := BuildWorkloadResources(content, changedManifest, []string{"new-token"}, testWorkloadImages())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(baselineResources.Deployment.Spec.Template, contentResources.Deployment.Spec.Template) {
		t.Fatalf("content changed the pod template:\nbaseline=%#v\nchanged=%#v", baselineResources.Deployment.Spec.Template, contentResources.Deployment.Spec.Template)
	}
	if baselineResources.Manifest.Data["manifest.json"] == contentResources.Manifest.Data["manifest.json"] {
		t.Fatal("test did not change rendered content")
	}
}

func TestClaimBackedWorkspaceCanBeSharedThroughSMB(t *testing.T) {
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	workstation.Spec.Storage.Workspace = t3v1alpha1.WorkspaceVolumeSource{
		Type:          t3v1alpha1.WorkspaceVolumeExistingClaim,
		ExistingClaim: &t3v1alpha1.ExistingClaimVolumeSource{Name: "workspace-pvc"},
	}
	workstation.Spec.WorkspaceSharing = &t3v1alpha1.WorkspaceSharing{SMB: &t3v1alpha1.SMBWorkspaceShare{
		Username:          "developer",
		ShareName:         "projects",
		PasswordSecretRef: t3v1alpha1.SecretKeyReference{Name: "workspace-smb", Key: "password"},
		Service: &t3v1alpha1.SMBServiceSpec{
			Type:                     corev1.ServiceTypeLoadBalancer,
			Annotations:              map[string]string{"lb.example.test/pool": "lan"},
			ExternalTrafficPolicy:    corev1.ServiceExternalTrafficPolicyLocal,
			LoadBalancerSourceRanges: []string{"192.0.2.0/24"},
		},
	}}
	resources, err := BuildWorkloadResources(workstation, controllerTestManifest(t, workstation), nil, testWorkloadImages())
	if err != nil {
		t.Fatal(err)
	}
	containers := resources.Deployment.Spec.Template.Spec.Containers
	if len(containers) != 3 || containers[2].Name != "workspace-smb" {
		t.Fatalf("SMB container is missing: %#v", containers)
	}
	smb := containers[2]
	if smb.Image != testWorkloadImages().SMB || smb.Image == workstation.Spec.Image ||
		!sliceContainsSequence(smb.Args, "--username", "developer") ||
		!sliceContainsSequence(smb.Args, "--share-name", "projects") ||
		!sliceContainsSequence(smb.Args, "--server-identity", string(workstation.UID)) {
		t.Fatalf("SMB container contract is wrong: %#v", smb)
	}
	if hasMountPath(smb.VolumeMounts, "/data") || !hasMountPath(smb.VolumeMounts, "/workspace") ||
		!hasMountPath(smb.VolumeMounts, "/var/run/secrets/t3-smb") {
		t.Fatalf("SMB mounts are not isolated: %#v", smb.VolumeMounts)
	}
	if smb.SecurityContext == nil || smb.SecurityContext.RunAsUser == nil || *smb.SecurityContext.RunAsUser != 0 ||
		smb.SecurityContext.RunAsNonRoot == nil || *smb.SecurityContext.RunAsNonRoot ||
		smb.SecurityContext.AllowPrivilegeEscalation == nil || *smb.SecurityContext.AllowPrivilegeEscalation ||
		smb.SecurityContext.ReadOnlyRootFilesystem == nil || !*smb.SecurityContext.ReadOnlyRootFilesystem ||
		smb.SecurityContext.Capabilities == nil || !containsCapability(smb.SecurityContext.Capabilities.Drop, "ALL") ||
		!containsCapability(smb.SecurityContext.Capabilities.Add, "SETUID") ||
		!containsCapability(smb.SecurityContext.Capabilities.Add, "SETGID") {
		t.Fatalf("SMB security context is wrong: %#v", smb.SecurityContext)
	}
	var credentialVolume *corev1.SecretVolumeSource
	for _, volume := range resources.Deployment.Spec.Template.Spec.Volumes {
		if volume.Name == "smb-credentials" {
			credentialVolume = volume.Secret
		}
	}
	if credentialVolume == nil || credentialVolume.SecretName != "workspace-smb" || len(credentialVolume.Items) != 1 ||
		credentialVolume.Items[0].Key != "password" || credentialVolume.Items[0].Path != "password" {
		t.Fatalf("SMB credential projection is wrong: %#v", credentialVolume)
	}
	if resources.SMBService == nil || resources.SMBService.Name != NamesForWorkstation(workstation.Name).SMBService ||
		resources.SMBService.Spec.Type != corev1.ServiceTypeLoadBalancer ||
		resources.SMBService.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal ||
		len(resources.SMBService.Spec.Ports) != 1 || resources.SMBService.Spec.Ports[0].Port != 445 ||
		resources.SMBService.Spec.Ports[0].TargetPort.StrVal != "smb" ||
		resources.SMBService.Annotations["lb.example.test/pool"] != "lan" {
		t.Fatalf("SMB Service is wrong: %#v", resources.SMBService)
	}
	if len(resources.Role.Rules) != 2 {
		t.Fatalf("SMB Secret access leaked into the t3-coded Role: %#v", resources.Role.Rules)
	}
}

func TestFilesystemMountsRejectBlockClaimTemplates(t *testing.T) {
	workstation := controllerTestWorkstation()
	block := corev1.PersistentVolumeBlock
	workstation.Spec.Storage.Workspace = t3v1alpha1.WorkspaceVolumeSource{
		Type: t3v1alpha1.WorkspaceVolumeClaimTemplate,
		ClaimTemplate: &t3v1alpha1.ClaimTemplateVolumeSource{Spec: corev1.PersistentVolumeClaimSpec{
			VolumeMode:  &block,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		}},
	}
	if _, err := BuildWorkloadResources(workstation, controllerTestManifest(t, workstation), nil, testWorkloadImages()); err == nil ||
		!strings.Contains(err.Error(), "volumeMode must be Filesystem") {
		t.Fatalf("expected block volume rejection, got %v", err)
	}
}

func TestSMBRejectsAWorkspaceThatReusesTheDataClaim(t *testing.T) {
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	workstation.Spec.Storage.Data = t3v1alpha1.DataVolumeSource{
		Type:          t3v1alpha1.DataVolumeExistingClaim,
		ExistingClaim: &t3v1alpha1.ExistingClaimVolumeSource{Name: "runtime-data"},
	}
	workstation.Spec.Storage.Workspace = t3v1alpha1.WorkspaceVolumeSource{
		Type:          t3v1alpha1.WorkspaceVolumeExistingClaim,
		ExistingClaim: &t3v1alpha1.ExistingClaimVolumeSource{Name: "runtime-data"},
	}
	workstation.Spec.WorkspaceSharing = &t3v1alpha1.WorkspaceSharing{SMB: &t3v1alpha1.SMBWorkspaceShare{
		PasswordSecretRef: t3v1alpha1.SecretKeyReference{Name: "workspace-smb", Key: "password"},
	}}
	if _, err := BuildWorkloadResources(workstation, controllerTestManifest(t, workstation), nil, testWorkloadImages()); err == nil ||
		!strings.Contains(err.Error(), "must use different claims") {
		t.Fatalf("expected data claim exposure rejection, got %v", err)
	}
}

func TestSMBServiceChangesDoNotRollTheWorkstation(t *testing.T) {
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	workstation.Spec.Storage.Workspace = t3v1alpha1.WorkspaceVolumeSource{
		Type:          t3v1alpha1.WorkspaceVolumeExistingClaim,
		ExistingClaim: &t3v1alpha1.ExistingClaimVolumeSource{Name: "workspace-pvc"},
	}
	workstation.Spec.WorkspaceSharing = &t3v1alpha1.WorkspaceSharing{SMB: &t3v1alpha1.SMBWorkspaceShare{
		PasswordSecretRef: t3v1alpha1.SecretKeyReference{Name: "workspace-smb", Key: "password"},
		Service:           &t3v1alpha1.SMBServiceSpec{Type: corev1.ServiceTypeClusterIP},
	}}
	manifest := controllerTestManifest(t, workstation)
	baseline, err := BuildWorkloadResources(workstation, manifest, nil, testWorkloadImages())
	if err != nil {
		t.Fatal(err)
	}
	changed := workstation.DeepCopy()
	changed.Spec.WorkspaceSharing.SMB.Service = &t3v1alpha1.SMBServiceSpec{
		Type:        corev1.ServiceTypeLoadBalancer,
		Annotations: map[string]string{"lb.example.test/pool": "lan"},
	}
	updated, err := BuildWorkloadResources(changed, manifest, nil, testWorkloadImages())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(baseline.Deployment.Spec.Template, updated.Deployment.Spec.Template) {
		t.Fatal("an SMB Service change altered the Pod template")
	}
	if reflect.DeepEqual(baseline.SMBService.Spec, updated.SMBService.Spec) {
		t.Fatal("test did not alter the SMB Service")
	}
}

func TestSMBSharingRejectsNonClaimWorkspace(t *testing.T) {
	workstation := controllerTestWorkstation()
	workstation.Spec.Storage.Workspace = t3v1alpha1.WorkspaceVolumeSource{
		Type:     t3v1alpha1.WorkspaceVolumeEmptyDir,
		EmptyDir: &corev1.EmptyDirVolumeSource{},
	}
	workstation.Spec.WorkspaceSharing = &t3v1alpha1.WorkspaceSharing{SMB: &t3v1alpha1.SMBWorkspaceShare{
		PasswordSecretRef: t3v1alpha1.SecretKeyReference{Name: "workspace-smb", Key: "password"},
	}}
	_, err := BuildWorkloadResources(workstation, controllerTestManifest(t, workstation), nil, testWorkloadImages())
	if err == nil || !strings.Contains(err.Error(), "claim-backed") {
		t.Fatalf("expected claim-backed workspace rejection, got %v", err)
	}
}

func TestEveryPodShapeFieldChangesThePodTemplate(t *testing.T) {
	baseline := controllerTestWorkstation()
	baseline.Spec.Tools = nil
	baseline.Spec.Storage.Workspace = t3v1alpha1.WorkspaceVolumeSource{
		Type:          t3v1alpha1.WorkspaceVolumeExistingClaim,
		ExistingClaim: &t3v1alpha1.ExistingClaimVolumeSource{Name: "workspace-base"},
	}
	manifest := controllerTestManifest(t, baseline)
	baselineResources, err := BuildWorkloadResources(baseline, manifest, nil, testWorkloadImages())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*t3v1alpha1.Workstation){
		"image": func(value *t3v1alpha1.Workstation) {
			value.Spec.Image = "registry.example/t3@sha256:" + strings.Repeat("e", 64)
		},
		"service-account": func(value *t3v1alpha1.Workstation) {
			value.Spec.ServiceAccountName = "agent-runtime"
		},
		"security": func(value *t3v1alpha1.Workstation) {
			runAsUser := int64(2000)
			value.Spec.SecurityContext = &t3v1alpha1.WorkstationSecurityContext{RunAsUser: &runAsUser}
		},
		"resources": func(value *t3v1alpha1.Workstation) {
			value.Spec.Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}}
		},
		"environment": func(value *t3v1alpha1.Workstation) {
			value.Spec.Environment = []t3v1alpha1.WorkstationEnvironmentVariable{{Name: "STARTUP_MODE", Value: "test"}}
		},
		"storage": func(value *t3v1alpha1.Workstation) {
			value.Spec.Storage.Workspace = t3v1alpha1.WorkspaceVolumeSource{
				Type:          t3v1alpha1.WorkspaceVolumeExistingClaim,
				ExistingClaim: &t3v1alpha1.ExistingClaimVolumeSource{Name: "workspace-pvc"},
			}
		},
		"workspace-sharing": func(value *t3v1alpha1.Workstation) {
			value.Spec.WorkspaceSharing = &t3v1alpha1.WorkspaceSharing{SMB: &t3v1alpha1.SMBWorkspaceShare{
				PasswordSecretRef: t3v1alpha1.SecretKeyReference{Name: "workspace-smb", Key: "password"},
			}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := baseline.DeepCopy()
			change(changed)
			resources, err := BuildWorkloadResources(changed, manifest, nil, testWorkloadImages())
			if err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(baselineResources.Deployment.Spec.Template, resources.Deployment.Spec.Template) {
				t.Fatalf("%s did not change the pod template", name)
			}
		})
	}
}

func hasMountPath(mounts []corev1.VolumeMount, path string) bool {
	for _, mount := range mounts {
		if mount.MountPath == path {
			return true
		}
	}
	return false
}

func containsCapability(capabilities []corev1.Capability, expected corev1.Capability) bool {
	for _, capability := range capabilities {
		if capability == expected {
			return true
		}
	}
	return false
}

func TestClaimTemplateIsRetainedAndIdentified(t *testing.T) {
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	workstation.Spec.Disposable = false
	workstation.Spec.Storage.Data = t3v1alpha1.DataVolumeSource{
		Type: t3v1alpha1.DataVolumeClaimTemplate,
		ClaimTemplate: &t3v1alpha1.ClaimTemplateVolumeSource{
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}},
			},
		},
	}
	resources, err := BuildWorkloadResources(workstation, controllerTestManifest(t, workstation), nil, testWorkloadImages())
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.Claims) != 1 {
		t.Fatalf("expected one generated claim, got %#v", resources.Claims)
	}
	claim := resources.Claims[0]
	if len(claim.OwnerReferences) != 0 || claim.Annotations[claimWorkstationAnnotation] != workstation.Name ||
		claim.Annotations[claimWorkstationUIDAnnotation] != string(workstation.UID) ||
		claim.Annotations[claimRetentionAnnotation] != string(t3v1alpha1.ClaimRetentionRetain) {
		t.Fatalf("claim retention identity is wrong: %#v", claim.ObjectMeta)
	}
}

func TestResourceNamesStayWithinDNSLabelLimit(t *testing.T) {
	name := strings.Repeat("a", 250)
	first := NamesForWorkstation(name)
	second := NamesForWorkstation(name)
	if first != second {
		t.Fatalf("resource names are not deterministic: first=%#v second=%#v", first, second)
	}
	for _, value := range []string{first.Base, first.Manifest, first.Report, first.SidecarRole, first.DataClaim, first.WorkspaceClaim, first.SMBService} {
		if len(value) == 0 || len(value) > 63 {
			t.Fatalf("invalid bounded resource name %q", value)
		}
	}
}

func TestReservedRuntimeEnvironmentCannotBeOverridden(t *testing.T) {
	for _, name := range []string{
		"HOME", "PATH", "T3CODE_HOME",
		"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
		"GH_CONFIG_DIR", "GH_HOST", "GH_GIT_PROTOCOL", "GH_TOKEN", "GITHUB_TOKEN",
	} {
		workstation := controllerTestWorkstation()
		workstation.Spec.Environment = []t3v1alpha1.WorkstationEnvironmentVariable{{Name: name, Value: "/tmp/replace"}}
		_, err := BuildWorkloadResources(workstation, controllerTestManifest(t, workstation), nil, testWorkloadImages())
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("expected reserved environment rejection for %s, got %v", name, err)
		}
	}
}

func controllerTestManifest(t *testing.T, workstation *t3v1alpha1.Workstation) render.Manifest {
	t.Helper()
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: workstation.Namespace,
		Name:      workstation.Name,
		UID:       string(workstation.UID),
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func sliceContainsSequence(values []string, first, second string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == first && values[index+1] == second {
			return true
		}
	}
	return false
}

func testWorkloadImages() WorkloadImages {
	return WorkloadImages{SMB: "ghcr.io/janpuc/t3-code-smbd@sha256:" + strings.Repeat("e", 64)}
}
