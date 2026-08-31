package controller

import (
	"context"
	"reflect"
	"sort"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/render"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	conditionAccepted = "Accepted"
)

type HarnessReconciler struct {
	client.Client
	Assembler *Assembler
}

type ExtensionReconciler struct {
	client.Client
	Assembler *Assembler
}

type MCPServerReconciler struct {
	client.Client
	Assembler *Assembler
}

func (reconciler *HarnessReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	harness := &t3v1alpha1.Harness{}
	if err := reconciler.Get(ctx, request.NamespacedName, harness); apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}
	if !harness.DeletionTimestamp.IsZero() {
		return reconciler.reconcileDeletion(ctx, harness)
	}
	if !controllerutil.ContainsFinalizer(harness, contentFinalizer) {
		controllerutil.AddFinalizer(harness, contentFinalizer)
		if err := reconciler.Update(ctx, harness); err != nil {
			return ctrl.Result{}, err
		}
	}
	resolved := convertHarness(harness.Namespace, harness)
	support, validationErr := render.ValidateHarness(harness.Namespace, resolved)
	attachments, attachmentsReady, err := reconciler.workstationAttachments(ctx, harness)
	if err != nil {
		return ctrl.Result{}, err
	}
	before := harness.DeepCopy()
	harness.Status.ObservedGeneration = harness.Generation
	harness.Status.Attachments = attachments
	harness.Status.DesiredRevision, harness.Status.LiveRevision = aggregateAttachmentRevisions(attachments)
	if validationErr != nil {
		harness.Status.AdapterSupport = t3v1alpha1.AdapterUnsupported
		setCondition(&harness.Status.Conditions, conditionResolved, metav1.ConditionFalse, "ValidationFailed", "The Harness configuration is not supported.", harness.Generation)
		setCondition(&harness.Status.Conditions, conditionReady, metav1.ConditionFalse, "ValidationFailed", "The Harness is not ready.", harness.Generation)
	} else {
		harness.Status.AdapterSupport = adapterSupportLevel(support)
		setCondition(&harness.Status.Conditions, conditionResolved, metav1.ConditionTrue, "Resolved", "The Harness configuration resolved.", harness.Generation)
		if attachmentsReady {
			setCondition(&harness.Status.Conditions, conditionReady, metav1.ConditionTrue, "AttachmentsReady", "All Workstation attachments are ready.", harness.Generation)
		} else {
			setCondition(&harness.Status.Conditions, conditionReady, metav1.ConditionFalse, "AttachmentsPending", "One or more Workstation attachments are not ready.", harness.Generation)
		}
	}
	if reflect.DeepEqual(before.Status, harness.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, reconciler.Status().Patch(ctx, harness, client.MergeFrom(before))
}

