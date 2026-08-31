package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/render"
	"github.com/janpuc/t3-code-operator/internal/sidecar"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	workstationFinalizer          = "t3code.janpuc.com/drain"
	t3Port                        = 3773
	sidecarProbePort              = 8082
	smbContainerPort              = 1445
	smbServicePort                = 445
	podRevisionAnnotation         = "t3code.janpuc.com/pod-revision"
	claimWorkstationAnnotation    = "t3code.janpuc.com/workstation"
	claimWorkstationUIDAnnotation = "t3code.janpuc.com/workstation-uid"
	claimVolumeAnnotation         = "t3code.janpuc.com/volume"
	claimRetentionAnnotation      = "t3code.janpuc.com/retention-policy"
)

var controllerTrue = true

type ResourceNames struct {
	Base           string
	Manifest       string
	Report         string
	SidecarRole    string
	DataClaim      string
	WorkspaceClaim string
	SMBService     string
}

type WorkloadResources struct {
	Manifest    *corev1.ConfigMap
	Report      *corev1.ConfigMap
	Role        *rbacv1.Role
	RoleBinding *rbacv1.RoleBinding
	Service     *corev1.Service
	SMBService  *corev1.Service
	PDB         *policyv1.PodDisruptionBudget
	Deployment  *appsv1.Deployment
	Claims      []*corev1.PersistentVolumeClaim
}

func NamesForWorkstation(name string) ResourceNames {
	base := boundedResourceName(name, "")
	return ResourceNames{
		Base:           base,
		Manifest:       boundedResourceName(name, "manifest"),
		Report:         boundedResourceName(name, "status"),
		SidecarRole:    boundedResourceName(name, "sidecar"),
		DataClaim:      boundedResourceName(name, "data"),
		WorkspaceClaim: boundedResourceName(name, "workspace"),
		SMBService:     boundedResourceName(name, "smb"),
	}
}

func boundedResourceName(name, suffix string) string {
	candidate := name
	if suffix != "" {
		candidate += "-" + suffix
	}
	if len(candidate) <= 63 {
		return candidate
	}
	digest := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(digest[:4])
	reserved := len(hash) + 1
	if suffix != "" {
		reserved += len(suffix) + 1
	}
	prefix := strings.TrimRight(name[:63-reserved], "-.")
	result := prefix + "-" + hash
	if suffix != "" {
		result += "-" + suffix
	}
	return result
}

func BuildWorkloadResources(
	workstation *t3v1alpha1.Workstation,
	manifest render.Manifest,
	secretNames []string,
) (WorkloadResources, error) {
	if workstation == nil {
		return WorkloadResources{}, errors.New("Workstation is required")
	}
	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		return WorkloadResources{}, err
	}
	if len(rawManifest) > render.MaxRenderedManifestBytes {
		return WorkloadResources{}, errors.New("rendered manifest exceeds its size limit")
	}
	names := NamesForWorkstation(workstation.Name)
	labels := workstationLabels(names.Base)
	owner := workstationOwner(workstation)

	volumes, mounts, claims, err := buildStorage(workstation, names, labels)
	if err != nil {
		return WorkloadResources{}, err
	}
	containers, err := buildContainers(workstation, names, mounts)
	if err != nil {
		return WorkloadResources{}, err
	}
	smbContainer, smbVolumes, smbService, err := buildSMBWorkspaceResources(workstation, names, labels, owner)
	if err != nil {
		return WorkloadResources{}, err
	}
	if smbContainer != nil {
		containers = append(containers, *smbContainer)
		volumes = append(volumes, smbVolumes...)
	}
	roleSecrets := append([]string(nil), secretNames...)
	sort.Strings(roleSecrets)
	roleSecrets = compactStrings(roleSecrets)

	resources := WorkloadResources{
		Manifest: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: workstation.Namespace, Name: names.Manifest, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}},
			Data:       map[string]string{sidecar.ManifestDataKey: string(rawManifest)},
		},
		Report: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: workstation.Namespace, Name: names.Report, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}},
		},
		Role: buildSidecarRole(workstation, names, labels, owner, roleSecrets),
		RoleBinding: &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: workstation.Namespace, Name: names.SidecarRole, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: names.SidecarRole},
			Subjects: []rbacv1.Subject{{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      serviceAccountName(workstation),
				Namespace: workstation.Namespace,
			}},
		},
		Service: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: workstation.Namespace, Name: names.Base, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}},
			Spec: corev1.ServiceSpec{
				Selector: labels,
				Type:     corev1.ServiceTypeClusterIP,
				Ports:    []corev1.ServicePort{{Name: "http", Port: t3Port, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP}},
			},
		},
		SMBService: smbService,
		PDB: &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Namespace: workstation.Namespace, Name: names.Base, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
				Selector:     &metav1.LabelSelector{MatchLabels: labels},
			},
		},
		Deployment: &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: workstation.Namespace, Name: names.Base, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Pointer(1),
				Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						ServiceAccountName:            workstation.Spec.ServiceAccountName,
						SecurityContext:               buildPodSecurityContext(workstation.Spec.SecurityContext),
						TerminationGracePeriodSeconds: int64Pointer(120),
						Containers:                    containers,
						Volumes:                       volumes,
					},
				},
			},
		},
		Claims: claims,
	}
	podRevision, err := podTemplateRevision(resources.Deployment.Spec.Template)
	if err != nil {
		return WorkloadResources{}, err
	}
	resources.Deployment.Spec.Template.Annotations = map[string]string{podRevisionAnnotation: podRevision}
	resources.Deployment.Annotations = map[string]string{podRevisionAnnotation: podRevision}
	return resources, nil
}

