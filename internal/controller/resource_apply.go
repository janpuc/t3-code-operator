package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const serviceManagedAnnotationsAnnotation = "t3code.janpuc.com/managed-service-annotations"

func ensureManifestConfigMap(ctx context.Context, kube client.Client, workstation *t3v1alpha1.Workstation, desired *corev1.ConfigMap) error {
	current := &corev1.ConfigMap{}
	key := client.ObjectKeyFromObject(desired)
	if err := kube.Get(ctx, key, current); apierrors.IsNotFound(err) {
		return kube.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	if err := requireWorkstationOwner(current, workstation); err != nil {
		return err
	}
	changed := mergeManagedMetadata(&current.ObjectMeta, desired.ObjectMeta)
	if !reflect.DeepEqual(current.Data, desired.Data) || !reflect.DeepEqual(current.BinaryData, desired.BinaryData) {
		current.Data = cloneStringMap(desired.Data)
		current.BinaryData = nil
		changed = true
	}
	if !changed {
		return nil
	}
	return kube.Update(ctx, current)
}

func ensureReportConfigMap(ctx context.Context, kube client.Client, workstation *t3v1alpha1.Workstation, desired *corev1.ConfigMap) error {
	current := &corev1.ConfigMap{}
	key := client.ObjectKeyFromObject(desired)
	if err := kube.Get(ctx, key, current); apierrors.IsNotFound(err) {
		return kube.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	if err := requireWorkstationOwner(current, workstation); err != nil {
		return err
	}
	if !mergeManagedMetadata(&current.ObjectMeta, desired.ObjectMeta) {
		return nil
	}
	return kube.Update(ctx, current)
}

func ensureRole(ctx context.Context, kube client.Client, workstation *t3v1alpha1.Workstation, desired *rbacv1.Role) error {
	current := &rbacv1.Role{}
	key := client.ObjectKeyFromObject(desired)
	if err := kube.Get(ctx, key, current); apierrors.IsNotFound(err) {
		return kube.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	if err := requireWorkstationOwner(current, workstation); err != nil {
		return err
	}
	changed := mergeManagedMetadata(&current.ObjectMeta, desired.ObjectMeta)
	if !reflect.DeepEqual(current.Rules, desired.Rules) {
		current.Rules = desired.DeepCopy().Rules
		changed = true
	}
	if !changed {
		return nil
	}
	return kube.Update(ctx, current)
}

func ensureRoleBinding(ctx context.Context, kube client.Client, workstation *t3v1alpha1.Workstation, desired *rbacv1.RoleBinding) error {
	current := &rbacv1.RoleBinding{}
	key := client.ObjectKeyFromObject(desired)
	if err := kube.Get(ctx, key, current); apierrors.IsNotFound(err) {
		return kube.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	if err := requireWorkstationOwner(current, workstation); err != nil {
		return err
	}
	changed := mergeManagedMetadata(&current.ObjectMeta, desired.ObjectMeta)
	if current.RoleRef != desired.RoleRef || !reflect.DeepEqual(current.Subjects, desired.Subjects) {
		current.RoleRef = desired.RoleRef
		current.Subjects = append([]rbacv1.Subject(nil), desired.Subjects...)
		changed = true
	}
	if !changed {
		return nil
	}
	return kube.Update(ctx, current)
}

func ensureService(ctx context.Context, kube client.Client, workstation *t3v1alpha1.Workstation, desired *corev1.Service) error {
	current := &corev1.Service{}
	key := client.ObjectKeyFromObject(desired)
	if err := kube.Get(ctx, key, current); apierrors.IsNotFound(err) {
		return kube.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	if err := requireWorkstationOwner(current, workstation); err != nil {
		return err
	}
	previousManagedAnnotations := serviceManagedAnnotationKeys(current.Annotations)
	changed := mergeManagedMetadata(&current.ObjectMeta, desired.ObjectMeta)
	for _, key := range previousManagedAnnotations {
		if _, remainsManaged := desired.Annotations[key]; remainsManaged {
			continue
		}
		if _, exists := current.Annotations[key]; exists {
			delete(current.Annotations, key)
			changed = true
		}
	}
	desiredPorts := preserveAllocatedNodePorts(current.Spec.Ports, desired.Spec.Ports, desired.Spec.Type)
	desiredHealthCheckNodePort := current.Spec.HealthCheckNodePort
	if desired.Spec.Type != corev1.ServiceTypeLoadBalancer ||
		desired.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		desiredHealthCheckNodePort = 0
	}
	if !reflect.DeepEqual(current.Spec.Selector, desired.Spec.Selector) ||
		!reflect.DeepEqual(current.Spec.Ports, desiredPorts) ||
		current.Spec.Type != desired.Spec.Type ||
		current.Spec.ExternalTrafficPolicy != desired.Spec.ExternalTrafficPolicy ||
		!reflect.DeepEqual(current.Spec.LoadBalancerSourceRanges, desired.Spec.LoadBalancerSourceRanges) ||
		current.Spec.HealthCheckNodePort != desiredHealthCheckNodePort {
		current.Spec.Selector = cloneStringMap(desired.Spec.Selector)
		current.Spec.Ports = desiredPorts
		current.Spec.Type = desired.Spec.Type
		current.Spec.ExternalTrafficPolicy = desired.Spec.ExternalTrafficPolicy
		current.Spec.LoadBalancerSourceRanges = append([]string(nil), desired.Spec.LoadBalancerSourceRanges...)
		current.Spec.HealthCheckNodePort = desiredHealthCheckNodePort
		if desired.Spec.Type != corev1.ServiceTypeLoadBalancer {
			current.Spec.AllocateLoadBalancerNodePorts = nil
			current.Spec.LoadBalancerClass = nil
			current.Spec.LoadBalancerIP = ""
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return kube.Update(ctx, current)
}

func ensureOptionalService(
	ctx context.Context,
	kube client.Client,
	workstation *t3v1alpha1.Workstation,
	key types.NamespacedName,
	desired *corev1.Service,
) error {
	if desired != nil {
		return ensureService(ctx, kube, workstation, desired)
	}
	current := &corev1.Service{}
	if err := kube.Get(ctx, key, current); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := requireWorkstationOwner(current, workstation); err != nil {
		return err
	}
	return kube.Delete(ctx, current)
}

func managedServiceAnnotations(input map[string]string) map[string]string {
	result := cloneStringMap(input)
	keys := make([]string, 0, len(input))
	for key := range input {
		if key != serviceManagedAnnotationsAnnotation {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	raw, _ := json.Marshal(keys)
	result[serviceManagedAnnotationsAnnotation] = string(raw)
	return result
}

func serviceManagedAnnotationKeys(annotations map[string]string) []string {
	if annotations == nil {
		return nil
	}
	var keys []string
	if json.Unmarshal([]byte(annotations[serviceManagedAnnotationsAnnotation]), &keys) != nil {
		return nil
	}
	return keys
}

func preserveAllocatedNodePorts(current, desired []corev1.ServicePort, serviceType corev1.ServiceType) []corev1.ServicePort {
	result := append([]corev1.ServicePort(nil), desired...)
	if serviceType != corev1.ServiceTypeNodePort && serviceType != corev1.ServiceTypeLoadBalancer {
		return result
	}
	for index := range result {
		if result[index].NodePort != 0 {
			continue
		}
		for _, existing := range current {
			if existing.Name == result[index].Name && existing.Protocol == result[index].Protocol && existing.Port == result[index].Port {
				result[index].NodePort = existing.NodePort
				break
			}
		}
	}
	return result
}

func ensurePDB(ctx context.Context, kube client.Client, workstation *t3v1alpha1.Workstation, desired *policyv1.PodDisruptionBudget) error {
	current := &policyv1.PodDisruptionBudget{}
	key := client.ObjectKeyFromObject(desired)
	if err := kube.Get(ctx, key, current); apierrors.IsNotFound(err) {
		return kube.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	if err := requireWorkstationOwner(current, workstation); err != nil {
		return err
	}
	changed := mergeManagedMetadata(&current.ObjectMeta, desired.ObjectMeta)
	if !reflect.DeepEqual(current.Spec, desired.Spec) {
		current.Spec = *desired.Spec.DeepCopy()
		changed = true
	}
	if !changed {
		return nil
	}
	return kube.Update(ctx, current)
}

func getDeployment(ctx context.Context, kube client.Client, key types.NamespacedName) (*appsv1.Deployment, bool, error) {
	deployment := &appsv1.Deployment{}
	if err := kube.Get(ctx, key, deployment); apierrors.IsNotFound(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	return deployment, true, nil
}

func createOrUpdateDeployment(
	ctx context.Context,
	kube client.Client,
	workstation *t3v1alpha1.Workstation,
	current *appsv1.Deployment,
	desired *appsv1.Deployment,
) (*appsv1.Deployment, error) {
	if current == nil {
		if err := kube.Create(ctx, desired); err != nil {
			return nil, err
		}
		return desired, nil
	}
	if err := requireWorkstationOwner(current, workstation); err != nil {
		return nil, err
	}
	changed := mergeManagedMetadata(&current.ObjectMeta, desired.ObjectMeta)
	if !reflect.DeepEqual(current.Spec, desired.Spec) {
		current.Spec = *desired.Spec.DeepCopy()
		changed = true
	}
	if !changed {
		return current, nil
	}
	if err := kube.Update(ctx, current); err != nil {
		return nil, err
	}
	return current, nil
}

func ensureClaims(
	ctx context.Context,
	kube client.Client,
	workstation *t3v1alpha1.Workstation,
	claims []*corev1.PersistentVolumeClaim,
) error {
	for _, desired := range claims {
		current := &corev1.PersistentVolumeClaim{}
		key := client.ObjectKeyFromObject(desired)
		if err := kube.Get(ctx, key, current); apierrors.IsNotFound(err) {
			if err := kube.Create(ctx, desired); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}
		if err := requireClaimIdentity(current, workstation, desired.Annotations[claimVolumeAnnotation]); err != nil {
			return err
		}
		claimChanged, compatible := mergeClaimTemplateSpec(current, desired)
		if !compatible {
			return fmt.Errorf("PersistentVolumeClaim %s storage identity does not match its ClaimTemplate", current.Name)
		}
		changed := claimChanged
		if current.Labels == nil {
			current.Labels = map[string]string{}
		}
		for key, value := range desired.Labels {
			if current.Labels[key] != value {
				current.Labels[key] = value
				changed = true
			}
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		for key, value := range desired.Annotations {
			if current.Annotations[key] != value {
				current.Annotations[key] = value
				changed = true
			}
		}
		if changed {
			if err := kube.Update(ctx, current); err != nil {
				return err
			}
		}
	}
	return validateExistingClaims(ctx, kube, workstation)
}

func mergeClaimTemplateSpec(current, desired *corev1.PersistentVolumeClaim) (bool, bool) {
	currentIdentity := current.Spec.DeepCopy()
	desiredStorage, desiredHasStorage := desired.Spec.Resources.Requests[corev1.ResourceStorage]
	currentStorage, currentHasStorage := current.Spec.Resources.Requests[corev1.ResourceStorage]
	if desiredHasStorage {
		if currentIdentity.Resources.Requests == nil {
			currentIdentity.Resources.Requests = corev1.ResourceList{}
		}
		currentIdentity.Resources.Requests[corev1.ResourceStorage] = desiredStorage.DeepCopy()
	}
	if !apiequality.Semantic.DeepDerivative(desired.Spec, *currentIdentity) {
		return false, false
	}
	if !desiredHasStorage || currentHasStorage && desiredStorage.Cmp(currentStorage) <= 0 {
		return false, true
	}
	if current.Spec.Resources.Requests == nil {
		current.Spec.Resources.Requests = corev1.ResourceList{}
	}
	current.Spec.Resources.Requests[corev1.ResourceStorage] = desiredStorage.DeepCopy()
	return true, true
}

func validateExistingClaims(ctx context.Context, kube client.Client, workstation *t3v1alpha1.Workstation) error {
	claimNames := make([]string, 0, 2)
	storage := effectiveStorage(workstation)
	if source := storage.Data.ExistingClaim; source != nil {
		claimNames = append(claimNames, source.Name)
	}
	if source := storage.Workspace.ExistingClaim; source != nil {
		claimNames = append(claimNames, source.Name)
	}
	for _, name := range claimNames {
		claim := &corev1.PersistentVolumeClaim{}
		if err := kube.Get(ctx, types.NamespacedName{Namespace: workstation.Namespace, Name: name}, claim); err != nil {
			return fmt.Errorf("get existing PersistentVolumeClaim %s: %w", name, err)
		}
		if claim.Spec.VolumeMode != nil && *claim.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
			return fmt.Errorf("existing PersistentVolumeClaim %s volumeMode must be Filesystem", name)
		}
	}
	return nil
}

func requireClaimIdentity(claim *corev1.PersistentVolumeClaim, workstation *t3v1alpha1.Workstation, volume string) error {
	if claim.Annotations[claimWorkstationAnnotation] != workstation.Name ||
		claim.Annotations[claimVolumeAnnotation] != volume {
		return fmt.Errorf("PersistentVolumeClaim %s is not owned by this Workstation identity", claim.Name)
	}
	if claim.Annotations[claimWorkstationUIDAnnotation] != string(workstation.UID) &&
		claim.Annotations[claimRetentionAnnotation] != string(t3v1alpha1.ClaimRetentionRetain) {
		return fmt.Errorf("PersistentVolumeClaim %s is not owned by this Workstation identity", claim.Name)
	}
	return nil
}

func requireWorkstationOwner(object metav1.Object, workstation *t3v1alpha1.Workstation) error {
	for _, reference := range object.GetOwnerReferences() {
		if reference.Controller != nil && *reference.Controller {
			if reference.APIVersion == t3v1alpha1.GroupVersion.String() && reference.Kind == "Workstation" &&
				reference.Name == workstation.Name && reference.UID == workstation.UID {
				return nil
			}
			return fmt.Errorf("resource %s is controlled by another owner", object.GetName())
		}
	}
	return fmt.Errorf("resource %s already exists without the Workstation controller owner", object.GetName())
}

func mergeManagedMetadata(current *metav1.ObjectMeta, desired metav1.ObjectMeta) bool {
	changed := false
	if current.Labels == nil {
		current.Labels = map[string]string{}
	}
	for key, value := range desired.Labels {
		if current.Labels[key] != value {
			current.Labels[key] = value
			changed = true
		}
	}
	if current.Annotations == nil && len(desired.Annotations) != 0 {
		current.Annotations = map[string]string{}
	}
	for key, value := range desired.Annotations {
		if current.Annotations[key] != value {
			current.Annotations[key] = value
			changed = true
		}
	}
	if !reflect.DeepEqual(current.OwnerReferences, desired.OwnerReferences) {
		current.OwnerReferences = append([]metav1.OwnerReference(nil), desired.OwnerReferences...)
		changed = true
	}
	return changed
}

type generatedClaimIdentity struct {
	name   string
	volume string
}

func cleanupDetachedClaims(
	ctx context.Context,
	kube client.Client,
	workstation *t3v1alpha1.Workstation,
	names ResourceNames,
) error {
	return deleteManagedClaims(ctx, kube, workstation, names, currentStorageClaimNames(workstation, names))
}

func deleteManagedClaims(
	ctx context.Context,
	kube client.Client,
	workstation *t3v1alpha1.Workstation,
	names ResourceNames,
	protected map[string]struct{},
) error {
	for _, identity := range []generatedClaimIdentity{
		{name: names.DataClaim, volume: "data"},
		{name: names.WorkspaceClaim, volume: "workspace"},
	} {
		if _, exists := protected[identity.name]; exists {
			continue
		}
		if err := deleteManagedClaim(ctx, kube, workstation, identity); err != nil {
			return err
		}
	}
	return nil
}

func deleteManagedClaim(
	ctx context.Context,
	kube client.Client,
	workstation *t3v1alpha1.Workstation,
	identity generatedClaimIdentity,
) error {
	claim := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: workstation.Namespace, Name: identity.name}
	if err := kube.Get(ctx, key, claim); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if requireClaimIdentity(claim, workstation, identity.volume) != nil ||
		claim.Annotations[claimRetentionAnnotation] != string(t3v1alpha1.ClaimRetentionDelete) {
		return nil
	}
	if err := kube.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func currentStorageClaimNames(workstation *t3v1alpha1.Workstation, names ResourceNames) map[string]struct{} {
	result := existingClaimNames(workstation)
	storage := effectiveStorage(workstation)
	if storage.Data.Type == t3v1alpha1.DataVolumeClaimTemplate {
		result[names.DataClaim] = struct{}{}
	}
	if storage.Workspace.Type == t3v1alpha1.WorkspaceVolumeClaimTemplate {
		result[names.WorkspaceClaim] = struct{}{}
	}
	return result
}

func existingClaimNames(workstation *t3v1alpha1.Workstation) map[string]struct{} {
	result := make(map[string]struct{}, 2)
	storage := effectiveStorage(workstation)
	if source := storage.Data.ExistingClaim; source != nil {
		result[source.Name] = struct{}{}
	}
	if source := storage.Workspace.ExistingClaim; source != nil {
		result[source.Name] = struct{}{}
	}
	return result
}
