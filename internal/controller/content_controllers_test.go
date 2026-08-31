package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestContentReconcilersReportSupportAndAttachmentPolicy(t *testing.T) {
	scheme := controllerTestScheme(t)
	revision := "sha256:" + strings.Repeat("a", 64)
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	workstation.Status.DesiredRevision = revision
	workstation.Status.LiveRevision = revision
	workstation.Status.Conditions = []metav1.Condition{{Type: conditionReady, Status: metav1.ConditionTrue, Reason: "RuntimeReady"}}
	harness := &t3v1alpha1.Harness{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "cursor", Generation: 2},
		Spec: t3v1alpha1.HarnessSpec{
			InstanceID:      "cursor",
			Driver:          "cursor",
			WorkstationRefs: []t3v1alpha1.LocalObjectReference{{Name: "primary"}},
			AttachmentPolicy: t3v1alpha1.AttachmentPolicy{
				Extensions: t3v1alpha1.AttachmentPolicyNone,
				MCPServers: t3v1alpha1.AttachmentPolicyNone,
			},
		},
	}
	extension := &t3v1alpha1.Extension{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "skill", Generation: 3},
		Spec: t3v1alpha1.ExtensionSpec{
			Source: t3v1alpha1.ExtensionSource{
				Type: t3v1alpha1.ExtensionSourceGit,
				Git: &t3v1alpha1.GitExtensionSource{
					URL:    "https://example.test/skill.git",
					Commit: strings.Repeat("b", 40),
				},
			},
			HarnessRefs: []t3v1alpha1.LocalObjectReference{{Name: "cursor"}},
		},
	}
	server := &t3v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "remote", Generation: 4},
		Spec: t3v1alpha1.MCPServerSpec{
			Transport:   "http",
			Config:      jsonObject(`{"url":"https://mcp.example.test"}`),
			HarnessRefs: []t3v1alpha1.LocalObjectReference{{Name: "cursor"}},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&t3v1alpha1.Workstation{}, &t3v1alpha1.Harness{}, &t3v1alpha1.Extension{}, &t3v1alpha1.MCPServer{}).
		WithObjects(workstation, harness, extension, server).
		Build()

	if _, err := (&HarnessReconciler{Client: kube}).Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agents", Name: "cursor"}}); err != nil {
		t.Fatal(err)
	}
	storedHarness := &t3v1alpha1.Harness{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: "cursor"}, storedHarness); err != nil {
		t.Fatal(err)
	}
	if storedHarness.Status.AdapterSupport != t3v1alpha1.AdapterAlpha ||
		conditionStatus(storedHarness.Status.Conditions, conditionReady) != metav1.ConditionTrue ||
		len(storedHarness.Status.Attachments) != 1 {
		t.Fatalf("unexpected Harness status: %#v", storedHarness.Status)
	}

	if _, err := (&ExtensionReconciler{Client: kube}).Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agents", Name: "skill"}}); err != nil {
		t.Fatal(err)
	}
	storedExtension := &t3v1alpha1.Extension{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: "skill"}, storedExtension); err != nil {
		t.Fatal(err)
	}
	if storedExtension.Status.ResolvedSource == "" || len(storedExtension.Status.Attachments) != 1 ||
		conditionStatus(storedExtension.Status.Attachments[0].Conditions, conditionAccepted) != metav1.ConditionFalse {
		t.Fatalf("Extension policy status mismatch: %#v", storedExtension.Status)
	}

	if _, err := (&MCPServerReconciler{Client: kube}).Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agents", Name: "remote"}}); err != nil {
		t.Fatal(err)
	}
	storedServer := &t3v1alpha1.MCPServer{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: "remote"}, storedServer); err != nil {
		t.Fatal(err)
	}
	if len(storedServer.Status.Attachments) != 1 ||
		conditionStatus(storedServer.Status.Attachments[0].Conditions, conditionAccepted) != metav1.ConditionFalse ||
		conditionStatus(storedServer.Status.Conditions, conditionResolved) != metav1.ConditionTrue {
		t.Fatalf("MCP server policy status mismatch: %#v", storedServer.Status)
	}
}

