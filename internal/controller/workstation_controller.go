package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	conditionResolved   = "Resolved"
	conditionProgrammed = "Programmed"
	conditionReady      = "Ready"
	conditionDraining   = "Draining"
)

type WorkstationReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Assembler         *Assembler
	ActivityFreshness time.Duration
	SMBImage          string
	WorkstationImage  string
	Now               func() time.Time
}

func (reconciler *WorkstationReconciler) images() WorkloadImages {
	return WorkloadImages{SMB: reconciler.SMBImage, Workstation: reconciler.WorkstationImage}
}

func (reconciler *WorkstationReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	workstation := &t3v1alpha1.Workstation{}
	if err := reconciler.Get(ctx, request.NamespacedName, workstation); apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}
	if !workstation.DeletionTimestamp.IsZero() {
		return reconciler.reconcileDeletion(ctx, workstation)
	}
	if !controllerutil.ContainsFinalizer(workstation, workstationFinalizer) {
		controllerutil.AddFinalizer(workstation, workstationFinalizer)
		if err := reconciler.Update(ctx, workstation); err != nil {
			return ctrl.Result{}, err
		}
	}
	if reconciler.Assembler == nil {
		return ctrl.Result{}, errors.New("Workstation assembler is required")
	}

	assembly, err := reconciler.Assembler.Assemble(ctx, workstation)
	if err != nil {
		if statusErr := reconciler.setResolutionFailure(ctx, workstation, err); statusErr != nil {
			return ctrl.Result{}, errors.Join(err, statusErr)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	names := NamesForWorkstation(workstation.Name)
	priorSecrets, err := reconciler.currentSecretNames(ctx, workstation.Namespace, names)
	if err != nil {
		return reconciler.failReconcile(ctx, workstation, "ResourceReadFailed", err)
	}
	expandedSecrets := unionSortedStrings(priorSecrets, assembly.SecretNames)
	resources, err := BuildWorkloadResources(workstation, assembly.Manifest, expandedSecrets, reconciler.images())
	if err != nil {
		return reconciler.failReconcile(ctx, workstation, "ResourceBuildFailed", err)
	}

	if err := ensureClaims(ctx, reconciler.Client, workstation, resources.Claims); err != nil {
		return reconciler.failReconcile(ctx, workstation, "StorageFailed", err)
	}
	if err := ensureReportConfigMap(ctx, reconciler.Client, workstation, resources.Report); err != nil {
		return reconciler.failReconcile(ctx, workstation, "ReportResourceFailed", err)
	}
	if err := ensureRole(ctx, reconciler.Client, workstation, resources.Role); err != nil {
		return reconciler.failReconcile(ctx, workstation, "RBACFailed", err)
	}
	if err := ensureRoleBinding(ctx, reconciler.Client, workstation, resources.RoleBinding); err != nil {
		return reconciler.failReconcile(ctx, workstation, "RBACFailed", err)
	}
	if err := ensureService(ctx, reconciler.Client, workstation, resources.Service); err != nil {
		return reconciler.failReconcile(ctx, workstation, "ServiceFailed", err)
	}
	if resources.SMBService != nil {
		if err := ensureOptionalService(
			ctx,
			reconciler.Client,
			workstation,
			types.NamespacedName{Namespace: workstation.Namespace, Name: names.SMBService},
			resources.SMBService,
		); err != nil {
			return reconciler.failReconcile(ctx, workstation, "SMBServiceFailed", err)
		}
	}
	if err := ensurePDB(ctx, reconciler.Client, workstation, resources.PDB); err != nil {
		return reconciler.failReconcile(ctx, workstation, "DisruptionFailed", err)
	}
	if err := ensureManifestConfigMap(ctx, reconciler.Client, workstation, resources.Manifest); err != nil {
		return reconciler.failReconcile(ctx, workstation, "ManifestPublishFailed", err)
	}

	report, err := reconciler.readReport(ctx, workstation.Namespace, names.Report)
	if err != nil {
		return reconciler.failReconcile(ctx, workstation, "ReportReadFailed", err)
	}
	deployment, exists, err := getDeployment(ctx, reconciler.Client, types.NamespacedName{Namespace: workstation.Namespace, Name: names.Base})
	if err != nil {
		return reconciler.failReconcile(ctx, workstation, "DeploymentReadFailed", err)
	}
	desiredPodRevision := resources.Deployment.Annotations[podRevisionAnnotation]
	forcedDrain := false
	if exists && deployment.Annotations[podRevisionAnnotation] != desiredPodRevision {
		decision := evaluateDrain(reportForDeployment(report, deployment), reconciler.now(), workstation.Status.DrainStartedAt, workstation.Spec.Drain, reconciler.ActivityFreshness)
		if !decision.Permit {
			if err := reconciler.setDrainingStatus(ctx, workstation, assembly.Manifest, deployment, desiredPodRevision, decision); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		forcedDrain = decision.Forced
	}
	deployment, err = createOrUpdateDeployment(ctx, reconciler.Client, workstation, deployment, resources.Deployment)
	if err != nil {
		return reconciler.failReconcile(ctx, workstation, "DeploymentFailed", err)
	}
	if deploymentCurrentAndAvailable(deployment) {
		if resources.SMBService == nil {
			if err := ensureOptionalService(
				ctx,
				reconciler.Client,
				workstation,
				types.NamespacedName{Namespace: workstation.Namespace, Name: names.SMBService},
				nil,
			); err != nil {
				return reconciler.failReconcile(ctx, workstation, "SMBServiceFailed", err)
			}
		}
		if err := cleanupDetachedClaims(ctx, reconciler.Client, workstation, names); err != nil {
			return reconciler.failReconcile(ctx, workstation, "StorageCleanupFailed", err)
		}
	}

	if reportProgramsDeployment(report, deployment, assembly.Manifest.DesiredRevision) {
		contracted, buildErr := BuildWorkloadResources(workstation, assembly.Manifest, assembly.SecretNames, reconciler.images())
		if buildErr != nil {
			return reconciler.failReconcile(ctx, workstation, "ResourceBuildFailed", buildErr)
		}
		if err := ensureRole(ctx, reconciler.Client, workstation, contracted.Role); err != nil {
			return reconciler.failReconcile(ctx, workstation, "RBACContractFailed", err)
		}
	}
	if err := reconciler.setSuccessStatus(ctx, workstation, assembly.Manifest, deployment, report, forcedDrain); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (reconciler *WorkstationReconciler) reconcileDeletion(
	ctx context.Context,
	workstation *t3v1alpha1.Workstation,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(workstation, workstationFinalizer) {
		return ctrl.Result{}, nil
	}
	names := NamesForWorkstation(workstation.Name)
	deployment, exists, err := getDeployment(ctx, reconciler.Client, types.NamespacedName{Namespace: workstation.Namespace, Name: names.Base})
	if err != nil {
		return ctrl.Result{}, err
	}
	if exists {
		report, err := reconciler.readReport(ctx, workstation.Namespace, names.Report)
		if err != nil {
			return ctrl.Result{}, err
		}
		decision := evaluateDrain(reportForDeployment(report, deployment), reconciler.now(), workstation.Status.DrainStartedAt, workstation.Spec.Drain, reconciler.ActivityFreshness)
		if !decision.Permit {
			manifest := render.Manifest{DesiredRevision: workstation.Status.DesiredRevision}
			if err := reconciler.setDrainingStatus(ctx, workstation, manifest, deployment, workstation.Status.PendingPodRevision, decision); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}
	if err := reconciler.deleteClaims(ctx, workstation, names); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(workstation, workstationFinalizer)
	if err := reconciler.Update(ctx, workstation); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (reconciler *WorkstationReconciler) deleteClaims(
	ctx context.Context,
	workstation *t3v1alpha1.Workstation,
	names ResourceNames,
) error {
	return deleteManagedClaims(ctx, reconciler.Client, workstation, names, existingClaimNames(workstation))
}

func (reconciler *WorkstationReconciler) currentSecretNames(
	ctx context.Context,
	namespace string,
	names ResourceNames,
) ([]string, error) {
	secretSet := make(map[string]struct{})
	role := &rbacv1.Role{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: namespace, Name: names.SidecarRole}, role); err == nil {
		for _, rule := range role.Rules {
			if containsString(rule.Resources, "secrets") {
				for _, name := range rule.ResourceNames {
					secretSet[name] = struct{}{}
				}
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	configMap := &corev1.ConfigMap{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: namespace, Name: names.Manifest}, configMap); err == nil {
		var manifest render.Manifest
		if raw := configMap.Data[sidecar.ManifestDataKey]; len(raw) <= render.MaxRenderedManifestBytes && json.Unmarshal([]byte(raw), &manifest) == nil {
			for _, reference := range apply.ReferencedSecrets(manifest) {
				if reference.Namespace == namespace {
					secretSet[reference.Name] = struct{}{}
				}
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	result := make([]string, 0, len(secretSet))
	for name := range secretSet {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func (reconciler *WorkstationReconciler) readReport(
	ctx context.Context,
	namespace string,
	name string,
) (*sidecar.StatusReport, error) {
	configMap := &corev1.ConfigMap{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, configMap); apierrors.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	raw := configMap.Data[sidecar.ReportDataKey]
	if raw == "" {
		return nil, nil
	}
	if len(raw) > sidecar.MaxReportBytes {
		return nil, nil
	}
	var report sidecar.StatusReport
	if json.Unmarshal([]byte(raw), &report) != nil || report.Validate() != nil {
		return nil, nil
	}
	return &report, nil
}

func (reconciler *WorkstationReconciler) setResolutionFailure(
	ctx context.Context,
	workstation *t3v1alpha1.Workstation,
	cause error,
) error {
	message := resolutionFailureMessage(cause)
	return reconciler.patchStatus(ctx, workstation, func(status *t3v1alpha1.WorkstationStatus) {
		status.ObservedGeneration = workstation.Generation
		setCondition(&status.Conditions, conditionResolved, metav1.ConditionFalse, "ResolutionFailed", message, workstation.Generation)
		setCondition(&status.Conditions, conditionProgrammed, metav1.ConditionFalse, "ResolutionFailed", "The sidecar cannot apply this generation.", workstation.Generation)
		setCondition(&status.Conditions, conditionReady, metav1.ConditionFalse, "ResolutionFailed", "The current generation is not ready.", workstation.Generation)
	})
}

const resolutionFailureDetailLimit = 1024

func resolutionFailureMessage(cause error) string {
	message := "The desired resources did not resolve."
	if cause == nil {
		return message
	}
	detail := strings.TrimSpace(cause.Error())
	if detail == "" {
		return message
	}
	if len(detail) > resolutionFailureDetailLimit {
		detail = detail[:resolutionFailureDetailLimit]
	}
	return "The desired resources did not resolve: " + detail
}

func (reconciler *WorkstationReconciler) failReconcile(
	ctx context.Context,
	workstation *t3v1alpha1.Workstation,
	reason string,
	cause error,
) (ctrl.Result, error) {
	statusErr := reconciler.patchStatus(ctx, workstation, func(status *t3v1alpha1.WorkstationStatus) {
		status.ObservedGeneration = workstation.Generation
		setCondition(&status.Conditions, conditionReady, metav1.ConditionFalse, reason, "The operator could not reconcile the Workstation.", workstation.Generation)
	})
	return ctrl.Result{}, errors.Join(cause, statusErr)
}

func (reconciler *WorkstationReconciler) setDrainingStatus(
	ctx context.Context,
	workstation *t3v1alpha1.Workstation,
	manifest render.Manifest,
	deployment *appsv1.Deployment,
	pendingRevision string,
	decision drainDecision,
) error {
	return reconciler.patchStatus(ctx, workstation, func(status *t3v1alpha1.WorkstationStatus) {
		status.ObservedGeneration = workstation.Generation
		if manifest.DesiredRevision != "" {
			status.DesiredRevision = manifest.DesiredRevision
		}
		if deploymentCurrentAndAvailable(deployment) {
			status.PodRevision = deployment.Annotations[podRevisionAnnotation]
			status.LiveImage = deploymentImage(deployment)
		}
		status.PendingPodRevision = pendingRevision
		status.DrainStartedAt = decision.StartedAt
		setCondition(&status.Conditions, conditionResolved, metav1.ConditionTrue, "Resolved", "The desired manifest resolved.", workstation.Generation)
		setCondition(&status.Conditions, conditionDraining, metav1.ConditionTrue, decision.Reason, "The operator is waiting for active work to finish.", workstation.Generation)
		setCondition(&status.Conditions, conditionReady, metav1.ConditionFalse, decision.Reason, "A pod-shape change is waiting for drain.", workstation.Generation)
	})
}

func (reconciler *WorkstationReconciler) setSuccessStatus(
	ctx context.Context,
	workstation *t3v1alpha1.Workstation,
	manifest render.Manifest,
	deployment *appsv1.Deployment,
	report *sidecar.StatusReport,
	forcedDrain bool,
) error {
	report = reportForDeployment(report, deployment)
	return reconciler.patchStatus(ctx, workstation, func(status *t3v1alpha1.WorkstationStatus) {
		status.ObservedGeneration = workstation.Generation
		status.DesiredRevision = manifest.DesiredRevision
		runtimeAvailable := deploymentCurrentAndAvailable(deployment)
		if runtimeAvailable {
			status.PodRevision = deployment.Annotations[podRevisionAnnotation]
			status.PendingPodRevision = ""
			status.LiveImage = deploymentImage(deployment)
		} else {
			status.PendingPodRevision = deployment.Annotations[podRevisionAnnotation]
		}
		status.DrainStartedAt = nil
		status.DataClaimName, status.WorkspaceClaimName = claimStatusNames(workstation)
		setCondition(&status.Conditions, conditionResolved, metav1.ConditionTrue, "Resolved", "The desired manifest resolved.", workstation.Generation)
		drainReason := "NotDraining"
		if forcedDrain {
			drainReason = "ContinuityWaived"
		}
		setCondition(&status.Conditions, conditionDraining, metav1.ConditionFalse, drainReason, "No pod-shape change is waiting for drain.", workstation.Generation)

		programmed := report != nil &&
			report.ProtocolVersion == render.ProtocolVersion &&
			report.PodRevision == deployment.Annotations[podRevisionAnnotation] &&
			report.LiveRevision == manifest.DesiredRevision &&
			report.State == apply.ApplyStateProgrammed
		if programmed {
			status.LiveRevision = report.LiveRevision
			setCondition(&status.Conditions, conditionProgrammed, metav1.ConditionTrue, "Programmed", "The sidecar applied the desired revision.", workstation.Generation)
		} else {
			if report != nil {
				status.LiveRevision = report.LiveRevision
			}
			setCondition(&status.Conditions, conditionProgrammed, metav1.ConditionFalse, reportReason(report), "The sidecar has not applied the desired revision.", workstation.Generation)
		}
		ready := runtimeAvailable && programmed
		if ready {
			setCondition(&status.Conditions, conditionReady, metav1.ConditionTrue, "RuntimeReady", "The Workstation runtime is available.", workstation.Generation)
		} else {
			setCondition(&status.Conditions, conditionReady, metav1.ConditionFalse, "RuntimeUnavailable", "The Workstation runtime is not available.", workstation.Generation)
		}
	})
}

func reportProgramsDeployment(
	report *sidecar.StatusReport,
	deployment *appsv1.Deployment,
	desiredRevision string,
) bool {
	report = reportForDeployment(report, deployment)
	return report != nil &&
		report.ProtocolVersion == render.ProtocolVersion &&
		report.LiveRevision == desiredRevision &&
		report.State == apply.ApplyStateProgrammed
}

func deploymentCurrentAndAvailable(deployment *appsv1.Deployment) bool {
	if deployment == nil || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas <= 0 {
		return false
	}
	desired := *deployment.Spec.Replicas
	return deployment.Status.ObservedGeneration >= deployment.Generation &&
		deployment.Status.UpdatedReplicas >= desired &&
		deployment.Status.AvailableReplicas >= desired
}

func reportForDeployment(report *sidecar.StatusReport, deployment *appsv1.Deployment) *sidecar.StatusReport {
	if report == nil || deployment == nil {
		return nil
	}
	expected := deployment.Annotations[podRevisionAnnotation]
	if report.PodRevision == expected && expected != "" {
		return report
	}
	_, managedTemplate := deployment.Spec.Template.Annotations[podRevisionAnnotation]
	if report.PodRevision == "" && !managedTemplate {
		return report
	}
	return nil
}

func (reconciler *WorkstationReconciler) patchStatus(
	ctx context.Context,
	workstation *t3v1alpha1.Workstation,
	mutate func(*t3v1alpha1.WorkstationStatus),
) error {
	before := workstation.DeepCopy()
	mutate(&workstation.Status)
	if reflect.DeepEqual(before.Status, workstation.Status) {
		return nil
	}
	return reconciler.Status().Patch(ctx, workstation, client.MergeFrom(before))
}

func (reconciler *WorkstationReconciler) now() time.Time {
	if reconciler.Now != nil {
		return reconciler.Now().UTC()
	}
	return time.Now().UTC()
}

func setCondition(
	conditions *[]metav1.Condition,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
	generation int64,
) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

func reportReason(report *sidecar.StatusReport) string {
	if report == nil || report.Reason == "" {
		return "ApplyPending"
	}
	return report.Reason
}

func claimStatusNames(workstation *t3v1alpha1.Workstation) (string, string) {
	names := NamesForWorkstation(workstation.Name)
	storage := effectiveStorage(workstation)
	data := ""
	switch storage.Data.Type {
	case t3v1alpha1.DataVolumeExistingClaim:
		if storage.Data.ExistingClaim != nil {
			data = storage.Data.ExistingClaim.Name
		}
	case t3v1alpha1.DataVolumeClaimTemplate:
		data = names.DataClaim
	}
	workspace := ""
	switch storage.Workspace.Type {
	case t3v1alpha1.WorkspaceVolumeExistingClaim:
		if storage.Workspace.ExistingClaim != nil {
			workspace = storage.Workspace.ExistingClaim.Name
		}
	case t3v1alpha1.WorkspaceVolumeClaimTemplate:
		workspace = names.WorkspaceClaim
	}
	return data, workspace
}

func deploymentImage(deployment *appsv1.Deployment) string {
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "t3-code" {
			return container.Image
		}
	}
	return ""
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func unionSortedStrings(groups ...[]string) []string {
	set := make(map[string]struct{})
	for _, group := range groups {
		for _, value := range group {
			if value != "" {
				set[value] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (reconciler *WorkstationReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&t3v1alpha1.Workstation{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&t3v1alpha1.Harness{}, handler.EnqueueRequestsFromMapFunc(reconciler.requestsForHarness), builder.WithPredicates(contentChangePredicate())).
		Watches(&t3v1alpha1.Extension{}, handler.EnqueueRequestsFromMapFunc(reconciler.requestsForExtension), builder.WithPredicates(contentChangePredicate())).
		Watches(&t3v1alpha1.MCPServer{}, handler.EnqueueRequestsFromMapFunc(reconciler.requestsForMCPServer), builder.WithPredicates(contentChangePredicate())).
		Complete(reconciler)
}
