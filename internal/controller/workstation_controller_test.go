package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/apply"
	"github.com/janpuc/t3-code-operator/internal/render"
	"github.com/janpuc/t3-code-operator/internal/sidecar"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestWorkstationReconcilerCreatesTheOwnedRuntime(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	harness := controllerHarness("provider-token")
	kube, reconciler := newWorkstationTestReconciler(t, now, workstation, harness)
	reconcileWorkstation(t, reconciler)

	stored := getWorkstation(t, kube)
	if !controllerutilContains(stored.Finalizers, workstationFinalizer) {
		t.Fatalf("drain finalizer is missing: %#v", stored.Finalizers)
	}
	names := NamesForWorkstation(workstation.Name)
	for _, object := range []client.Object{
		&appsv1.Deployment{}, &corev1.Service{}, &corev1.ConfigMap{}, &rbacv1.Role{}, &rbacv1.RoleBinding{},
	} {
		name := names.Base
		switch object.(type) {
		case *corev1.ConfigMap:
			name = names.Manifest
		case *rbacv1.Role, *rbacv1.RoleBinding:
			name = names.SidecarRole
		}
		if err := kube.Get(context.Background(), types.NamespacedName{Namespace: workstation.Namespace, Name: name}, object); err != nil {
			t.Fatalf("get %T %s: %v", object, name, err)
		}
	}
	manifest := readControllerManifest(t, kube)
	if manifest.DesiredRevision == "" || len(manifest.ProviderInstances) != 1 {
		t.Fatalf("manifest was not published: %#v", manifest)
	}
	deployment := getControllerDeployment(t, kube)
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	if err := kube.Status().Update(context.Background(), deployment); err != nil {
		t.Fatal(err)
	}
	reconcileWorkstation(t, reconciler)
	stored = getWorkstation(t, kube)
	if stored.Status.DesiredRevision != manifest.DesiredRevision || stored.Status.PodRevision == "" ||
		conditionStatus(stored.Status.Conditions, conditionResolved) != metav1.ConditionTrue {
		t.Fatalf("unexpected Workstation status: %#v", stored.Status)
	}
}

func TestHarnessContentChangeDoesNotUpdateTheDeployment(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	harness := controllerHarness("provider-token")
	kube, reconciler := newWorkstationTestReconciler(t, now, workstation, harness)
	reconcileWorkstation(t, reconciler)

	beforeDeployment := getControllerDeployment(t, kube)
	beforeManifest := readControllerManifest(t, kube)
	storedHarness := &t3v1alpha1.Harness{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: "codex"}, storedHarness); err != nil {
		t.Fatal(err)
	}
	storedHarness.Spec.DisplayName = "Changed content"
	storedHarness.Spec.Config = jsonObject(`{"homePath":"/data/harnesses/codex/codex","model":"changed"}`)
	if err := kube.Update(context.Background(), storedHarness); err != nil {
		t.Fatal(err)
	}
	reconcileWorkstation(t, reconciler)

	afterDeployment := getControllerDeployment(t, kube)
	afterManifest := readControllerManifest(t, kube)
	if beforeManifest.DesiredRevision == afterManifest.DesiredRevision {
		t.Fatal("test did not change the rendered revision")
	}
	if beforeDeployment.ResourceVersion != afterDeployment.ResourceVersion ||
		!reflect.DeepEqual(beforeDeployment.Spec.Template, afterDeployment.Spec.Template) ||
		beforeDeployment.Annotations[podRevisionAnnotation] != afterDeployment.Annotations[podRevisionAnnotation] {
		t.Fatalf("Harness content updated the Deployment:\nbefore=%#v\nafter=%#v", beforeDeployment, afterDeployment)
	}
}

