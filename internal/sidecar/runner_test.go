package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janpuc/t3-code-operator/internal/apply"
	"github.com/janpuc/t3-code-operator/internal/render"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestRunnerReportsAProgrammedRevisionWithoutErrorText(t *testing.T) {
	manifest := sidecarTestManifest(t, "provider-token")
	store := newFakeRuntimeStore(manifest)
	applier := &fakeManifestApplier{
		result: apply.Report{
			DesiredRevision:         manifest.DesiredRevision,
			LiveRevision:            manifest.DesiredRevision,
			MaterializationRevision: "sha256:" + repeatHex("1"),
			State:                   apply.ApplyStateProgrammed,
		},
	}
	runner := newTestRunner(t, store, applier, time.Second)
	runner.probes = &ProbeState{}

	result := runner.reconcile(context.Background())
	if result.retryApply || result.reportPending {
		t.Fatalf("programmed revision requested a retry: %#v", result)
	}
	if result.report.ProtocolVersion != render.ProtocolVersion ||
		result.report.T3Version != UpstreamT3Version ||
		result.report.PodRevision != "sha256:"+repeatHex("f") ||
		result.report.LiveRevision != manifest.DesiredRevision ||
		result.report.Activity != ActivityStateIdle ||
		result.report.ActivityObservedAt == "" {
		t.Fatalf("unexpected sidecar report: %#v", result.report)
	}
	if _, exists := result.references["provider-token"]; !exists || len(result.references) != 1 {
		t.Fatalf("unexpected Secret watch set: %#v", result.references)
	}
	if applier.calls != 1 || len(store.reports) != 1 {
		t.Fatalf("unexpected reconcile calls: apply=%d reports=%d", applier.calls, len(store.reports))
	}
	if !runner.probes.Ready() {
		t.Fatal("a programmed revision with an authenticated activity snapshot is not ready")
	}
}

func TestRunnerReportsUnknownAndActiveActivity(t *testing.T) {
	manifest := sidecarTestManifest(t, "provider-token")
	runner := newTestRunner(t, newFakeRuntimeStore(manifest), &fakeManifestApplier{}, time.Second)
	runner.activity = &fakeActivityReader{err: errors.New("unavailable")}
	runner.sampleActivity(context.Background())
	if runner.activityState != ActivityStateUnknown {
		t.Fatalf("read failure was not unknown: %s", runner.activityState)
	}
	runner.activity = &fakeActivityReader{active: []string{"codex"}}
	runner.sampleActivity(context.Background())
	if runner.activityState != ActivityStateActive {
		t.Fatalf("active work was not reported: %s", runner.activityState)
	}
}

func TestRunnerReportsFailedToolWithoutErrorText(t *testing.T) {
	manifest := sidecarTestManifest(t, "provider-token")
	store := newFakeRuntimeStore(manifest)
	applier := &fakeManifestApplier{
		result: apply.Report{
			DesiredRevision: manifest.DesiredRevision,
			State:           apply.ApplyStateFailed,
			Reason:          "ToolStageFailed",
			FailedTools:     []string{"kubectl"},
		},
		err: errors.New("secret-canary"),
	}
	runner := newTestRunner(t, store, applier, time.Second)
	result := runner.reconcile(context.Background())
	raw, err := json.Marshal(result.report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-canary") || len(result.report.FailedTools) != 1 || result.report.FailedTools[0] != "kubectl" {
		t.Fatalf("unsafe or incomplete tool failure report: %s", raw)
	}
}

func TestRunnerKeepsLiveAndDesiredSecretWatchesWhileDeferred(t *testing.T) {
	live := sidecarTestManifest(t, "old-token")
	desired := sidecarTestManifest(t, "new-token")
	store := newFakeRuntimeStore(desired)
	applier := &fakeManifestApplier{
		live: &live,
		result: apply.Report{
			DesiredRevision: desired.DesiredRevision,
			LiveRevision:    live.DesiredRevision,
			State:           apply.ApplyStateDeferred,
			Reason:          "ActiveWork",
		},
	}
	runner := newTestRunner(t, store, applier, time.Second)

	result := runner.reconcile(context.Background())
	if !result.retryApply || result.report.State != apply.ApplyStateDeferred {
		t.Fatalf("deferred revision did not request a retry: %#v", result)
	}
	for _, name := range []string{"old-token", "new-token"} {
		if _, exists := result.references[name]; !exists {
			t.Fatalf("deferred revision dropped Secret %q: %#v", name, result.references)
		}
	}
	if len(result.references) != 2 {
		t.Fatalf("unexpected deferred Secret watch set: %#v", result.references)
	}
}

