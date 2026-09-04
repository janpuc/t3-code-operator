package controller

import (
	"context"
	"sort"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/render"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (reconciler *WorkstationReconciler) requestsForHarness(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	harness, ok := object.(*t3v1alpha1.Harness)
	if !ok {
		return nil
	}
	if len(harness.Spec.WorkstationRefs) != 0 {
		return requestsForReferences(harness.Namespace, harness.Spec.WorkstationRefs)
	}
	return reconciler.requestsForAllWorkstations(ctx, harness.Namespace)
}

func (reconciler *WorkstationReconciler) requestsForExtension(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	extension, ok := object.(*t3v1alpha1.Extension)
	if !ok {
		return nil
	}
	sourceType := render.ExtensionSourceType(extension.Spec.Source.Type)
	return reconciler.requestsForAttachment(ctx, extension.Namespace, extension.Spec.HarnessRefs, true, func(driver string) bool {
		return render.ProgramsExtensionSource(driver, sourceType)
	})
}

func (reconciler *WorkstationReconciler) requestsForMCPServer(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	server, ok := object.(*t3v1alpha1.MCPServer)
	if !ok {
		return nil
	}
	return reconciler.requestsForAttachment(ctx, server.Namespace, server.Spec.HarnessRefs, false, render.ProgramsMCPServers)
}

func (reconciler *WorkstationReconciler) requestsForAttachment(
	ctx context.Context,
	namespace string,
	references []t3v1alpha1.LocalObjectReference,
	extension bool,
	programs func(string) bool,
) []reconcile.Request {
	targets, err := listProviderTargets(ctx, reconciler, namespace)
	if err != nil {
		return nil
	}
	selected, _ := selectProviderTargets(targets, references, extension, programs)
	return requestsForNames(namespace, workstationNamesForTargets(selected))
}

func (reconciler *WorkstationReconciler) requestsForAllWorkstations(ctx context.Context, namespace string) []reconcile.Request {
	list := &t3v1alpha1.WorkstationList{}
	if err := reconciler.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for index := range list.Items {
		names = append(names, list.Items[index].Name)
	}
	return requestsForNames(namespace, names)
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
		if harnessAttachesToWorkstation(&list.Items[index], workstation.Name) {
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
		if len(list.Items[index].Spec.HarnessRefs) == 0 || referencesName(list.Items[index].Spec.HarnessRefs, harness.Name) {
			requests[types.NamespacedName{Namespace: harness.Namespace, Name: list.Items[index].Name}] = struct{}{}
		}
	}
	return sortedRequests(requests)
}

func (reconciler *ExtensionReconciler) requestsForWorkstation(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	list := &t3v1alpha1.ExtensionList{}
	if err := reconciler.List(ctx, list, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for index := range list.Items {
		names = append(names, list.Items[index].Name)
	}
	return requestsForNames(object.GetNamespace(), names)
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
		if len(list.Items[index].Spec.HarnessRefs) == 0 || referencesName(list.Items[index].Spec.HarnessRefs, harness.Name) {
			requests[types.NamespacedName{Namespace: harness.Namespace, Name: list.Items[index].Name}] = struct{}{}
		}
	}
	return sortedRequests(requests)
}

func (reconciler *MCPServerReconciler) requestsForWorkstation(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	list := &t3v1alpha1.MCPServerList{}
	if err := reconciler.List(ctx, list, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for index := range list.Items {
		names = append(names, list.Items[index].Name)
	}
	return requestsForNames(object.GetNamespace(), names)
}

func requestsForReferences(namespace string, references []t3v1alpha1.LocalObjectReference) []reconcile.Request {
	names := make([]string, 0, len(references))
	for _, reference := range references {
		names = append(names, reference.Name)
	}
	return requestsForNames(namespace, names)
}

func requestsForNames(namespace string, names []string) []reconcile.Request {
	requests := make(map[types.NamespacedName]struct{}, len(names))
	for _, name := range names {
		requests[types.NamespacedName{Namespace: namespace, Name: name}] = struct{}{}
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