func buildSMBWorkspaceResources(
	workstation *t3v1alpha1.Workstation,
	names ResourceNames,
	labels map[string]string,
	owner metav1.OwnerReference,
) (*corev1.Container, []corev1.Volume, *corev1.Service, error) {
	if workstation.Spec.WorkspaceSharing == nil || workstation.Spec.WorkspaceSharing.SMB == nil {
		return nil, nil, nil, nil
	}
	if workstation.Spec.Storage.Workspace.Type != t3v1alpha1.WorkspaceVolumeExistingClaim &&
		workstation.Spec.Storage.Workspace.Type != t3v1alpha1.WorkspaceVolumeClaimTemplate {
		return nil, nil, nil, errors.New("SMB workspace sharing requires a claim-backed workspace")
	}
	if workstation.UID == "" {
		return nil, nil, nil, errors.New("SMB workspace sharing requires a persisted Workstation UID")
	}
	dataClaim, workspaceClaim := claimStatusNames(workstation)
	if dataClaim != "" && dataClaim == workspaceClaim {
		return nil, nil, nil, errors.New("SMB workspace and data must use different claims")
	}
	share := workstation.Spec.WorkspaceSharing.SMB
	if share.PasswordSecretRef.Name == "" || share.PasswordSecretRef.Key == "" {
		return nil, nil, nil, errors.New("SMB password Secret reference is required")
	}
	username := share.Username
	if username == "" {
		username = "t3"
	}
	shareName := share.ShareName
	if shareName == "" {
		shareName = "workspace"
	}
	resources := corev1.ResourceRequirements{}
	if share.Resources != nil {
		resources = *share.Resources.DeepCopy()
	}
	readOnlyRoot := true
	allowPrivilegeEscalation := false
	runAsNonRoot := false
	runAsUser := int64(0)
	runAsGroup := int64(0)
	secretMode := int32(0o400)
	container := &corev1.Container{
		Name:            "workspace-smb",
		Image:           workstation.Spec.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/usr/bin/tini", "--"},
		Args: []string{
			"/usr/local/bin/t3-smbd",
			"--username", username,
			"--share-name", shareName,
			"--server-identity", string(workstation.UID),
			"--password-file", "/var/run/secrets/t3-smb/password",
			"--workspace", "/workspace",
			"--state-directory", "/var/lib/t3-smb",
			"--port", fmt.Sprint(smbContainerPort),
			"--read-only=" + fmt.Sprint(share.ReadOnly),
		},
		Resources: resources,
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             &runAsNonRoot,
			RunAsUser:                &runAsUser,
			RunAsGroup:               &runAsGroup,
			AllowPrivilegeEscalation: &allowPrivilegeEscalation,
			ReadOnlyRootFilesystem:   &readOnlyRoot,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"SETGID", "SETUID"},
			},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Ports: []corev1.ContainerPort{{Name: "smb", ContainerPort: smbContainerPort, Protocol: corev1.ProtocolTCP}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace", ReadOnly: share.ReadOnly},
			{Name: "smb-credentials", MountPath: "/var/run/secrets/t3-smb", ReadOnly: true},
			{Name: "smb-state", MountPath: "/var/lib/t3-smb"},
			{Name: "smb-tmp", MountPath: "/tmp"},
		},
		StartupProbe:   tcpProbe("smb", 5, 60),
		ReadinessProbe: tcpProbe("smb", 5, 3),
		LivenessProbe:  tcpProbe("smb", 10, 3),
	}
	volumes := []corev1.Volume{
		{
			Name: "smb-credentials",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: share.PasswordSecretRef.Name,
				Items:      []corev1.KeyToPath{{Key: share.PasswordSecretRef.Key, Path: "password", Mode: &secretMode}},
			}},
		},
		{Name: "smb-state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "smb-tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	serviceSpec := share.Service
	serviceType := corev1.ServiceTypeClusterIP
	var externalTrafficPolicy corev1.ServiceExternalTrafficPolicyType
	var sourceRanges []string
	annotations := map[string]string{}
	if serviceSpec != nil {
		if serviceSpec.Type != "" {
			serviceType = serviceSpec.Type
		}
		externalTrafficPolicy = serviceSpec.ExternalTrafficPolicy
		sourceRanges = append([]string(nil), serviceSpec.LoadBalancerSourceRanges...)
		annotations = cloneStringMap(serviceSpec.Annotations)
	}
	if externalTrafficPolicy == "" && (serviceType == corev1.ServiceTypeNodePort || serviceType == corev1.ServiceTypeLoadBalancer) {
		externalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       workstation.Namespace,
			Name:            names.SMBService,
			Labels:          labels,
			Annotations:     managedServiceAnnotations(annotations),
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: corev1.ServiceSpec{
			Selector:                 labels,
			Type:                     serviceType,
			ExternalTrafficPolicy:    externalTrafficPolicy,
			LoadBalancerSourceRanges: sourceRanges,
			Ports: []corev1.ServicePort{{
				Name:       "smb",
				Port:       smbServicePort,
				TargetPort: intstr.FromString("smb"),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	return container, volumes, service, nil
}

func podTemplateRevision(template corev1.PodTemplateSpec) (string, error) {
	raw, err := json.Marshal(template)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func workstationLabels(instance string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "t3-code",
		"app.kubernetes.io/instance":   instance,
		"app.kubernetes.io/managed-by": "t3-code-operator",
	}
}

func workstationOwner(workstation *t3v1alpha1.Workstation) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         t3v1alpha1.GroupVersion.String(),
		Kind:               "Workstation",
		Name:               workstation.Name,
		UID:                workstation.UID,
		Controller:         &controllerTrue,
		BlockOwnerDeletion: &controllerTrue,
	}
}

func buildSidecarRole(
	workstation *t3v1alpha1.Workstation,
	names ResourceNames,
	labels map[string]string,
	owner metav1.OwnerReference,
	secretNames []string,
) *rbacv1.Role {
	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{names.Manifest}, Verbs: []string{"get", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{names.Report}, Verbs: []string{"patch"}},
	}
	if len(secretNames) != 0 {
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: secretNames,
			Verbs:         []string{"get", "watch"},
		})
	}
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: workstation.Namespace, Name: names.SidecarRole, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}},
		Rules:      rules,
	}
}