func TestRunnerDoesNotEchoInvalidManifestOrErrorContent(t *testing.T) {
	for name, configure := range map[string]func(*fakeRuntimeStore){
		"invalid-manifest": func(store *fakeRuntimeStore) {
			store.manifest = render.Manifest{DesiredRevision: "secret-canary"}
		},
		"read-error": func(store *fakeRuntimeStore) {
			store.loadErr = errors.New("secret-canary")
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := newFakeRuntimeStore(render.Manifest{})
			configure(store)
			runner := newTestRunner(t, store, &fakeManifestApplier{}, time.Second)
			result := runner.reconcile(context.Background())
			raw, err := json.Marshal(result.report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "secret-canary") || result.report.State != apply.ApplyStateFailed {
				t.Fatalf("unsafe failure report: %s", raw)
			}
		})
	}
}

func TestRunnerReportsAndBlocksAnUnsupportedT3Version(t *testing.T) {
	manifest := sidecarTestManifest(t, "provider-token")
	store := newFakeRuntimeStore(manifest)
	applier := &fakeManifestApplier{}
	runner := newTestRunner(t, store, applier, time.Second)
	runner.t3Version = "0.0.33"

	result := runner.reconcile(context.Background())
	if result.retryApply || result.report.Reason != "UnsupportedT3Version" || applier.calls != 0 {
		t.Fatalf("unsupported t3 version was not blocked: %#v applyCalls=%d", result, applier.calls)
	}
}

func TestRunnerRefreshReportsDriftAndRequestsAnApply(t *testing.T) {
	manifest := sidecarTestManifest(t, "provider-token")
	store := newFakeRuntimeStore(manifest)
	applier := &fakeManifestApplier{
		refreshResult: apply.Report{
			DesiredRevision:         manifest.DesiredRevision,
			LiveRevision:            manifest.DesiredRevision,
			MaterializationRevision: "sha256:" + repeatHex("3"),
			State:                   apply.ApplyStateFailed,
			Reason:                  "DriftDetected",
		},
		refreshNeedsApply: true,
	}
	runner := newTestRunner(t, store, applier, time.Second)
	previous := StatusReport{
		APIVersion:         ReportAPIVersion,
		Kind:               ReportKind,
		ProtocolVersion:    render.ProtocolVersion,
		T3Version:          UpstreamT3Version,
		State:              apply.ApplyStateProgrammed,
		Activity:           ActivityStateIdle,
		ActivityObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}

	report, needsApply, pending := runner.refresh(context.Background(), previous)
	if !needsApply || pending || report.Reason != "DriftDetected" || report.State != apply.ApplyStateFailed {
		t.Fatalf("unexpected drift refresh: report=%#v needsApply=%v pending=%v", report, needsApply, pending)
	}
}

func TestRunnerRetriesReportOnlyAndReappliesOnSecretEvent(t *testing.T) {
	manifest := sidecarTestManifest(t, "provider-token")
	store := newFakeRuntimeStore(manifest)
	store.reportFailures = 2
	applier := &fakeManifestApplier{
		result: apply.Report{
			DesiredRevision:         manifest.DesiredRevision,
			LiveRevision:            manifest.DesiredRevision,
			MaterializationRevision: "sha256:" + repeatHex("2"),
			State:                   apply.ApplyStateProgrammed,
		},
	}
	runner := newTestRunner(t, store, applier, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	eventually(t, func() bool {
		return applier.callCount() >= 1 && store.reportCallCount() >= 3 && store.secretStream() != nil
	})
	before := applier.callCount()
	time.Sleep(40 * time.Millisecond)
	if got := applier.callCount(); got != before {
		cancel()
		<-done
		t.Fatalf("report retry reapplied a programmed revision: before=%d after=%d", before, got)
	}

	store.secretStream().Modify(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "provider-token", ResourceVersion: "2"},
	})
	eventually(t, func() bool { return applier.callCount() > before })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type fakeRuntimeStore struct {
	mutex sync.Mutex

	manifest        render.Manifest
	manifestVersion string
	loadErr         error
	reports         []StatusReport
	reportFailures  int
	reportCalls     int
	manifestWatcher *watch.RaceFreeFakeWatcher
	secretWatcher   *watch.RaceFreeFakeWatcher
}