func TestReadyDoesNotDescribeAStaleLiveRevision(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	harness := controllerHarness("provider-token")
	kube, reconciler := newWorkstationTestReconciler(t, now, workstation, harness)
	reconcileWorkstation(t, reconciler)
	oldManifest := readControllerManifest(t, kube)
	deployment := getControllerDeployment(t, kube)
	writeControllerReport(t, kube, controllerReport(oldManifest.DesiredRevision, deployment.Annotations[podRevisionAnnotation], now, sidecar.ActivityStateIdle))
	deployment.Status.AvailableReplicas = 1
	if err := kube.Status().Update(context.Background(), deployment); err != nil {
		t.Fatal(err)
	}

	storedHarness := &t3v1alpha1.Harness{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: "codex"}, storedHarness); err != nil {
		t.Fatal(err)
	}
	storedHarness.Spec.DisplayName = "new generation"
	if err := kube.Update(context.Background(), storedHarness); err != nil {
		t.Fatal(err)
	}
	reconcileWorkstation(t, reconciler)

	stored := getWorkstation(t, kube)
	if stored.Status.DesiredRevision == oldManifest.DesiredRevision {
		t.Fatal("test did not create a new desired revision")
	}
	if conditionStatus(stored.Status.Conditions, conditionProgrammed) != metav1.ConditionFalse ||
		conditionStatus(stored.Status.Conditions, conditionReady) != metav1.ConditionFalse {
		t.Fatalf("stale live revision was reported ready: %#v", stored.Status)
	}
}

func TestStatusDoesNotPromoteAnUnobservedPodRevision(t *testing.T) {
	workstation := controllerTestWorkstation()
	oldPodRevision := "sha256:" + strings.Repeat("a", 64)
	newPodRevision := "sha256:" + strings.Repeat("b", 64)
	workstation.Status.PodRevision = oldPodRevision
	workstation.Status.LiveImage = workstation.Spec.Image
	scheme := controllerTestScheme(t)
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&t3v1alpha1.Workstation{}).
		WithObjects(workstation).
		Build()
	reconciler := &WorkstationReconciler{Client: kube}
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: workstation.Namespace,
		Name:      workstation.Name,
		UID:       string(workstation.UID),
	})
	if err != nil {
		t.Fatal(err)
	}
	newImage := "registry.example/t3@sha256:" + strings.Repeat("e", 64)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Generation:  2,
			Annotations: map[string]string{podRevisionAnnotation: newPodRevision},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Pointer(1),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  "t3-code",
				Image: newImage,
			}}}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
		},
	}
	report := controllerReport(manifest.DesiredRevision, newPodRevision, time.Now().UTC(), sidecar.ActivityStateIdle)
	if err := reconciler.setSuccessStatus(context.Background(), workstation, manifest, deployment, &report, false); err != nil {
		t.Fatal(err)
	}
	stored := getWorkstation(t, kube)
	if stored.Status.PodRevision != oldPodRevision ||
		stored.Status.PendingPodRevision != newPodRevision ||
		stored.Status.LiveImage == newImage ||
		conditionStatus(stored.Status.Conditions, conditionReady) != metav1.ConditionFalse {
		t.Fatalf("unobserved Pod revision was promoted: %#v", stored.Status)
	}
}

func TestStatusRejectsAReportFromThePreviousPodRevision(t *testing.T) {
	workstation := controllerTestWorkstation()
	priorLiveRevision := "sha256:" + strings.Repeat("c", 64)
	workstation.Status.LiveRevision = priorLiveRevision
	scheme := controllerTestScheme(t)
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&t3v1alpha1.Workstation{}).
		WithObjects(workstation).
		Build()
	reconciler := &WorkstationReconciler{Client: kube}
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: workstation.Namespace,
		Name:      workstation.Name,
		UID:       string(workstation.UID),
	})
	if err != nil {
		t.Fatal(err)
	}
	oldPodRevision := "sha256:" + strings.Repeat("a", 64)
	newPodRevision := "sha256:" + strings.Repeat("b", 64)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Generation:  2,
			Annotations: map[string]string{podRevisionAnnotation: newPodRevision},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Pointer(1),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{podRevisionAnnotation: newPodRevision},
			}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
		},
	}
	report := controllerReport(manifest.DesiredRevision, oldPodRevision, time.Now().UTC(), sidecar.ActivityStateIdle)
	if err := reconciler.setSuccessStatus(context.Background(), workstation, manifest, deployment, &report, false); err != nil {
		t.Fatal(err)
	}
	stored := getWorkstation(t, kube)
	if conditionStatus(stored.Status.Conditions, conditionProgrammed) != metav1.ConditionFalse ||
		conditionStatus(stored.Status.Conditions, conditionReady) != metav1.ConditionFalse ||
		stored.Status.LiveRevision != priorLiveRevision {
		t.Fatalf("a previous Pod report programmed the replacement Pod: %#v", stored.Status)
	}
}