func buildContainers(
	workstation *t3v1alpha1.Workstation,
	names ResourceNames,
	mounts []corev1.VolumeMount,
) ([]corev1.Container, error) {
	environment := []corev1.EnvVar{
		{Name: "T3CODE_HOME", Value: "/data/t3"},
		{Name: "GH_CONFIG_DIR", Value: "/data/t3-coded/gh"},
	}
	for _, variable := range workstation.Spec.Environment {
		if reservedRuntimeEnvironment(variable.Name) {
			return nil, fmt.Errorf("Workstation environment variable %s is reserved", variable.Name)
		}
		environment = append(environment, corev1.EnvVar{Name: variable.Name, Value: variable.Value})
	}
	security := buildContainerSecurityContext(workstation.Spec.SecurityContext)
	resources := corev1.ResourceRequirements{}
	if workstation.Spec.Resources != nil {
		resources = *workstation.Spec.Resources.DeepCopy()
	}
	return []corev1.Container{
		{
			Name:            "t3-code",
			Image:           workstation.Spec.Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"/usr/bin/tini", "--"},
			Args: []string{
				"t3", "start", "--mode", "web", "--host", "0.0.0.0", "--port", fmt.Sprint(t3Port),
				"--base-dir", "/data/t3", "--no-browser", "/workspace",
			},
			Env:             environment,
			Resources:       resources,
			SecurityContext: security,
			Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: t3Port, Protocol: corev1.ProtocolTCP}},
			VolumeMounts:    mounts,
			StartupProbe:    httpProbe(5, 60),
			ReadinessProbe:  httpProbe(5, 3),
			LivenessProbe:   httpProbe(10, 3),
		},
		{
			Name:            "t3-coded",
			Image:           workstation.Spec.Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"/usr/bin/tini", "--"},
			Args: []string{
				"/usr/local/bin/t3-coded",
				"--namespace", workstation.Namespace,
				"--workstation-name", workstation.Name,
				"--workstation-uid", string(workstation.UID),
				"--manifest-configmap", names.Manifest,
				"--report-configmap", names.Report,
				"--t3-url", fmt.Sprintf("http://127.0.0.1:%d", t3Port),
				"--probe-bind-address", fmt.Sprintf(":%d", sidecarProbePort),
			},
			Env: []corev1.EnvVar{{
				Name: "T3_CODE_POD_REVISION",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.annotations['" + podRevisionAnnotation + "']",
				}},
			}},
			SecurityContext: security.DeepCopy(),
			VolumeMounts:    mounts,
			Ports: []corev1.ContainerPort{{
				Name:          "sidecar-health",
				ContainerPort: sidecarProbePort,
				Protocol:      corev1.ProtocolTCP,
			}},
			StartupProbe:   sidecarHTTPProbe("/healthz", 5, 60),
			ReadinessProbe: sidecarHTTPProbe("/readyz", 5, 3),
			LivenessProbe:  sidecarHTTPProbe("/healthz", 10, 3),
		},
	}, nil
}

