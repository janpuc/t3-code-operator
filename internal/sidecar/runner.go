package sidecar

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/janpuc/t3-code-operator/internal/apply"
	"github.com/janpuc/t3-code-operator/internal/render"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	DefaultRetryInterval    = 5 * time.Second
	DefaultRefreshInterval  = 30 * time.Second
	DefaultActivityInterval = 5 * time.Second
)

type RuntimeStore interface {
	LoadManifest(context.Context) (render.Manifest, string, error)
	ManifestResourceVersion(context.Context) (string, error)
	SecretResourceVersion(context.Context, string) (string, error)
	WatchManifest(context.Context, string) (watch.Interface, error)
	WatchSecret(context.Context, string, string) (watch.Interface, error)
	WriteReport(context.Context, StatusReport) error
}

type ManifestApplier interface {
	Apply(context.Context, render.Manifest) (apply.Report, error)
	LiveManifest() (render.Manifest, bool, error)
	Refresh(context.Context) (apply.Report, bool, error)
}

type RunnerConfig struct {
	Store            RuntimeStore
	Applier          ManifestApplier
	Activity         apply.ActivityReader
	Workstation      render.WorkstationIdentity
	PodRevision      string
	T3Version        string
	RetryInterval    time.Duration
	RefreshInterval  time.Duration
	ActivityInterval time.Duration
	Probes           *ProbeState
}

type Runner struct {
	store            RuntimeStore
	applier          ManifestApplier
	activity         apply.ActivityReader
	workstation      render.WorkstationIdentity
	podRevision      string
	t3Version        string
	retryInterval    time.Duration
	refreshInterval  time.Duration
	activityInterval time.Duration
	now              func() time.Time
	probes           *ProbeState
	contractVerified bool

	liveMaterializationRevision string
	activityState               ActivityState
	activityObservedAt          string
}

type reconcileResult struct {
	report        StatusReport
	references    map[string]struct{}
	retryApply    bool
	reportPending bool
}

func NewRunner(config RunnerConfig) (*Runner, error) {
	if config.Store == nil || config.Applier == nil || config.Activity == nil {
		return nil, errors.New("sidecar store, applier, and activity reader are required")
	}
	if config.Workstation.Namespace == "" || config.Workstation.Name == "" || config.Workstation.UID == "" {
		return nil, errors.New("Workstation identity is required")
	}
	if !reportRevisionPattern.MatchString(config.PodRevision) {
		return nil, errors.New("Pod revision is required")
	}
	if config.T3Version == "" {
		return nil, errors.New("t3 version is required")
	}
	retryInterval := config.RetryInterval
	if retryInterval <= 0 {
		retryInterval = DefaultRetryInterval
	}
	refreshInterval := config.RefreshInterval
	if refreshInterval <= 0 {
		refreshInterval = DefaultRefreshInterval
	}
	activityInterval := config.ActivityInterval
	if activityInterval <= 0 {
		activityInterval = DefaultActivityInterval
	}
	return &Runner{
		store:            config.Store,
		applier:          config.Applier,
		activity:         config.Activity,
		workstation:      config.Workstation,
		podRevision:      config.PodRevision,
		t3Version:        config.T3Version,
		retryInterval:    retryInterval,
		refreshInterval:  refreshInterval,
		activityInterval: activityInterval,
		now:              time.Now,
		probes:           config.Probes,
		activityState:    ActivityStateUnknown,
	}, nil
}

func (runner *Runner) Run(ctx context.Context) error {
	runner.probes.SetReady(false)
	defer runner.probes.SetReady(false)
	watchContext, stopWatches := context.WithCancel(ctx)
	defer stopWatches()

	events := make(chan struct{}, 1)
	var watchers sync.WaitGroup
	watchers.Add(1)
	go func() {
		defer watchers.Done()
		runner.watchResource(
			watchContext,
			runner.store.ManifestResourceVersion,
			runner.store.WatchManifest,
			events,
		)
	}()

	secretWatches := make(map[string]context.CancelFunc)
	defer func() {
		for _, cancel := range secretWatches {
			cancel()
		}
		stopWatches()
		watchers.Wait()
	}()

	ticker := time.NewTicker(runner.retryInterval)
	defer ticker.Stop()
	refreshTicker := time.NewTicker(runner.refreshInterval)
	defer refreshTicker.Stop()
	activityTicker := time.NewTicker(runner.activityInterval)
	defer activityTicker.Stop()
	retryApply := true
	var pendingReport *StatusReport
	var lastReport *StatusReport

	runner.sampleActivity(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-events:
			result := runner.reconcile(ctx)
			runner.syncSecretWatches(watchContext, result.references, secretWatches, &watchers, events)
			retryApply = result.retryApply
			report := result.report
			lastReport = &report
			if result.reportPending {
				pendingReport = lastReport
			} else {
				pendingReport = nil
			}
		case <-ticker.C:
			if retryApply {
				signalEvent(events)
				continue
			}
			if pendingReport != nil && runner.store.WriteReport(ctx, *pendingReport) == nil {
				pendingReport = nil
			}
		case <-refreshTicker.C:
			if retryApply || lastReport == nil || runner.t3Version != UpstreamT3Version {
				continue
			}
			refreshed, needsApply, reportPending := runner.refresh(ctx, *lastReport)
			lastReport = &refreshed
			if reportPending {
				pendingReport = lastReport
			} else {
				pendingReport = nil
			}
			if needsApply {
				retryApply = true
				signalEvent(events)
			}
		case <-activityTicker.C:
			runner.sampleActivity(ctx)
			if lastReport == nil {
				continue
			}
			lastReport.Activity = runner.activityState
			lastReport.ActivityObservedAt = runner.activityObservedAt
			runner.updateReadiness(lastReport.LiveRevision)
			if runner.store.WriteReport(ctx, *lastReport) != nil {
				pendingReport = lastReport
			} else {
				pendingReport = nil
			}
		}
	}
}