func TestDrainingStatusDoesNotPromoteAnUnobservedPodRevision(t *testing.T) {
	workstation := controllerTestWorkstation()
	workstation.Status.PodRevision = "sha256:old-pod"
	workstation.Status.LiveImage = workstation.Spec.Image
	scheme := controllerTestScheme(t)
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&t3v1alpha1.Workstation{}).
		WithObjects(workstation).
		Build()
	reconciler := &WorkstationReconciler{Client: kube}
	newImage := "registry.example/t3@sha256:" + strings.Repeat("e", 64)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Generation:  2,
			Annotations: map[string]string{podRevisionAnnotation: "sha256:rolling-pod"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Pointer(1),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  "t3-code",
				Image: newImage,
			}}}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
		},
	}
	decision := drainDecision{StartedAt: &metav1.Time{Time: time.Now().UTC()}, Reason: "WorkActive"}
	if err := reconciler.setDrainingStatus(
		context.Background(),
		workstation,
		render.Manifest{DesiredRevision: "sha256:content"},
		deployment,
		"sha256:next-pod",
		decision,
	); err != nil {
		t.Fatal(err)
	}
	stored := getWorkstation(t, kube)
	if stored.Status.PodRevision != "sha256:old-pod" ||
		stored.Status.PendingPodRevision != "sha256:next-pod" ||
		stored.Status.LiveImage == newImage {
		t.Fatalf("draining promoted an unobserved Pod revision: %#v", stored.Status)
	}
}

func TestPodShapeChangeWaitsForFreshIdle(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	kube, reconciler := newWorkstationTestReconciler(t, now, workstation, controllerHarness("provider-token"))
	reconcileWorkstation(t, reconciler)
	baselineManifest := readControllerManifest(t, kube)
	baselineDeployment := getControllerDeployment(t, kube)
	writeControllerReport(t, kube, controllerReport(baselineManifest.DesiredRevision, baselineDeployment.Annotations[podRevisionAnnotation], now, sidecar.ActivityStateActive))

	stored := getWorkstation(t, kube)
	oldImage := stored.Spec.Image
	newImage := "registry.example/t3@sha256:" + strings.Repeat("e", 64)
	stored.Spec.Image = newImage
	stored.Generation++
	if err := kube.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	reconcileWorkstation(t, reconciler)
	blockedDeployment := getControllerDeployment(t, kube)
	if deploymentImage(blockedDeployment) != oldImage {
		t.Fatalf("active work did not block the image change: %s", deploymentImage(blockedDeployment))
	}
	stored = getWorkstation(t, kube)
	if stored.Status.DrainStartedAt == nil || conditionStatus(stored.Status.Conditions, conditionDraining) != metav1.ConditionTrue {
		t.Fatalf("drain state is missing: %#v", stored.Status)
	}

	writeControllerReport(t, kube, controllerReport(baselineManifest.DesiredRevision, baselineDeployment.Annotations[podRevisionAnnotation], now.Add(time.Second), sidecar.ActivityStateIdle))
	reconciler.Now = func() time.Time { return now.Add(time.Second) }
	reconcileWorkstation(t, reconciler)
	updatedDeployment := getControllerDeployment(t, kube)
	if deploymentImage(updatedDeployment) != newImage {
		t.Fatalf("fresh idle did not permit the image change: %s", deploymentImage(updatedDeployment))
	}
}

