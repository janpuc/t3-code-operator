package controller

import (
	"context"
	"testing"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureServicePreservesAllocatedNodePortAndUnmanagedAnnotations(t *testing.T) {
	workstation := controllerTestWorkstation()
	name := NamesForWorkstation(workstation.Name).SMBService
	labels := workstationLabels(NamesForWorkstation(workstation.Name).Base)
	current := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: workstation.Namespace,
			Name:      name,
			Annotations: map[string]string{
				"lb.example.test/old":               "remove",
				"metallb.io/ip-allocated-from-pool": "lan",
				serviceManagedAnnotationsAnnotation: `["lb.example.test/old"]`,
			},
			OwnerReferences: []metav1.OwnerReference{workstationOwner(workstation)},
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Type:     corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{
				Name: "smb", Port: 445, TargetPort: intstr.FromString("smb"), Protocol: corev1.ProtocolTCP, NodePort: 30445,
			}},
		},
	}
	desired := current.DeepCopy()
	desired.Annotations = managedServiceAnnotations(map[string]string{"lb.example.test/new": "desired"})
	desired.Spec.Ports[0].NodePort = 0
	scheme := controllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
	if err := ensureService(context.Background(), kube, workstation, desired); err != nil {
		t.Fatal(err)
	}
	stored := &corev1.Service{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: workstation.Namespace, Name: name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Ports[0].NodePort != 30445 {
		t.Fatalf("allocated NodePort changed: %#v", stored.Spec.Ports)
	}
	if _, exists := stored.Annotations["lb.example.test/old"]; exists {
		t.Fatalf("removed managed annotation remains: %#v", stored.Annotations)
	}
	if stored.Annotations["lb.example.test/new"] != "desired" || stored.Annotations["metallb.io/ip-allocated-from-pool"] != "lan" {
		t.Fatalf("Service annotations were not reconciled safely: %#v", stored.Annotations)
	}
}

func TestEnsureOptionalServiceRemovesDisabledSMBService(t *testing.T) {
	workstation := controllerTestWorkstation()
	name := NamesForWorkstation(workstation.Name).SMBService
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Namespace:       workstation.Namespace,
		Name:            name,
		OwnerReferences: []metav1.OwnerReference{workstationOwner(workstation)},
	}}
	scheme := controllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()
	key := types.NamespacedName{Namespace: workstation.Namespace, Name: name}
	if err := ensureOptionalService(context.Background(), kube, workstation, key, nil); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), key, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("disabled SMB Service remains: %v", err)
	}
}

func TestEnsureServiceClearsLoadBalancerOnlyFieldsWhenExposureChanges(t *testing.T) {
	workstation := controllerTestWorkstation()
	name := NamesForWorkstation(workstation.Name).SMBService
	allocateNodePorts := true
	loadBalancerClass := "example.test/lan"
	current := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       workstation.Namespace,
			Name:            name,
			OwnerReferences: []metav1.OwnerReference{workstationOwner(workstation)},
		},
		Spec: corev1.ServiceSpec{
			Type:                          corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy:         corev1.ServiceExternalTrafficPolicyLocal,
			HealthCheckNodePort:           32001,
			AllocateLoadBalancerNodePorts: &allocateNodePorts,
			LoadBalancerClass:             &loadBalancerClass,
			LoadBalancerIP:                "192.0.2.10",
			Ports: []corev1.ServicePort{{
				Name: "smb", Port: 445, TargetPort: intstr.FromString("smb"), Protocol: corev1.ProtocolTCP, NodePort: 30445,
			}},
		},
	}
	desired := current.DeepCopy()
	desired.Spec.Type = corev1.ServiceTypeClusterIP
	desired.Spec.ExternalTrafficPolicy = ""
	desired.Spec.HealthCheckNodePort = 0
	desired.Spec.AllocateLoadBalancerNodePorts = nil
	desired.Spec.LoadBalancerClass = nil
	desired.Spec.LoadBalancerIP = ""
	desired.Spec.Ports[0].NodePort = 0
	scheme := controllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
	if err := ensureService(context.Background(), kube, workstation, desired); err != nil {
		t.Fatal(err)
	}
	stored := &corev1.Service{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: workstation.Namespace, Name: name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Type != corev1.ServiceTypeClusterIP || stored.Spec.Ports[0].NodePort != 0 ||
		stored.Spec.HealthCheckNodePort != 0 || stored.Spec.AllocateLoadBalancerNodePorts != nil ||
		stored.Spec.LoadBalancerClass != nil || stored.Spec.LoadBalancerIP != "" {
		t.Fatalf("LoadBalancer-only fields remain: %#v", stored.Spec)
	}
}