func (runner *Runner) sampleActivity(ctx context.Context) {
	active, err := runner.activity.ActiveInstances(ctx, nil)
	runner.activityObservedAt = runner.now().UTC().Format(time.RFC3339Nano)
	switch {
	case err != nil:
		runner.activityState = ActivityStateUnknown
	case len(active) != 0:
		runner.contractVerified = true
		runner.activityState = ActivityStateActive
	default:
		runner.contractVerified = true
		runner.activityState = ActivityStateIdle
	}
}

func (runner *Runner) refresh(ctx context.Context, previous StatusReport) (StatusReport, bool, bool) {
	refreshed, needsApply, _ := runner.applier.Refresh(ctx)
	report := previous
	if refreshed.DesiredRevision != "" {
		report.DesiredRevision = refreshed.DesiredRevision
	}
	report.LiveRevision = refreshed.LiveRevision
	report.MaterializationRevision = refreshed.MaterializationRevision
	report.State = refreshed.State
	report.Reason = refreshed.Reason
	report.FailedTools = append([]string(nil), refreshed.FailedTools...)
	if report.MaterializationRevision != "" {
		runner.liveMaterializationRevision = report.MaterializationRevision
	}
	runner.updateReadiness(report.LiveRevision)
	return report, needsApply, runner.store.WriteReport(ctx, report) != nil
}

func (runner *Runner) reconcile(ctx context.Context) reconcileResult {
	if runner.activityObservedAt == "" {
		runner.sampleActivity(ctx)
	}
	report := StatusReport{
		APIVersion:              ReportAPIVersion,
		Kind:                    ReportKind,
		ProtocolVersion:         render.ProtocolVersion,
		T3Version:               runner.t3Version,
		PodRevision:             runner.podRevision,
		MaterializationRevision: runner.liveMaterializationRevision,
		State:                   apply.ApplyStateFailed,
		Activity:                runner.activityState,
		ActivityObservedAt:      runner.activityObservedAt,
	}
	live, liveExists, liveErr := runner.applier.LiveManifest()
	if liveErr == nil && liveExists {
		report.LiveRevision = live.DesiredRevision
	}
	runner.updateReadiness(report.LiveRevision)
	liveReferences := runner.secretNames(live, liveExists && liveErr == nil)
	if runner.t3Version != UpstreamT3Version {
		report.Reason = "UnsupportedT3Version"
		return runner.finishReconcile(ctx, report, liveReferences, false)
	}

	manifest, resourceVersion, err := runner.store.LoadManifest(ctx)
	report.ManifestResourceVersion = resourceVersion
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidManifest):
			report.Reason = "InvalidManifest"
		case apierrors.IsNotFound(err):
			report.Reason = "ManifestUnavailable"
		default:
			report.Reason = "ManifestReadFailed"
		}
		return runner.finishReconcile(ctx, report, liveReferences, true)
	}
	if err := render.VerifyManifest(manifest); err != nil {
		report.Reason = "InvalidManifest"
		return runner.finishReconcile(ctx, report, liveReferences, true)
	}
	report.DesiredRevision = manifest.DesiredRevision
	if manifest.Workstation != runner.workstation {
		report.Reason = "InvalidManifest"
		return runner.finishReconcile(ctx, report, liveReferences, true)
	}

	desiredReferences := runner.secretNames(manifest, true)
	applyReport, applyErr := runner.applier.Apply(ctx, manifest)
	report.DesiredRevision = applyReport.DesiredRevision
	report.LiveRevision = applyReport.LiveRevision
	report.MaterializationRevision = applyReport.MaterializationRevision
	report.State = applyReport.State
	report.Reason = applyReport.Reason
	report.FailedTools = append([]string(nil), applyReport.FailedTools...)
	if report.MaterializationRevision != "" {
		runner.liveMaterializationRevision = report.MaterializationRevision
	}
	if applyErr != nil && report.Reason == "" {
		report.State = apply.ApplyStateFailed
		report.Reason = "ApplyFailed"
	}

	latestLive, latestExists, latestErr := runner.applier.LiveManifest()
	if latestErr == nil && latestExists {
		report.LiveRevision = latestLive.DesiredRevision
		liveReferences = runner.secretNames(latestLive, true)
	}
	runner.updateReadiness(report.LiveRevision)
	if applyErr == nil && report.State == apply.ApplyStateProgrammed && report.LiveRevision == manifest.DesiredRevision {
		return runner.finishReconcile(ctx, report, desiredReferences, false)
	}
	return runner.finishReconcile(ctx, report, unionNames(liveReferences, desiredReferences), true)
}

