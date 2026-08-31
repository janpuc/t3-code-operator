package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/janpuc/t3-code-operator/internal/apply"
	"github.com/janpuc/t3-code-operator/internal/render"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const ManifestDataKey = "manifest.json"

var ErrInvalidManifest = errors.New("rendered manifest is invalid")

type StoreConfig struct {
	Namespace             string
	ManifestConfigMapName string
	ReportConfigMapName   string
}

type KubernetesStore struct {
	configMaps            typedcorev1.ConfigMapInterface
	secrets               typedcorev1.SecretInterface
	manifestConfigMapName string
	reportConfigMapName   string
	namespace             string
}

func NewKubernetesStore(core typedcorev1.CoreV1Interface, config StoreConfig) (*KubernetesStore, error) {
	if core == nil {
		return nil, errors.New("Kubernetes CoreV1 client is required")
	}
	if problems := validation.IsDNS1123Label(config.Namespace); len(problems) != 0 {
		return nil, errors.New("Kubernetes namespace is invalid")
	}
	for _, name := range []string{config.ManifestConfigMapName, config.ReportConfigMapName} {
		if problems := validation.IsDNS1123Subdomain(name); len(problems) != 0 {
			return nil, errors.New("Kubernetes ConfigMap name is invalid")
		}
	}
	return &KubernetesStore{
		configMaps:            core.ConfigMaps(config.Namespace),
		secrets:               core.Secrets(config.Namespace),
		manifestConfigMapName: config.ManifestConfigMapName,
		reportConfigMapName:   config.ReportConfigMapName,
		namespace:             config.Namespace,
	}, nil
}

func (store *KubernetesStore) Resolve(
	ctx context.Context,
	reference render.SecretReference,
) (apply.SecretValue, error) {
	if reference.Namespace != store.namespace {
		return apply.SecretValue{}, errors.New("Secret reference must use the Workstation namespace")
	}
	if problems := validation.IsDNS1123Subdomain(reference.Name); len(problems) != 0 {
		return apply.SecretValue{}, errors.New("Secret reference is invalid")
	}
	if problems := validation.IsConfigMapKey(reference.Key); len(problems) != 0 {
		return apply.SecretValue{}, errors.New("Secret reference is invalid")
	}
	secret, err := store.secrets.Get(ctx, reference.Name, metav1.GetOptions{})
	if err != nil {
		return apply.SecretValue{}, fmt.Errorf("get Secret %s/%s: %w", store.namespace, reference.Name, err)
	}
	value, exists := secret.Data[reference.Key]
	if !exists {
		return apply.SecretValue{}, fmt.Errorf("Secret %s/%s has no key %s", store.namespace, reference.Name, reference.Key)
	}
	if !utf8.Valid(value) || bytes.IndexByte(value, 0) >= 0 {
		return apply.SecretValue{}, fmt.Errorf("Secret %s/%s key %s is not valid text", store.namespace, reference.Name, reference.Key)
	}
	if secret.ResourceVersion == "" {
		return apply.SecretValue{}, fmt.Errorf("Secret %s/%s has no resource version", store.namespace, reference.Name)
	}
	return apply.SecretValue{Value: string(value), Version: secret.ResourceVersion}, nil
}

func (store *KubernetesStore) LoadManifest(ctx context.Context) (render.Manifest, string, error) {
	configMap, err := store.configMaps.Get(ctx, store.manifestConfigMapName, metav1.GetOptions{})
	if err != nil {
		return render.Manifest{}, "", err
	}
	raw, exists := configMap.Data[ManifestDataKey]
	if !exists || len(raw) == 0 || len(raw) > render.MaxRenderedManifestBytes {
		return render.Manifest{}, configMap.ResourceVersion, ErrInvalidManifest
	}
	var manifest render.Manifest
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return render.Manifest{}, configMap.ResourceVersion, ErrInvalidManifest
	}
	return manifest, configMap.ResourceVersion, nil
}

func (store *KubernetesStore) ManifestResourceVersion(ctx context.Context) (string, error) {
	configMap, err := store.configMaps.Get(ctx, store.manifestConfigMapName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return configMap.ResourceVersion, nil
}

func (store *KubernetesStore) SecretResourceVersion(ctx context.Context, name string) (string, error) {
	if problems := validation.IsDNS1123Subdomain(name); len(problems) != 0 {
		return "", errors.New("Secret name is invalid")
	}
	secret, err := store.secrets.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return secret.ResourceVersion, nil
}

func (store *KubernetesStore) WatchManifest(ctx context.Context, resourceVersion string) (watch.Interface, error) {
	return store.configMaps.Watch(ctx, exactNameWatchOptions(store.manifestConfigMapName, resourceVersion))
}

func (store *KubernetesStore) WatchSecret(
	ctx context.Context,
	name string,
	resourceVersion string,
) (watch.Interface, error) {
	if problems := validation.IsDNS1123Subdomain(name); len(problems) != 0 {
		return nil, errors.New("Secret name is invalid")
	}
	return store.secrets.Watch(ctx, exactNameWatchOptions(name, resourceVersion))
}

func exactNameWatchOptions(name, resourceVersion string) metav1.ListOptions {
	return metav1.ListOptions{
		AllowWatchBookmarks: true,
		FieldSelector:       fields.OneTermEqualSelector("metadata.name", name).String(),
		ResourceVersion:     resourceVersion,
	}
}

func (store *KubernetesStore) WriteReport(ctx context.Context, report StatusReport) error {
	if err := report.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return errors.New("serialize sidecar report")
	}
	if len(raw) > MaxReportBytes {
		return errors.New("sidecar report exceeds its size limit")
	}
	patch, err := json.Marshal(map[string]any{
		"data": map[string]string{ReportDataKey: string(raw)},
	})
	if err != nil {
		return errors.New("serialize sidecar report patch")
	}
	_, err = store.configMaps.Patch(
		ctx,
		store.reportConfigMapName,
		types.MergePatchType,
		patch,
		metav1.PatchOptions{FieldManager: "t3-coded"},
	)
	if err != nil {
		return fmt.Errorf("patch sidecar report ConfigMap: %w", err)
	}
	return nil
}