func TestContentReconcilersRejectUnavailableDialects(t *testing.T) {
	scheme := controllerTestScheme(t)
	revision := "sha256:" + strings.Repeat("a", 64)
	harnesses := []client.Object{
		readyHarness("cursor", "cursor", revision),
		readyHarness("opencode", "opencode", revision),
	}
	gitExtension := &t3v1alpha1.Extension{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "cursor-skill", Generation: 2},
		Spec: t3v1alpha1.ExtensionSpec{
			Source: t3v1alpha1.ExtensionSource{
				Type: t3v1alpha1.ExtensionSourceGit,
				Git: &t3v1alpha1.GitExtensionSource{
					URL:    "https://example.test/skill.git",
					Commit: strings.Repeat("b", 40),
				},
			},
			HarnessRefs: []t3v1alpha1.LocalObjectReference{{Name: "cursor"}},
		},
	}
	marketplaceExtension := &t3v1alpha1.Extension{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "opencode-plugin", Generation: 3},
		Spec: t3v1alpha1.ExtensionSpec{
			Source: t3v1alpha1.ExtensionSource{
				Type: t3v1alpha1.ExtensionSourceMarketplace,
				Marketplace: &t3v1alpha1.MarketplaceExtensionSource{
					Marketplace:   "example",
					Extension:     "plugin",
					RepositoryURL: "https://example.test/plugins.git",
					Commit:        strings.Repeat("c", 40),
				},
			},
			HarnessRefs: []t3v1alpha1.LocalObjectReference{{Name: "opencode"}},
		},
	}
	server := &t3v1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "cursor-mcp", Generation: 4},
		Spec: t3v1alpha1.MCPServerSpec{
			Transport:   "http",
			Config:      jsonObject(`{"url":"https://mcp.example.test"}`),
			HarnessRefs: []t3v1alpha1.LocalObjectReference{{Name: "cursor"}},
		},
	}
	objects := append(harnesses, gitExtension, marketplaceExtension, server)
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&t3v1alpha1.Harness{}, &t3v1alpha1.Extension{}, &t3v1alpha1.MCPServer{}).
		WithObjects(objects...).
		Build()

	extensionReconciler := &ExtensionReconciler{Client: kube}
	for _, name := range []string{"cursor-skill", "opencode-plugin"} {
		request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agents", Name: name}}
		if _, err := extensionReconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		stored := &t3v1alpha1.Extension{}
		if err := kube.Get(context.Background(), request.NamespacedName, stored); err != nil {
			t.Fatal(err)
		}
		assertUnavailableDialectStatus(t, stored.Status.Attachments, stored.Status.Conditions)
	}

	serverRequest := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agents", Name: "cursor-mcp"}}
	if _, err := (&MCPServerReconciler{Client: kube}).Reconcile(context.Background(), serverRequest); err != nil {
		t.Fatal(err)
	}
	storedServer := &t3v1alpha1.MCPServer{}
	if err := kube.Get(context.Background(), serverRequest.NamespacedName, storedServer); err != nil {
		t.Fatal(err)
	}
	assertUnavailableDialectStatus(t, storedServer.Status.Attachments, storedServer.Status.Conditions)
}

func readyHarness(name, driver, revision string) *t3v1alpha1.Harness {
	return &t3v1alpha1.Harness{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: name},
		Spec: t3v1alpha1.HarnessSpec{
			InstanceID: name,
			Driver:     driver,
			AttachmentPolicy: t3v1alpha1.AttachmentPolicy{
				Extensions: t3v1alpha1.AttachmentPolicySameNamespace,
				MCPServers: t3v1alpha1.AttachmentPolicySameNamespace,
			},
		},
		Status: t3v1alpha1.HarnessStatus{
			ReconciledStatus: t3v1alpha1.ReconciledStatus{
				DesiredRevision: revision,
				LiveRevision:    revision,
				Conditions: []metav1.Condition{{
					Type: conditionReady, Status: metav1.ConditionTrue, Reason: "Ready",
				}},
			},
		},
	}
}