func newFakeRuntimeStore(manifest render.Manifest) *fakeRuntimeStore {
	return &fakeRuntimeStore{manifest: manifest, manifestVersion: "1"}
}

func (store *fakeRuntimeStore) LoadManifest(context.Context) (render.Manifest, string, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	return store.manifest, store.manifestVersion, store.loadErr
}

func (store *fakeRuntimeStore) ManifestResourceVersion(context.Context) (string, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	return store.manifestVersion, nil
}

func (store *fakeRuntimeStore) SecretResourceVersion(context.Context, string) (string, error) {
	return "1", nil
}

func (store *fakeRuntimeStore) WatchManifest(context.Context, string) (watch.Interface, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.manifestWatcher = watch.NewRaceFreeFake()
	return store.manifestWatcher, nil
}

func (store *fakeRuntimeStore) WatchSecret(context.Context, string, string) (watch.Interface, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.secretWatcher = watch.NewRaceFreeFake()
	return store.secretWatcher, nil
}

func (store *fakeRuntimeStore) WriteReport(_ context.Context, report StatusReport) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.reportCalls++
	if store.reportCalls <= store.reportFailures {
		return errors.New("injected report failure")
	}
	store.reports = append(store.reports, report)
	return nil
}

func (store *fakeRuntimeStore) reportCallCount() int {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	return store.reportCalls
}

func (store *fakeRuntimeStore) secretStream() *watch.RaceFreeFakeWatcher {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	return store.secretWatcher
}

type fakeManifestApplier struct {
	mutex sync.Mutex

	live              *render.Manifest
	result            apply.Report
	err               error
	calls             int
	refreshResult     apply.Report
	refreshNeedsApply bool
	refreshErr        error
}

func (applier *fakeManifestApplier) Apply(_ context.Context, manifest render.Manifest) (apply.Report, error) {
	applier.mutex.Lock()
	defer applier.mutex.Unlock()
	applier.calls++
	if applier.result.State == apply.ApplyStateProgrammed && applier.err == nil {
		copy := manifest
		applier.live = &copy
	}
	return applier.result, applier.err
}

func (applier *fakeManifestApplier) LiveManifest() (render.Manifest, bool, error) {
	applier.mutex.Lock()
	defer applier.mutex.Unlock()
	if applier.live == nil {
		return render.Manifest{}, false, nil
	}
	return *applier.live, true, nil
}

func (applier *fakeManifestApplier) Refresh(context.Context) (apply.Report, bool, error) {
	applier.mutex.Lock()
	defer applier.mutex.Unlock()
	return applier.refreshResult, applier.refreshNeedsApply, applier.refreshErr
}

func (applier *fakeManifestApplier) callCount() int {
	applier.mutex.Lock()
	defer applier.mutex.Unlock()
	return applier.calls
}

func newTestRunner(
	t *testing.T,
	store RuntimeStore,
	applier ManifestApplier,
	retryInterval time.Duration,
) *Runner {
	t.Helper()
	runner, err := NewRunner(RunnerConfig{
		Store:    store,
		Applier:  applier,
		Activity: &fakeActivityReader{},
		Workstation: render.WorkstationIdentity{
			Namespace: "agents",
			Name:      "primary",
			UID:       "workstation-uid",
		},
		PodRevision:      "sha256:" + repeatHex("f"),
		T3Version:        UpstreamT3Version,
		RetryInterval:    retryInterval,
		RefreshInterval:  time.Hour,
		ActivityInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

type fakeActivityReader struct {
	active []string
	err    error
}

func (reader *fakeActivityReader) ActiveInstances(context.Context, []string) ([]string, error) {
	return append([]string(nil), reader.active...), reader.err
}

func eventually(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func repeatHex(value string) string {
	return strings.Repeat(value, 64/len(value)+1)[:64]
}