func (reconciler *ExtensionReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	extension := &t3v1alpha1.Extension{}
	if err := reconciler.Get(ctx, request.NamespacedName, extension); apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}
	if !extension.DeletionTimestamp.IsZero() {
		return reconciler.reconcileDeletion(ctx, extension)
	}
	if !controllerutil.ContainsFinalizer(extension, contentFinalizer) {
		controllerutil.AddFinalizer(extension, contentFinalizer)
		if err := reconciler.Update(ctx, extension); err != nil {
			return ctrl.Result{}, err
		}
	}
	resolved := convertExtension(extension.Namespace, extension)
	validationErr := render.ValidateExtension(extension.Namespace, resolved)
	attachments, attachmentsReady, err := reconciler.harnessAttachments(
		ctx,
		extension.Namespace,
		extension.Spec.HarnessRefs,
		resolved.Source.Type,
		extension.Generation,
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	before := extension.DeepCopy()
	extension.Status.ObservedGeneration = extension.Generation
	extension.Status.Attachments = attachments
	extension.Status.DesiredRevision, extension.Status.LiveRevision = aggregateAttachmentRevisions(attachments)
	if validationErr != nil {
		extension.Status.ResolvedSource = ""
		setCondition(&extension.Status.Conditions, conditionResolved, metav1.ConditionFalse, "ValidationFailed", "The Extension source is invalid.", extension.Generation)
		setCondition(&extension.Status.Conditions, conditionReady, metav1.ConditionFalse, "ValidationFailed", "The Extension is not ready.", extension.Generation)
	} else {
		cacheKey, err := render.ExtensionCacheKey(resolved.Source)
		if err != nil {
			return ctrl.Result{}, err
		}
		extension.Status.ResolvedSource = cacheKey
		setCondition(&extension.Status.Conditions, conditionResolved, metav1.ConditionTrue, "Resolved", "The Extension source resolved.", extension.Generation)
		setAttachmentReadyCondition(&extension.Status.Conditions, attachmentsReady, "Extension", extension.Generation)
	}
	if reflect.DeepEqual(before.Status, extension.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, reconciler.Status().Patch(ctx, extension, client.MergeFrom(before))
}

func (reconciler *MCPServerReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	server := &t3v1alpha1.MCPServer{}
	if err := reconciler.Get(ctx, request.NamespacedName, server); apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}
	if !server.DeletionTimestamp.IsZero() {
		return reconciler.reconcileDeletion(ctx, server)
	}
	if !controllerutil.ContainsFinalizer(server, contentFinalizer) {
		controllerutil.AddFinalizer(server, contentFinalizer)
		if err := reconciler.Update(ctx, server); err != nil {
			return ctrl.Result{}, err
		}
	}
	validationErr := render.ValidateMCPServer(server.Namespace, convertMCPServer(server.Namespace, server))
	attachments, attachmentsReady, err := reconciler.harnessAttachments(ctx, server.Namespace, server.Spec.HarnessRefs, server.Generation)
	if err != nil {
		return ctrl.Result{}, err
	}
	before := server.DeepCopy()
	server.Status.ObservedGeneration = server.Generation
	server.Status.Attachments = attachments
	server.Status.DesiredRevision, server.Status.LiveRevision = aggregateAttachmentRevisions(attachments)
	if validationErr != nil {
		setCondition(&server.Status.Conditions, conditionResolved, metav1.ConditionFalse, "ValidationFailed", "The MCP server configuration is invalid.", server.Generation)
		setCondition(&server.Status.Conditions, conditionReady, metav1.ConditionFalse, "ValidationFailed", "The MCP server is not ready.", server.Generation)
	} else {
		setCondition(&server.Status.Conditions, conditionResolved, metav1.ConditionTrue, "Resolved", "The MCP server configuration resolved.", server.Generation)
		setAttachmentReadyCondition(&server.Status.Conditions, attachmentsReady, "MCP server", server.Generation)
	}
	if reflect.DeepEqual(before.Status, server.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, reconciler.Status().Patch(ctx, server, client.MergeFrom(before))
}

func (reconciler *HarnessReconciler) workstationAttachments(
	ctx context.Context,
	harness *t3v1alpha1.Harness,
) ([]t3v1alpha1.AttachmentStatus, bool, error) {
	attachments := make([]t3v1alpha1.AttachmentStatus, 0, len(harness.Spec.WorkstationRefs))
	ready := true
	for _, reference := range harness.Spec.WorkstationRefs {
		workstation := &t3v1alpha1.Workstation{}
		err := reconciler.Get(ctx, client.ObjectKey{Namespace: harness.Namespace, Name: reference.Name}, workstation)
		if apierrors.IsNotFound(err) {
			attachments = append(attachments, rejectedAttachment(reference.Name, "TargetNotFound", harness.Generation))
			ready = false
			continue
		}
		if err != nil {
			return nil, false, err
		}
		attachment := acceptedAttachment(
			reference.Name,
			workstation.Status.DesiredRevision,
			workstation.Status.LiveRevision,
			conditionIsTrue(workstation.Status.Conditions, conditionReady),
			harness.Generation,
		)
		if !attachmentIsReady(attachment) {
			ready = false
		}
		attachments = append(attachments, attachment)
	}
	sortAttachmentStatuses(attachments)
	return attachments, ready, nil
}

func (reconciler *ExtensionReconciler) harnessAttachments(
	ctx context.Context,
	namespace string,
	references []t3v1alpha1.LocalObjectReference,
	sourceType render.ExtensionSourceType,
	generation int64,
) ([]t3v1alpha1.AttachmentStatus, bool, error) {
	return harnessAttachmentStatuses(ctx, reconciler.Client, namespace, references, true, generation, func(driver string) bool {
		return render.ProgramsExtensionSource(driver, sourceType)
	})
}

func (reconciler *MCPServerReconciler) harnessAttachments(
	ctx context.Context,
	namespace string,
	references []t3v1alpha1.LocalObjectReference,
	generation int64,
) ([]t3v1alpha1.AttachmentStatus, bool, error) {
	return harnessAttachmentStatuses(ctx, reconciler.Client, namespace, references, false, generation, render.ProgramsMCPServers)
}

func harnessAttachmentStatuses(
	ctx context.Context,
	kube client.Client,
	namespace string,
	references []t3v1alpha1.LocalObjectReference,
	extension bool,
	generation int64,
	programs func(string) bool,
) ([]t3v1alpha1.AttachmentStatus, bool, error) {
	attachments := make([]t3v1alpha1.AttachmentStatus, 0, len(references))
	ready := true
	for _, reference := range references {
		harness := &t3v1alpha1.Harness{}
		err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: reference.Name}, harness)
		if apierrors.IsNotFound(err) {
			attachments = append(attachments, rejectedAttachment(reference.Name, "TargetNotFound", generation))
			ready = false
			continue
		}
		if err != nil {
			return nil, false, err
		}
		allowed := attachmentsAllowed(harness.Spec.AttachmentPolicy.MCPServers)
		if extension {
			allowed = attachmentsAllowed(harness.Spec.AttachmentPolicy.Extensions)
		}
		if !allowed {
			attachments = append(attachments, rejectedAttachment(reference.Name, "AttachmentNotAllowed", generation))
			ready = false
			continue
		}
		if !programs(harness.Spec.Driver) {
			attachments = append(attachments, rejectedAttachment(reference.Name, "DialectUnavailable", generation))
			ready = false
			continue
		}
		attachment := acceptedAttachment(
			reference.Name,
			harness.Status.DesiredRevision,
			harness.Status.LiveRevision,
			conditionIsTrue(harness.Status.Conditions, conditionReady),
			generation,
		)
		if !attachmentIsReady(attachment) {
			ready = false
		}
		attachments = append(attachments, attachment)
	}
	sortAttachmentStatuses(attachments)
	return attachments, ready, nil
}