func assertUnavailableDialectStatus(t *testing.T, attachments []t3v1alpha1.AttachmentStatus, conditions []metav1.Condition) {
	t.Helper()
	if len(attachments) != 1 {
		t.Fatalf("attachment count = %d, want 1", len(attachments))
	}
	accepted := meta.FindStatusCondition(attachments[0].Conditions, conditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse || accepted.Reason != "DialectUnavailable" {
		t.Fatalf("unexpected attachment status: %#v", attachments[0])
	}
	if conditionStatus(conditions, conditionReady) != metav1.ConditionFalse {
		t.Fatalf("resource reported Ready despite unavailable dialect: %#v", conditions)
	}
}

func TestHarnessReconcilerReportsUnknownDriverWithoutChangingSchema(t *testing.T) {
	scheme := controllerTestScheme(t)
	harness := &t3v1alpha1.Harness{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "future"},
		Spec: t3v1alpha1.HarnessSpec{
			InstanceID:      "future",
			Driver:          "futureDriver",
			WorkstationRefs: []t3v1alpha1.LocalObjectReference{{Name: "missing"}},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&t3v1alpha1.Harness{}).WithObjects(harness).Build()
	if _, err := (&HarnessReconciler{Client: kube}).Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agents", Name: "future"}}); err != nil {
		t.Fatal(err)
	}
	stored := &t3v1alpha1.Harness{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: "future"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.AdapterSupport != t3v1alpha1.AdapterUnsupported ||
		conditionStatus(stored.Status.Conditions, conditionResolved) != metav1.ConditionFalse {
		t.Fatalf("unknown driver status mismatch: %#v", stored.Status)
	}
}

func TestHarnessDeletionWaitsForTheRemovalRevision(t *testing.T) {
	scheme := controllerTestScheme(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	workstation := controllerTestWorkstation()
	workstation.Spec.Tools = nil
	oldRevision := "sha256:" + strings.Repeat("a", 64)
	workstation.Status.DesiredRevision = oldRevision
	workstation.Status.LiveRevision = oldRevision
	workstation.Status.Conditions = []metav1.Condition{{Type: conditionProgrammed, Status: metav1.ConditionTrue, Reason: "Programmed"}}
	deletionTimestamp := metav1.NewTime(time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC))
	harness := &t3v1alpha1.Harness{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "agents",
			Name:              "codex",
			Finalizers:        []string{contentFinalizer},
			DeletionTimestamp: &deletionTimestamp,
		},
		Spec: t3v1alpha1.HarnessSpec{
			InstanceID:      "codex",
			Driver:          "codex",
			WorkstationRefs: []t3v1alpha1.LocalObjectReference{{Name: "primary"}},
		},
	}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: NamesForWorkstation("primary").Base}}
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&t3v1alpha1.Workstation{}, &t3v1alpha1.Harness{}).
		WithObjects(workstation, harness, deployment).
		Build()
	assembler := &Assembler{Reader: kube}
	reconciler := &HarnessReconciler{Client: kube, Assembler: assembler}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agents", Name: "codex"}}

	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("Harness deletion did not wait for the removal revision")
	}
	pending := &t3v1alpha1.Harness{}
	if err := kube.Get(context.Background(), request.NamespacedName, pending); err != nil {
		t.Fatal(err)
	}
	if !containsString(pending.Finalizers, contentFinalizer) || conditionStatus(pending.Status.Conditions, conditionReady) != metav1.ConditionFalse {
		t.Fatalf("Harness did not report pending removal: %#v", pending)
	}

	assembly, err := assembler.Assemble(context.Background(), workstation)
	if err != nil {
		t.Fatal(err)
	}
	storedWorkstation := &t3v1alpha1.Workstation{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "agents", Name: "primary"}, storedWorkstation); err != nil {
		t.Fatal(err)
	}
	storedWorkstation.Status.DesiredRevision = assembly.Manifest.DesiredRevision
	storedWorkstation.Status.LiveRevision = assembly.Manifest.DesiredRevision
	storedWorkstation.Status.Conditions = []metav1.Condition{{Type: conditionProgrammed, Status: metav1.ConditionTrue, Reason: "Programmed"}}
	if err := kube.Status().Update(context.Background(), storedWorkstation); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	completed := &t3v1alpha1.Harness{}
	err = kube.Get(context.Background(), request.NamespacedName, completed)
	if err == nil && containsString(completed.Finalizers, contentFinalizer) {
		t.Fatalf("Harness finalizer remained after live removal: %#v", completed.Finalizers)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}
