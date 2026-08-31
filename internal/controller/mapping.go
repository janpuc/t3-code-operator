package controller

import (
	"context"
	"sort"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (reconciler *WorkstationReconciler) requestsForHarness(
	_ context.Context,
	object client.Object,
) []reconcile.Request {
	harness, ok := object.(*t3v1alpha1.Harness)
	if !ok {
		return nil
	}
	return requestsForReferences(harness.Namespace, harness.Spec.WorkstationRefs)
}

func (reconciler *WorkstationReconciler) requestsForExtension(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	extension, ok := object.(*t3v1alpha1.Extension)
	if !ok {
		return nil
	}
	return reconciler.requestsForHarnessReferences(ctx, extension.Namespace, extension.Spec.HarnessRefs)
}

func (reconciler *WorkstationReconciler) requestsForMCPServer(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	server, ok := object.(*t3v1alpha1.MCPServer)
	if !ok {
		return nil
	}
	return reconciler.requestsForHarnessReferences(ctx, server.Namespace, server.Spec.HarnessRefs)
}

func (reconciler *WorkstationReconciler) requestsForHarnessReferences(
	ctx context.Context,
	namespace string,
	references []t3v1alpha1.LocalObjectReference,
) []reconcile.Request {
	requests := make(map[types.NamespacedName]struct{})
	for _, reference := range references {
		harness := &t3v1alpha1.Harness{}
		if err := reconciler.Get(ctx, types.NamespacedName{Namespace: namespace, Name: reference.Name}, harness); err != nil {
			continue
		}
		for _, request := range requestsForReferences(namespace, harness.Spec.WorkstationRefs) {
			requests[request.NamespacedName] = struct{}{}
		}
	}
	return sortedRequests(requests)
}

func (reconciler *HarnessReconciler) requestsForWorkstation(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	workstation, ok := object.(*t3v1alpha1.Workstation)
	if !ok {
		return nil
	}
	list := &t3v1alpha1.HarnessList{}
	if err := reconciler.List(ctx, list, client.InNamespace(workstation.Namespace)); err != nil {
		return nil
	}
	requests := make(map[types.NamespacedName]struct{})
	for index := range list.Items {
		if referencesName(list.Items[index].Spec.WorkstationRefs, workstation.Name) {
			requests[types.NamespacedName{Namespace: workstation.Namespace, Name: list.Items[index].Name}] = struct{}{}
		}
	}
	return sortedRequests(requests)
}

func (reconciler *ExtensionReconciler) requestsForHarness(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	harness, ok := object.(*t3v1alpha1.Harness)
	if !ok {
		return nil
	}
	list := &t3v1alpha1.ExtensionList{}
	if err := reconciler.List(ctx, list, client.InNamespace(harness.Namespace)); err != nil {
		return nil
	}
	requests := make(map[types.NamespacedName]struct{})
	for index := range list.Items {
		if referencesName(list.Items[index].Spec.HarnessRefs, harness.Name) {
			requests[types.NamespacedName{Namespace: harness.Namespace, Name: list.Items[index].Name}] = struct{}{}
		}
	}
	return sortedRequests(requests)
}

func (reconciler *MCPServerReconciler) requestsForHarness(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	harness, ok := object.(*t3v1alpha1.Harness)
	if !ok {
		return nil
	}
	list := &t3v1alpha1.MCPServerList{}
	if err := reconciler.List(ctx, list, client.InNamespace(harness.Namespace)); err != nil {
		return nil
	}
	requests := make(map[types.NamespacedName]struct{})
	for index := range list.Items {
		if referencesName(list.Items[index].Spec.HarnessRefs, harness.Name) {
			requests[types.NamespacedName{Namespace: harness.Namespace, Name: list.Items[index].Name}] = struct{}{}
		}
	}
	return sortedRequests(requests)
}

func requestsForReferences(namespace string, references []t3v1alpha1.LocalObjectReference) []reconcile.Request {
	requests := make(map[types.NamespacedName]struct{}, len(references))
	for _, reference := range references {
		requests[types.NamespacedName{Namespace: namespace, Name: reference.Name}] = struct{}{}
	}
	return sortedRequests(requests)
}

func sortedRequests(values map[types.NamespacedName]struct{}) []reconcile.Request {
	keys := make([]types.NamespacedName, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		return keys[i].Name < keys[j].Name
	})
	result := make([]reconcile.Request, 0, len(keys))
	for _, key := range keys {
		result = append(result, reconcile.Request{NamespacedName: key})
	}
	return result
}