func rejectedAttachment(name, reason string, generation int64) t3v1alpha1.AttachmentStatus {
	attachment := t3v1alpha1.AttachmentStatus{TargetName: name}
	setCondition(&attachment.Conditions, conditionAccepted, metav1.ConditionFalse, reason, "The target did not accept this attachment.", generation)
	setCondition(&attachment.Conditions, conditionReady, metav1.ConditionFalse, reason, "The attachment is not ready.", generation)
	return attachment
}

func acceptedAttachment(name, desired, live string, targetReady bool, generation int64) t3v1alpha1.AttachmentStatus {
	attachment := t3v1alpha1.AttachmentStatus{TargetName: name, DesiredRevision: desired, LiveRevision: live}
	setCondition(&attachment.Conditions, conditionAccepted, metav1.ConditionTrue, "Accepted", "The target accepted this attachment.", generation)
	programmed := desired != "" && desired == live
	if programmed {
		setCondition(&attachment.Conditions, conditionProgrammed, metav1.ConditionTrue, "Programmed", "The target applied this attachment revision.", generation)
	} else {
		setCondition(&attachment.Conditions, conditionProgrammed, metav1.ConditionFalse, "ApplyPending", "The target has not applied this attachment revision.", generation)
	}
	if programmed && targetReady {
		setCondition(&attachment.Conditions, conditionReady, metav1.ConditionTrue, "TargetReady", "The attachment target is ready.", generation)
	} else {
		setCondition(&attachment.Conditions, conditionReady, metav1.ConditionFalse, "TargetPending", "The attachment target is not ready.", generation)
	}
	return attachment
}

func setAttachmentReadyCondition(conditions *[]metav1.Condition, ready bool, subject string, generation int64) {
	if ready {
		setCondition(conditions, conditionReady, metav1.ConditionTrue, "AttachmentsReady", "All "+subject+" attachments are ready.", generation)
	} else {
		setCondition(conditions, conditionReady, metav1.ConditionFalse, "AttachmentsPending", "One or more "+subject+" attachments are not ready.", generation)
	}
}

func attachmentIsReady(attachment t3v1alpha1.AttachmentStatus) bool {
	return conditionIsTrue(attachment.Conditions, conditionReady)
}

func conditionIsTrue(conditions []metav1.Condition, conditionType string) bool {
	return meta.IsStatusConditionTrue(conditions, conditionType)
}

func sortAttachmentStatuses(attachments []t3v1alpha1.AttachmentStatus) {
	sort.Slice(attachments, func(i, j int) bool { return attachments[i].TargetName < attachments[j].TargetName })
}

func aggregateAttachmentRevisions(attachments []t3v1alpha1.AttachmentStatus) (string, string) {
	if len(attachments) == 0 {
		return "", ""
	}
	desired := attachments[0].DesiredRevision
	live := attachments[0].LiveRevision
	for _, attachment := range attachments[1:] {
		if attachment.DesiredRevision != desired {
			desired = ""
		}
		if attachment.LiveRevision != live {
			live = ""
		}
	}
	return desired, live
}

func adapterSupportLevel(support render.SupportLevel) t3v1alpha1.AdapterSupportLevel {
	if support == render.SupportLevelAlpha {
		return t3v1alpha1.AdapterAlpha
	}
	return t3v1alpha1.AdapterSupported
}

func (reconciler *HarnessReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&t3v1alpha1.Harness{}).
		Watches(&t3v1alpha1.Workstation{}, handler.EnqueueRequestsFromMapFunc(reconciler.requestsForWorkstation)).
		Complete(reconciler)
}

func (reconciler *ExtensionReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&t3v1alpha1.Extension{}).
		Watches(&t3v1alpha1.Harness{}, handler.EnqueueRequestsFromMapFunc(reconciler.requestsForHarness)).
		Complete(reconciler)
}

func (reconciler *MCPServerReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&t3v1alpha1.MCPServer{}).
		Watches(&t3v1alpha1.Harness{}, handler.EnqueueRequestsFromMapFunc(reconciler.requestsForHarness)).
		Complete(reconciler)
}