func TestCleanupDetachedClaimsHonorsTheRecordedRetentionPolicy(t *testing.T) {
	workstation := controllerTestWorkstation()
	workstation.Spec.Storage.Data = t3v1alpha1.DataVolumeSource{
		Type:          t3v1alpha1.DataVolumeExistingClaim,
		ExistingClaim: &t3v1alpha1.ExistingClaimVolumeSource{Name: "external-data"},
	}
	workstation.Spec.Storage.Workspace = t3v1alpha1.WorkspaceVolumeSource{
		Type: t3v1alpha1.WorkspaceVolumeNFS,
		NFS:  &t3v1alpha1.NFSVolumeSource{Server: "nas.internal", ExportPath: "/workspace"},
	}
	names := NamesForWorkstation(workstation.Name)
	deleteClaim := controllerManagedClaim(workstation, names.DataClaim, "data", t3v1alpha1.ClaimRetentionDelete)
	retainClaim := controllerManagedClaim(workstation, names.WorkspaceClaim, "workspace", t3v1alpha1.ClaimRetentionRetain)
	scheme := controllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deleteClaim, retainClaim).Build()
	if err := cleanupDetachedClaims(context.Background(), kube, workstation, names); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: workstation.Namespace, Name: names.DataClaim}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("detached Delete claim remains: %v", err)
	}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: workstation.Namespace, Name: names.WorkspaceClaim}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("detached Retain claim was removed: %v", err)
	}
}

func TestExistingClaimAdoptionPreventsManagedClaimDeletion(t *testing.T) {
	workstation := controllerTestWorkstation()
	names := NamesForWorkstation(workstation.Name)
	workstation.Spec.Storage.Data = t3v1alpha1.DataVolumeSource{
		Type:          t3v1alpha1.DataVolumeExistingClaim,
		ExistingClaim: &t3v1alpha1.ExistingClaimVolumeSource{Name: names.DataClaim},
	}
	claim := controllerManagedClaim(workstation, names.DataClaim, "data", t3v1alpha1.ClaimRetentionDelete)
	scheme := controllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()
	if err := cleanupDetachedClaims(context.Background(), kube, workstation, names); err != nil {
		t.Fatal(err)
	}
	if err := deleteManagedClaims(context.Background(), kube, workstation, names, existingClaimNames(workstation)); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: workstation.Namespace, Name: names.DataClaim}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("adopted ExistingClaim was removed: %v", err)
	}
}

func TestReplacementWorkstationCannotDeleteAClaimFromThePreviousUID(t *testing.T) {
	previous := controllerTestWorkstation()
	names := NamesForWorkstation(previous.Name)
	claim := controllerManagedClaim(previous, names.DataClaim, "data", t3v1alpha1.ClaimRetentionDelete)
	replacement := previous.DeepCopy()
	replacement.UID = types.UID("replacement-uid")
	replacement.Spec.Storage.Data = t3v1alpha1.DataVolumeSource{
		Type:          t3v1alpha1.DataVolumeExistingClaim,
		ExistingClaim: &t3v1alpha1.ExistingClaimVolumeSource{Name: "replacement-data"},
	}
	scheme := controllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()
	if err := cleanupDetachedClaims(context.Background(), kube, replacement, names); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(
		context.Background(),
		types.NamespacedName{Namespace: previous.Namespace, Name: names.DataClaim},
		&corev1.PersistentVolumeClaim{},
	); err != nil {
		t.Fatalf("replacement Workstation removed the previous claim: %v", err)
	}
}

func TestEnsureClaimsAdoptsCompatibleRetainedClaimFromRecreatedWorkstation(t *testing.T) {
	previous := controllerTestWorkstation()
	replacement := previous.DeepCopy()
	replacement.UID = types.UID("replacement-uid")
	template := &t3v1alpha1.ClaimTemplateVolumeSource{
		RetentionPolicy: t3v1alpha1.ClaimRetentionRetain,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("5Gi"),
			}},
		},
	}
	name := NamesForWorkstation(previous.Name).DataClaim
	current := buildClaim(previous, name, "data", template, nil)
	desired := buildClaim(replacement, name, "data", template, nil)
	scheme := controllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
	if err := ensureClaims(context.Background(), kube, replacement, []*corev1.PersistentVolumeClaim{desired}); err != nil {
		t.Fatalf("recreated Workstation did not adopt its compatible retained claim: %v", err)
	}
	stored := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: current.Namespace, Name: current.Name}
	if err := kube.Get(context.Background(), key, stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Annotations[claimWorkstationUIDAnnotation]; got != string(replacement.UID) {
		t.Fatalf("retained claim kept stale Workstation UID %q", got)
	}
}

