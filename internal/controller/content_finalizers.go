package controller

import (
	"context"
	"errors"
	"sort"
	"time"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/render"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const contentFinalizer = "t3code.janpuc.com/runtime-removal"

func (reconciler *HarnessReconciler) reconcileDeletion(
	ctx context.Context,
	harness *t3v1alpha1.Harness,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(harness, contentFinalizer) {
		return ctrl.Result{}, nil
	}
	workstations, err := workstationReferencesForHarness(ctx, reconciler.Client, harness)
	if err != nil {
		return ctrl.Result{}, err
	}
	ready, err := contentRemovalReady(
		ctx,
		reconciler.Client,
		reconciler.Assembler,
		harness.Namespace,
		workstations,
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		before := harness.DeepCopy()
		harness.Status.ObservedGeneration = harness.Generation
		setRemovalPendingConditions(&harness.Status.Conditions, harness.Generation)
		if err := reconciler.Status().Patch(ctx, harness, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	controllerutil.RemoveFinalizer(harness, contentFinalizer)
	return ctrl.Result{}, reconciler.Update(ctx, harness)
}

func (reconciler *ExtensionReconciler) reconcileDeletion(
	ctx context.Context,
	extension *t3v1alpha1.Extension,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(extension, contentFinalizer) {
		return ctrl.Result{}, nil
	}
	workstations, err := workstationReferencesForAttachment(
		ctx,
		reconciler.Client,
		extension.Namespace,
		extension.Spec.HarnessRefs,
		true,
		func(driver string) bool {
			return render.ProgramsExtensionSource(driver, render.ExtensionSourceType(extension.Spec.Source.Type))
		},
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	ready, err := contentRemovalReady(ctx, reconciler.Client, reconciler.Assembler, extension.Namespace, workstations)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		before := extension.DeepCopy()
		extension.Status.ObservedGeneration = extension.Generation
		setRemovalPendingConditions(&extension.Status.Conditions, extension.Generation)
		if err := reconciler.Status().Patch(ctx, extension, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	controllerutil.RemoveFinalizer(extension, contentFinalizer)
	return ctrl.Result{}, reconciler.Update(ctx, extension)
}

func (reconciler *MCPServerReconciler) reconcileDeletion(
	ctx context.Context,
	server *t3v1alpha1.MCPServer,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(server, contentFinalizer) {
		return ctrl.Result{}, nil
	}
	workstations, err := workstationReferencesForAttachment(
		ctx,
		reconciler.Client,
		server.Namespace,
		server.Spec.HarnessRefs,
		false,
		render.ProgramsMCPServers,
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	ready, err := contentRemovalReady(ctx, reconciler.Client, reconciler.Assembler, server.Namespace, workstations)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		before := server.DeepCopy()
		server.Status.ObservedGeneration = server.Generation
		setRemovalPendingConditions(&server.Status.Conditions, server.Generation)
		if err := reconciler.Status().Patch(ctx, server, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	controllerutil.RemoveFinalizer(server, contentFinalizer)
	return ctrl.Result{}, reconciler.Update(ctx, server)
}

func contentRemovalReady(
	ctx context.Context,
	kube client.Client,
	assembler *Assembler,
	namespace string,
	references []t3v1alpha1.LocalObjectReference,
) (bool, error) {
	if assembler == nil {
		return false, errors.New("content removal assembler is required")
	}
	for _, reference := range uniqueLocalReferences(references) {
		workstation := &t3v1alpha1.Workstation{}
		key := types.NamespacedName{Namespace: namespace, Name: reference.Name}
		if err := kube.Get(ctx, key, workstation); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return false, err
		}
		if !workstation.DeletionTimestamp.IsZero() {
			continue
		}
		deployment := &appsv1.Deployment{}
		deploymentKey := types.NamespacedName{Namespace: namespace, Name: NamesForWorkstation(workstation.Name).Base}
		if err := kube.Get(ctx, deploymentKey, deployment); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return false, err
		}
		assembly, err := assembler.Assemble(ctx, workstation)
		if err != nil {
			return false, err
		}
		expected := assembly.Manifest.DesiredRevision
		if workstation.Status.DesiredRevision != expected || workstation.Status.LiveRevision != expected {
			return false, nil
		}
		if !conditionIsTrue(workstation.Status.Conditions, conditionProgrammed) {
			return false, nil
		}
	}
	return true, nil
}

func workstationReferencesForAttachment(
	ctx context.Context,
	kube client.Client,
	namespace string,
	references []t3v1alpha1.LocalObjectReference,
	extension bool,
	programs func(string) bool,
) ([]t3v1alpha1.LocalObjectReference, error) {
	targets, err := listProviderTargets(ctx, kube, namespace)
	if err != nil {
		return nil, err
	}
	selected, _ := selectProviderTargets(targets, references, extension, programs)
	return localReferences(workstationNamesForTargets(selected)), nil
}

func workstationReferencesForHarness(
	ctx context.Context,
	kube client.Client,
	harness *t3v1alpha1.Harness,
) ([]t3v1alpha1.LocalObjectReference, error) {
	if len(harness.Spec.WorkstationRefs) != 0 {
		return uniqueLocalReferences(harness.Spec.WorkstationRefs), nil
	}
	var list t3v1alpha1.WorkstationList
	if err := kube.List(ctx, &list, client.InNamespace(harness.Namespace)); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for index := range list.Items {
		names = append(names, list.Items[index].Name)
	}
	return uniqueLocalReferences(localReferences(names)), nil
}

func uniqueLocalReferences(input []t3v1alpha1.LocalObjectReference) []t3v1alpha1.LocalObjectReference {
	names := make(map[string]struct{}, len(input))
	for _, reference := range input {
		if reference.Name != "" {
			names[reference.Name] = struct{}{}
		}
	}
	result := make([]t3v1alpha1.LocalObjectReference, 0, len(names))
	for name := range names {
		result = append(result, t3v1alpha1.LocalObjectReference{Name: name})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func setRemovalPendingConditions(conditions *[]metav1.Condition, generation int64) {
	setCondition(conditions, conditionProgrammed, metav1.ConditionFalse, "RemovalPending", "The runtime has not applied this removal.", generation)
	setCondition(conditions, conditionReady, metav1.ConditionFalse, "RemovalPending", "The object waits for safe runtime removal.", generation)
}