func reservedRuntimeEnvironment(name string) bool {
	switch name {
	case "HOME", "PATH", "T3CODE_HOME",
		"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
		"GH_CONFIG_DIR", "GH_HOST", "GH_GIT_PROTOCOL", "GH_TOKEN", "GITHUB_TOKEN":
		return true
	default:
		return false
	}
}

func sidecarHTTPProbe(path string, periodSeconds, failureThreshold int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: path,
			Port: intstr.FromString("sidecar-health"),
		}},
		PeriodSeconds:    periodSeconds,
		TimeoutSeconds:   2,
		FailureThreshold: failureThreshold,
	}
}

func tcpProbe(port string, periodSeconds, failureThreshold int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(port)}},
		PeriodSeconds:    periodSeconds,
		TimeoutSeconds:   2,
		FailureThreshold: failureThreshold,
	}
}

func httpProbe(periodSeconds, failureThreshold int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/",
			Port: intstr.FromString("http"),
		}},
		PeriodSeconds:    periodSeconds,
		TimeoutSeconds:   2,
		FailureThreshold: failureThreshold,
	}
}

func buildPodSecurityContext(input *t3v1alpha1.WorkstationSecurityContext) *corev1.PodSecurityContext {
	runAsNonRoot := boolPointer(true)
	runAsUser := int64Pointer(1000)
	runAsGroup := int64Pointer(1000)
	fsGroup := int64Pointer(1000)
	seccompProfile := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	supplementalGroups := []int64(nil)
	if input != nil {
		if input.RunAsNonRoot != nil {
			runAsNonRoot = cloneBool(input.RunAsNonRoot)
		}
		if input.RunAsUser != nil {
			runAsUser = cloneInt64(input.RunAsUser)
		}
		if input.RunAsGroup != nil {
			runAsGroup = cloneInt64(input.RunAsGroup)
			fsGroup = cloneInt64(input.RunAsGroup)
		}
		if input.SeccompProfile != nil {
			seccompProfile = input.SeccompProfile.DeepCopy()
		}
		supplementalGroups = append([]int64(nil), input.SupplementalGroups...)
	}
	fsGroupChangePolicy := corev1.FSGroupChangeOnRootMismatch
	return &corev1.PodSecurityContext{
		RunAsNonRoot:        runAsNonRoot,
		RunAsUser:           runAsUser,
		RunAsGroup:          runAsGroup,
		FSGroup:             fsGroup,
		FSGroupChangePolicy: &fsGroupChangePolicy,
		SupplementalGroups:  supplementalGroups,
		SeccompProfile:      seccompProfile,
	}
}