func TestSMBServiceRemainsUntilTheDisabledPodShapeIsAvailable(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	workstation.Spec.Storage.Workspace = t3v1alpha1.WorkspaceVolumeSource{
		Type: t3v1alpha1.WorkspaceVolumeClaimTemplate,
		ClaimTemplate: &t3v1alpha1.ClaimTemplateVolumeSource{Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		}},
	}
	workstation.Spec.WorkspaceSharing = &t3v1alpha1.WorkspaceSharing{SMB: &t3v1alpha1.SMBWorkspaceShare{
		PasswordSecretRef: t3v1alpha1.SecretKeyReference{Name: "workspace-smb", Key: "password"},
	}}
	kube, reconciler := newWorkstationTestReconciler(t, now, workstation, controllerHarness("provider-token"))
	reconcileWorkstation(t, reconciler)
	names := NamesForWorkstation(workstation.Name)
	serviceKey := types.NamespacedName{Namespace: workstation.Namespace, Name: names.SMBService}
	if err := kube.Get(context.Background(), serviceKey, &corev1.Service{}); err != nil {
		t.Fatalf("SMB Service was not created: %v", err)
	}
	manifest := readControllerManifest(t, kube)
	deployment := getControllerDeployment(t, kube)
	writeControllerReport(t, kube, controllerReport(manifest.DesiredRevision, deployment.Annotations[podRevisionAnnotation], now, sidecar.ActivityStateActive))

	stored := getWorkstation(t, kube)
	stored.Spec.WorkspaceSharing = nil
	stored.Generation++
	if err := kube.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	reconcileWorkstation(t, reconciler)
	if err := kube.Get(context.Background(), serviceKey, &corev1.Service{}); err != nil {
		t.Fatalf("active drain removed the SMB Service: %v", err)
	}

	writeControllerReport(t, kube, controllerReport(manifest.DesiredRevision, deployment.Annotations[podRevisionAnnotation], now.Add(time.Second), sidecar.ActivityStateIdle))
	reconciler.Now = func() time.Time { return now.Add(time.Second) }
	reconcileWorkstation(t, reconciler)
	if err := kube.Get(context.Background(), serviceKey, &corev1.Service{}); err != nil {
		t.Fatalf("unavailable replacement removed the SMB Service: %v", err)
	}
	replacement := getControllerDeployment(t, kube)
	replacement.Status.ObservedGeneration = replacement.Generation
	replacement.Status.UpdatedReplicas = 1
	replacement.Status.AvailableReplicas = 1
	if err := kube.Status().Update(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	reconcileWorkstation(t, reconciler)
	if err := kube.Get(context.Background(), serviceKey, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("available SMB-disabled Pod left its Service: %v", err)
	}
}

func TestSecretPermissionsExpandBeforePublishAndContractAfterLiveReport(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	kube, reconciler := newWorkstationTestReconciler(t, now, workstation, controllerHarness("old-token"))
	reconcileWorkstation(t, reconciler)
	oldManifest := readControllerManifest(t, kube)
	deployment := getControllerDeployment(t, kube)
	writeControllerReport(t, kube, controllerReport(oldManifest.DesiredRevision, deployment.Annotations[podRevisionAnnotation], now, sidecar.ActivityStateIdle))

	harness := &t3v1alpha1.Harness{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: "codex"}, harness); err != nil {
		t.Fatal(err)
	}
	harness.Spec.Environment[0].ValueFrom.SecretKeyRef.Name = "new-token"
	if err := kube.Update(context.Background(), harness); err != nil {
		t.Fatal(err)
	}
	reconcileWorkstation(t, reconciler)
	newManifest := readControllerManifest(t, kube)
	if newManifest.DesiredRevision == oldManifest.DesiredRevision {
		t.Fatal("Secret reference did not change the manifest")
	}
	if got, want := strings.Join(readRoleSecrets(t, kube), ","), "new-token,old-token"; got != want {
		t.Fatalf("Secret Role did not expand before publish: got=%q want=%q", got, want)
	}

	wrongPodRevision := "sha256:" + strings.Repeat("f", 64)
	writeControllerReport(t, kube, controllerReport(newManifest.DesiredRevision, wrongPodRevision, now.Add(time.Second), sidecar.ActivityStateIdle))
	reconcileWorkstation(t, reconciler)
	if got, want := strings.Join(readRoleSecrets(t, kube), ","), "new-token,old-token"; got != want {
		t.Fatalf("stale Pod report contracted the Secret Role: got=%q want=%q", got, want)
	}

	writeControllerReport(t, kube, controllerReport(newManifest.DesiredRevision, deployment.Annotations[podRevisionAnnotation], now.Add(time.Second), sidecar.ActivityStateIdle))
	reconcileWorkstation(t, reconciler)
	if got, want := strings.Join(readRoleSecrets(t, kube), ","), "new-token"; got != want {
		t.Fatalf("Secret Role did not contract after live report: got=%q want=%q", got, want)
	}
}

