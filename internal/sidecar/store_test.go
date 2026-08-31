package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/janpuc/t3-code-operator/internal/apply"
	"github.com/janpuc/t3-code-operator/internal/render"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestKubernetesStoreResolvesOneExactSameNamespaceSecret(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "provider-token", ResourceVersion: "17"},
		Data:       map[string][]byte{"token": []byte("secret-canary")},
	})
	store := newTestKubernetesStore(t, client)

	value, err := store.Resolve(context.Background(), render.SecretReference{
		Namespace: "agents",
		Name:      "provider-token",
		Key:       "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.Value != "secret-canary" || value.Version != "17" {
		t.Fatalf("unexpected Secret value metadata: %#v", value)
	}
	if len(client.Actions()) != 1 || client.Actions()[0].GetVerb() != "get" ||
		client.Actions()[0].GetResource().Resource != "secrets" {
		t.Fatalf("Secret resolver used an unexpected API operation: %#v", client.Actions())
	}

	before := len(client.Actions())
	_, err = store.Resolve(context.Background(), render.SecretReference{
		Namespace: "other",
		Name:      "provider-token",
		Key:       "token",
	})
	if err == nil || len(client.Actions()) != before {
		t.Fatalf("cross-namespace Secret reference reached the API: err=%v actions=%#v", err, client.Actions())
	}
}

func TestKubernetesStoreRejectsUnsafeSecretText(t *testing.T) {
	for name, value := range map[string][]byte{
		"invalid-utf8": {0xff},
		"nul":          []byte("before\x00after"),
	} {
		t.Run(name, func(t *testing.T) {
			client := fake.NewSimpleClientset(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "provider-token", ResourceVersion: "3"},
				Data:       map[string][]byte{"token": value},
			})
			store := newTestKubernetesStore(t, client)
			_, err := store.Resolve(context.Background(), render.SecretReference{
				Namespace: "agents",
				Name:      "provider-token",
				Key:       "token",
			})
			if err == nil || strings.Contains(err.Error(), string(value)) {
				t.Fatalf("unsafe Secret text was accepted or disclosed: %v", err)
			}
		})
	}
}

func TestKubernetesStoreLoadsBoundedManifest(t *testing.T) {
	manifest := sidecarTestManifest(t, "provider-token")
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "desired", ResourceVersion: "42"},
		Data:       map[string]string{ManifestDataKey: string(raw)},
	})
	store := newTestKubernetesStore(t, client)

	loaded, resourceVersion, err := store.LoadManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DesiredRevision != manifest.DesiredRevision || resourceVersion != "42" {
		t.Fatalf("unexpected manifest snapshot: revision=%q resourceVersion=%q", loaded.DesiredRevision, resourceVersion)
	}

	configMap, err := client.CoreV1().ConfigMaps("agents").Get(context.Background(), "desired", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	configMap.Data[ManifestDataKey] = strings.Repeat("x", render.MaxRenderedManifestBytes+1)
	if _, err := client.CoreV1().ConfigMaps("agents").Update(context.Background(), configMap, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.LoadManifest(context.Background())
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("oversized manifest was accepted: %v", err)
	}
}

func TestKubernetesStoreUsesExactNameWatchesWithoutLists(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := newTestKubernetesStore(t, client)
	manifestWatch, err := store.WatchManifest(context.Background(), "11")
	if err != nil {
		t.Fatal(err)
	}
	manifestWatch.Stop()
	secretWatch, err := store.WatchSecret(context.Background(), "provider-token", "12")
	if err != nil {
		t.Fatal(err)
	}
	secretWatch.Stop()

	watches := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" {
			t.Fatalf("sidecar issued a forbidden list operation: %#v", action)
		}
		watchAction, ok := action.(clienttesting.WatchAction)
		if !ok {
			continue
		}
		watches++
		restrictions := watchAction.GetWatchRestrictions()
		name, exact := restrictions.Fields.RequiresExactMatch("metadata.name")
		if !exact {
			t.Fatalf("watch has no exact metadata.name field selector: %#v", restrictions)
		}
		want := "desired"
		if action.GetResource().Resource == "secrets" {
			want = "provider-token"
		}
		if name != want {
			t.Fatalf("watch selected %q, want %q", name, want)
		}
	}
	if watches != 2 {
		t.Fatalf("expected two exact watches, got %d actions=%#v", watches, client.Actions())
	}
}

func TestKubernetesStorePatchesOnlyTheNamedReportKey(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "report"},
		Data:       map[string]string{"operator-owned": "preserve"},
	})
	store := newTestKubernetesStore(t, client)
	report := StatusReport{
		APIVersion:         ReportAPIVersion,
		Kind:               ReportKind,
		ProtocolVersion:    render.ProtocolVersion,
		T3Version:          UpstreamT3Version,
		State:              apply.ApplyStateFailed,
		Reason:             "SecretResolutionFailed",
		Activity:           ActivityStateIdle,
		ActivityObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := store.WriteReport(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	actions := client.Actions()
	if len(actions) != 1 {
		t.Fatalf("report writer used unexpected operations: %#v", actions)
	}
	patchAction, ok := actions[0].(clienttesting.PatchAction)
	if !ok || patchAction.GetName() != "report" || patchAction.GetResource().Resource != "configmaps" {
		t.Fatalf("report writer used an unexpected target: %#v", actions[0])
	}
	patch := string(patchAction.GetPatch())
	if !strings.Contains(patch, ReportDataKey) || strings.Contains(patch, "secret-canary") || strings.Contains(patch, "operator-owned") {
		t.Fatalf("report patch was unsafe or overbroad: %s", patch)
	}

	report.Reason = "invalid reason: secret-canary"
	if err := store.WriteReport(context.Background(), report); err == nil || len(client.Actions()) != 1 {
		t.Fatalf("unsafe report reached the API: err=%v actions=%#v", err, client.Actions())
	}
	report.Reason = "ToolStageFailed"
	report.FailedTools = []string{"invalid/tool"}
	if err := store.WriteReport(context.Background(), report); err == nil || len(client.Actions()) != 1 {
		t.Fatalf("unsafe tool name reached the API: err=%v actions=%#v", err, client.Actions())
	}
	report.FailedTools = nil
	report.PodRevision = "not-a-revision"
	if err := store.WriteReport(context.Background(), report); err == nil || len(client.Actions()) != 1 {
		t.Fatalf("unsafe Pod revision reached the API: err=%v actions=%#v", err, client.Actions())
	}
}

func newTestKubernetesStore(t *testing.T, client *fake.Clientset) *KubernetesStore {
	t.Helper()
	store, err := NewKubernetesStore(client.CoreV1(), StoreConfig{
		Namespace:             "agents",
		ManifestConfigMapName: "desired",
		ReportConfigMapName:   "report",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func sidecarTestManifest(t *testing.T, secretName string) render.Manifest {
	t.Helper()
	reference := render.SecretReference{Namespace: "agents", Name: secretName, Key: "token"}
	manifest, err := render.Render(render.ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Harnesses: []render.Harness{{
			InstanceID: "codex",
			Driver:     "codex",
			Enabled:    true,
			Environment: []render.EnvironmentVariable{{
				Name:      "OPENAI_API_KEY",
				ValueFrom: &reference,
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}