func buildContainerSecurityContext(input *t3v1alpha1.WorkstationSecurityContext) *corev1.SecurityContext {
	runAsNonRoot := boolPointer(true)
	runAsUser := int64Pointer(1000)
	runAsGroup := int64Pointer(1000)
	allowPrivilegeEscalation := boolPointer(false)
	readOnlyRootFilesystem := boolPointer(true)
	capabilities := &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
	seccompProfile := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	if input != nil {
		if input.RunAsNonRoot != nil {
			runAsNonRoot = cloneBool(input.RunAsNonRoot)
		}
		if input.RunAsUser != nil {
			runAsUser = cloneInt64(input.RunAsUser)
		}
		if input.RunAsGroup != nil {
			runAsGroup = cloneInt64(input.RunAsGroup)
		}
		if input.AllowPrivilegeEscalation != nil {
			allowPrivilegeEscalation = cloneBool(input.AllowPrivilegeEscalation)
		}
		if input.ReadOnlyRootFilesystem != nil {
			readOnlyRootFilesystem = cloneBool(input.ReadOnlyRootFilesystem)
		}
		if input.Capabilities != nil {
			capabilities = input.Capabilities.DeepCopy()
		}
		if input.SeccompProfile != nil {
			seccompProfile = input.SeccompProfile.DeepCopy()
		}
	}
	return &corev1.SecurityContext{
		RunAsNonRoot:             runAsNonRoot,
		RunAsUser:                runAsUser,
		RunAsGroup:               runAsGroup,
		AllowPrivilegeEscalation: allowPrivilegeEscalation,
		ReadOnlyRootFilesystem:   readOnlyRootFilesystem,
		Capabilities:             capabilities,
		SeccompProfile:           seccompProfile,
	}
}

func buildStorage(
	workstation *t3v1alpha1.Workstation,
	names ResourceNames,
	labels map[string]string,
) ([]corev1.Volume, []corev1.VolumeMount, []*corev1.PersistentVolumeClaim, error) {
	data, dataClaim, err := buildDataVolume(workstation, names, labels)
	if err != nil {
		return nil, nil, nil, err
	}
	workspace, workspaceClaim, workspaceReadOnly, err := buildWorkspaceVolume(workstation, names, labels)
	if err != nil {
		return nil, nil, nil, err
	}
	claims := make([]*corev1.PersistentVolumeClaim, 0, 2)
	if dataClaim != nil {
		claims = append(claims, dataClaim)
	}
	if workspaceClaim != nil {
		claims = append(claims, workspaceClaim)
	}
	return []corev1.Volume{
			data,
			workspace,
			{Name: "config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}, []corev1.VolumeMount{
			{Name: "data", MountPath: "/data"},
			{Name: "workspace", MountPath: "/workspace", ReadOnly: workspaceReadOnly},
			{Name: "config", MountPath: "/config"},
			{Name: "tmp", MountPath: "/tmp"},
		}, claims, nil
}

func buildDataVolume(
	workstation *t3v1alpha1.Workstation,
	names ResourceNames,
	labels map[string]string,
) (corev1.Volume, *corev1.PersistentVolumeClaim, error) {
	source := workstation.Spec.Storage.Data
	volume := corev1.Volume{Name: "data"}
	switch source.Type {
	case t3v1alpha1.DataVolumeExistingClaim:
		if source.ExistingClaim == nil {
			return volume, nil, errors.New("data ExistingClaim configuration is required")
		}
		volume.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: source.ExistingClaim.Name}
	case t3v1alpha1.DataVolumeClaimTemplate:
		if source.ClaimTemplate == nil {
			return volume, nil, errors.New("data ClaimTemplate configuration is required")
		}
		if err := validateClaimTemplateVolumeMode(source.ClaimTemplate); err != nil {
			return volume, nil, fmt.Errorf("data %w", err)
		}
		claim := buildClaim(workstation, names.DataClaim, "data", source.ClaimTemplate, labels)
		volume.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}
		return volume, claim, nil
	case t3v1alpha1.DataVolumeEmptyDir:
		if source.EmptyDir == nil || !workstation.Spec.Disposable {
			return volume, nil, errors.New("data EmptyDir requires a disposable Workstation")
		}
		volume.EmptyDir = source.EmptyDir.DeepCopy()
	default:
		return volume, nil, fmt.Errorf("unsupported data volume type %q", source.Type)
	}
	return volume, nil, nil
}