func newWorkstationTestReconciler(
	t *testing.T,
	now time.Time,
	objects ...client.Object,
) (client.Client, *WorkstationReconciler) {
	t.Helper()
	scheme := controllerTestScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := policyv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&t3v1alpha1.Workstation{}, &appsv1.Deployment{}).
		WithObjects(objects...).
		Build()
	reconciler := &WorkstationReconciler{
		Client:            kube,
		Scheme:            scheme,
		ActivityFreshness: 15 * time.Second,
		Now:               func() time.Time { return now },
	}
	reconciler.Assembler = &Assembler{Reader: kube}
	return kube, reconciler
}

func reconcileWorkstation(t *testing.T, reconciler *WorkstationReconciler) {
	t.Helper()
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agents", Name: "primary"}}); err != nil {
		t.Fatal(err)
	}
}

func controllerHarness(secretName string) *t3v1alpha1.Harness {
	return &t3v1alpha1.Harness{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "codex"},
		Spec: t3v1alpha1.HarnessSpec{
			InstanceID: "codex",
			Driver:     "codex",
			Config:     jsonObject(`{"homePath":"/data/harnesses/codex/codex"}`),
			Environment: []t3v1alpha1.EnvironmentVariable{{
				Name: "OPENAI_API_KEY",
				ValueFrom: &t3v1alpha1.EnvironmentValueSource{SecretKeyRef: t3v1alpha1.SecretKeyReference{
					Name: secretName,
					Key:  "token",
				}},
			}},
			WorkstationRefs: []t3v1alpha1.LocalObjectReference{{Name: "primary"}},
		},
	}
}

func controllerReport(revision, podRevision string, observedAt time.Time, activity sidecar.ActivityState) sidecar.StatusReport {
	return sidecar.StatusReport{
		APIVersion:         sidecar.ReportAPIVersion,
		Kind:               sidecar.ReportKind,
		ProtocolVersion:    render.ProtocolVersion,
		T3Version:          sidecar.UpstreamT3Version,
		PodRevision:        podRevision,
		DesiredRevision:    revision,
		LiveRevision:       revision,
		State:              apply.ApplyStateProgrammed,
		Activity:           activity,
		ActivityObservedAt: observedAt.UTC().Format(time.RFC3339Nano),
	}
}

func writeControllerReport(t *testing.T, kube client.Client, report sidecar.StatusReport) {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	names := NamesForWorkstation("primary")
	configMap := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: "agents", Name: names.Report}
	if err := kube.Get(context.Background(), key, configMap); err != nil {
		t.Fatal(err)
	}
	if configMap.Data == nil {
		configMap.Data = map[string]string{}
	}
	configMap.Data[sidecar.ReportDataKey] = string(raw)
	if err := kube.Update(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}
}

func readControllerManifest(t *testing.T, kube client.Client) render.Manifest {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	names := NamesForWorkstation("primary")
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: names.Manifest}, configMap); err != nil {
		t.Fatal(err)
	}
	var manifest render.Manifest
	if err := json.Unmarshal([]byte(configMap.Data[sidecar.ManifestDataKey]), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func readRoleSecrets(t *testing.T, kube client.Client) []string {
	t.Helper()
	role := &rbacv1.Role{}
	names := NamesForWorkstation("primary")
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: names.SidecarRole}, role); err != nil {
		t.Fatal(err)
	}
	var result []string
	for _, rule := range role.Rules {
		if containsString(rule.Resources, "secrets") {
			result = append(result, rule.ResourceNames...)
		}
	}
	sort.Strings(result)
	return result
}

func getControllerDeployment(t *testing.T, kube client.Client) *appsv1.Deployment {
	t.Helper()
	deployment := &appsv1.Deployment{}
	names := NamesForWorkstation("primary")
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: names.Base}, deployment); err != nil {
		t.Fatal(err)
	}
	return deployment
}

func getWorkstation(t *testing.T, kube client.Client) *t3v1alpha1.Workstation {
	t.Helper()
	workstation := &t3v1alpha1.Workstation{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: "primary"}, workstation); err != nil {
		t.Fatal(err)
	}
	return workstation
}

func conditionStatus(conditions []metav1.Condition, conditionType string) metav1.ConditionStatus {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status
		}
	}
	return metav1.ConditionUnknown
}

func controllerutilContains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