func TestEnsureClaimsRejectsDeleteClaimFromPreviousWorkstationUID(t *testing.T) {
	previous := controllerTestWorkstation()
	replacement := previous.DeepCopy()
	replacement.UID = types.UID("replacement-uid")
	template := &t3v1alpha1.ClaimTemplateVolumeSource{
		RetentionPolicy: t3v1alpha1.ClaimRetentionDelete,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}
	name := NamesForWorkstation(previous.Name).DataClaim
	current := buildClaim(previous, name, "data", template, nil)
	desired := buildClaim(replacement, name, "data", template, nil)
	scheme := controllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
	if err := ensureClaims(context.Background(), kube, replacement, []*corev1.PersistentVolumeClaim{desired}); err == nil {
		t.Fatal("recreated Workstation adopted a Delete claim from the previous UID")
	}
}

func TestEnsureClaimsExpandsButNeverShrinksGeneratedStorage(t *testing.T) {
	workstation := controllerTestWorkstation()
	names := NamesForWorkstation(workstation.Name)
	template := &t3v1alpha1.ClaimTemplateVolumeSource{Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse("5Gi"),
		}},
	}}
	desired := buildClaim(workstation, names.DataClaim, "data", template, map[string]string{"managed": "true"})
	current := desired.DeepCopy()
	current.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
	scheme := controllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
	if err := ensureClaims(context.Background(), kube, workstation, []*corev1.PersistentVolumeClaim{desired}); err != nil {
		t.Fatal(err)
	}
	stored := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: workstation.Namespace, Name: names.DataClaim}
	if err := kube.Get(context.Background(), key, stored); err != nil {
		t.Fatal(err)
	}
	if size := stored.Spec.Resources.Requests[corev1.ResourceStorage]; size.Cmp(resource.MustParse("5Gi")) != 0 {
		t.Fatalf("generated claim did not expand: %s", size.String())
	}

	desired.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("1Gi")
	if err := ensureClaims(context.Background(), kube, workstation, []*corev1.PersistentVolumeClaim{desired}); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), key, stored); err != nil {
		t.Fatal(err)
	}
	if size := stored.Spec.Resources.Requests[corev1.ResourceStorage]; size.Cmp(resource.MustParse("5Gi")) != 0 {
		t.Fatalf("generated claim shrank: %s", size.String())
	}
}

func TestEnsureClaimsRejectsAnImmutableTemplateChange(t *testing.T) {
	workstation := controllerTestWorkstation()
	names := NamesForWorkstation(workstation.Name)
	template := &t3v1alpha1.ClaimTemplateVolumeSource{Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
	}}
	current := buildClaim(workstation, names.DataClaim, "data", template, nil)
	desired := current.DeepCopy()
	desired.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	scheme := controllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
	if err := ensureClaims(context.Background(), kube, workstation, []*corev1.PersistentVolumeClaim{desired}); err == nil {
		t.Fatal("immutable generated claim change passed identity validation")
	}
}

func TestEnsureClaimsRejectsAnExistingBlockVolume(t *testing.T) {
	workstation := controllerTestWorkstation()
	workstation.Spec.Storage.Data = t3v1alpha1.DataVolumeSource{
		Type:          t3v1alpha1.DataVolumeExistingClaim,
		ExistingClaim: &t3v1alpha1.ExistingClaimVolumeSource{Name: "block-data"},
	}
	block := corev1.PersistentVolumeBlock
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: workstation.Namespace, Name: "block-data"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: &block},
	}
	scheme := controllerTestScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()
	if err := ensureClaims(context.Background(), kube, workstation, nil); err == nil {
		t.Fatal("existing block volume passed filesystem validation")
	}
}

func controllerManagedClaim(
	workstation *t3v1alpha1.Workstation,
	name string,
	volume string,
	retention t3v1alpha1.ClaimRetentionPolicy,
) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Namespace: workstation.Namespace,
		Name:      name,
		Annotations: map[string]string{
			claimWorkstationAnnotation:    workstation.Name,
			claimWorkstationUIDAnnotation: string(workstation.UID),
			claimVolumeAnnotation:         volume,
			claimRetentionAnnotation:      string(retention),
		},
	}}
}