func buildWorkspaceVolume(
	workstation *t3v1alpha1.Workstation,
	names ResourceNames,
	labels map[string]string,
) (corev1.Volume, *corev1.PersistentVolumeClaim, bool, error) {
	source := workstation.Spec.Storage.Workspace
	volume := corev1.Volume{Name: "workspace"}
	switch source.Type {
	case t3v1alpha1.WorkspaceVolumeExistingClaim:
		if source.ExistingClaim == nil {
			return volume, nil, false, errors.New("workspace ExistingClaim configuration is required")
		}
		volume.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: source.ExistingClaim.Name}
	case t3v1alpha1.WorkspaceVolumeClaimTemplate:
		if source.ClaimTemplate == nil {
			return volume, nil, false, errors.New("workspace ClaimTemplate configuration is required")
		}
		if err := validateClaimTemplateVolumeMode(source.ClaimTemplate); err != nil {
			return volume, nil, false, fmt.Errorf("workspace %w", err)
		}
		claim := buildClaim(workstation, names.WorkspaceClaim, "workspace", source.ClaimTemplate, labels)
		volume.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}
		return volume, claim, false, nil
	case t3v1alpha1.WorkspaceVolumeNFS:
		if source.NFS == nil {
			return volume, nil, false, errors.New("workspace NFS configuration is required")
		}
		volume.NFS = &corev1.NFSVolumeSource{Server: source.NFS.Server, Path: source.NFS.ExportPath, ReadOnly: source.NFS.ReadOnly}
		return volume, nil, source.NFS.ReadOnly, nil
	case t3v1alpha1.WorkspaceVolumeEmptyDir:
		if source.EmptyDir == nil {
			return volume, nil, false, errors.New("workspace EmptyDir configuration is required")
		}
		volume.EmptyDir = source.EmptyDir.DeepCopy()
	default:
		return volume, nil, false, fmt.Errorf("unsupported workspace volume type %q", source.Type)
	}
	return volume, nil, false, nil
}

func validateClaimTemplateVolumeMode(template *t3v1alpha1.ClaimTemplateVolumeSource) error {
	if template.Spec.VolumeMode != nil && *template.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return errors.New("ClaimTemplate volumeMode must be Filesystem")
	}
	return nil
}

func buildClaim(
	workstation *t3v1alpha1.Workstation,
	name string,
	volumeName string,
	template *t3v1alpha1.ClaimTemplateVolumeSource,
	labels map[string]string,
) *corev1.PersistentVolumeClaim {
	claimLabels := cloneStringMap(template.Metadata.Labels)
	for key, value := range labels {
		claimLabels[key] = value
	}
	annotations := cloneStringMap(template.Metadata.Annotations)
	retentionPolicy := template.RetentionPolicy
	if retentionPolicy == "" {
		retentionPolicy = t3v1alpha1.ClaimRetentionRetain
	}
	annotations[claimWorkstationAnnotation] = workstation.Name
	annotations[claimWorkstationUIDAnnotation] = string(workstation.UID)
	annotations[claimVolumeAnnotation] = volumeName
	annotations[claimRetentionAnnotation] = string(retentionPolicy)
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: workstation.Namespace, Name: name, Labels: claimLabels, Annotations: annotations},
		Spec:       *template.Spec.DeepCopy(),
	}
}

func compactStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if value == "" || len(result) != 0 && result[len(result)-1] == value {
			continue
		}
		result = append(result, value)
	}
	return result
}

func cloneStringMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input)+4)
	for key, value := range input {
		result[key] = value
	}
	return result
}

func serviceAccountName(workstation *t3v1alpha1.Workstation) string {
	if workstation.Spec.ServiceAccountName == "" {
		return "default"
	}
	return workstation.Spec.ServiceAccountName
}

func int32Pointer(value int32) *int32 { return &value }
func int64Pointer(value int64) *int64 { return &value }
func boolPointer(value bool) *bool    { return &value }
func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