func (runner *Runner) updateReadiness(liveRevision string) {
	runner.probes.SetReady(
		runner.t3Version == UpstreamT3Version && runner.contractVerified && liveRevision != "",
	)
}

func (runner *Runner) finishReconcile(
	ctx context.Context,
	report StatusReport,
	references map[string]struct{},
	retryApply bool,
) reconcileResult {
	return reconcileResult{
		report:        report,
		references:    references,
		retryApply:    retryApply,
		reportPending: runner.store.WriteReport(ctx, report) != nil,
	}
}

func (runner *Runner) secretNames(manifest render.Manifest, include bool) map[string]struct{} {
	result := make(map[string]struct{})
	if !include {
		return result
	}
	for _, reference := range apply.ReferencedSecrets(manifest) {
		if reference.Namespace == runner.workstation.Namespace && reference.Name != "" {
			result[reference.Name] = struct{}{}
		}
	}
	return result
}

func unionNames(groups ...map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{})
	for _, group := range groups {
		for name := range group {
			result[name] = struct{}{}
		}
	}
	return result
}

func (runner *Runner) syncSecretWatches(
	ctx context.Context,
	desired map[string]struct{},
	current map[string]context.CancelFunc,
	watchers *sync.WaitGroup,
	events chan<- struct{},
) {
	for name := range desired {
		if _, exists := current[name]; exists {
			continue
		}
		secretName := name
		secretContext, cancel := context.WithCancel(ctx)
		current[secretName] = cancel
		watchers.Add(1)
		go func() {
			defer watchers.Done()
			runner.watchResource(
				secretContext,
				func(ctx context.Context) (string, error) {
					return runner.store.SecretResourceVersion(ctx, secretName)
				},
				func(ctx context.Context, resourceVersion string) (watch.Interface, error) {
					return runner.store.WatchSecret(ctx, secretName, resourceVersion)
				},
				events,
			)
		}()
	}
	for name, cancel := range current {
		if _, exists := desired[name]; exists {
			continue
		}
		cancel()
		delete(current, name)
	}
}

type resourceVersionReader func(context.Context) (string, error)
type resourceWatch func(context.Context, string) (watch.Interface, error)

func (runner *Runner) watchResource(
	ctx context.Context,
	readVersion resourceVersionReader,
	openWatch resourceWatch,
	events chan<- struct{},
) {
	lastMarker := ""
	initialized := false
	for {
		resourceVersion, err := readVersion(ctx)
		if ctx.Err() != nil {
			return
		}
		marker := "present:" + resourceVersion
		if err != nil {
			if !apierrors.IsNotFound(err) {
				if !initialized {
					signalEvent(events)
					initialized = true
				}
				if !waitForRetry(ctx, runner.retryInterval) {
					return
				}
				continue
			}
			resourceVersion = ""
			marker = "absent"
		}
		if !initialized || marker != lastMarker {
			signalEvent(events)
		}
		initialized = true
		lastMarker = marker

		stream, err := openWatch(ctx, resourceVersion)
		if err != nil {
			if !waitForRetry(ctx, runner.retryInterval) {
				return
			}
			continue
		}
		closed := runner.consumeWatch(ctx, stream, &lastMarker, events)
		stream.Stop()
		if !closed {
			return
		}
		if !waitForRetry(ctx, runner.retryInterval) {
			return
		}
	}
}

func (runner *Runner) consumeWatch(
	ctx context.Context,
	stream watch.Interface,
	lastMarker *string,
	events chan<- struct{},
) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case event, open := <-stream.ResultChan():
			if !open {
				return true
			}
			if event.Type == watch.Error {
				return true
			}
			accessor, err := meta.Accessor(event.Object)
			if err == nil {
				if event.Type == watch.Deleted {
					*lastMarker = "absent"
				} else {
					*lastMarker = "present:" + accessor.GetResourceVersion()
				}
			}
			if event.Type != watch.Bookmark {
				signalEvent(events)
			}
		}
	}
}

func signalEvent(events chan<- struct{}) {
	select {
	case events <- struct{}{}:
	default:
	}
}

func waitForRetry(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
