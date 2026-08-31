package controller

import (
	"time"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/render"
	"github.com/janpuc/t3-code-operator/internal/sidecar"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultDrainTimeout      = 30 * time.Minute
	defaultActivityFreshness = 15 * time.Second
	maximumClockSkew         = 5 * time.Second
)

type drainDecision struct {
	Permit    bool
	Forced    bool
	TimedOut  bool
	Reason    string
	StartedAt *metav1.Time
}

func evaluateDrain(
	report *sidecar.StatusReport,
	now time.Time,
	startedAt *metav1.Time,
	policy *t3v1alpha1.DrainPolicy,
	freshness time.Duration,
) drainDecision {
	now = now.UTC()
	if freshness <= 0 {
		freshness = defaultActivityFreshness
	}
	if reportIsFreshAndIdle(report, now, freshness) {
		return drainDecision{Permit: true, Reason: "StableIdle"}
	}
	decision := drainDecision{Reason: drainWaitReason(report, now, freshness)}
	if startedAt == nil {
		started := metav1.NewTime(now)
		decision.StartedAt = &started
		return decision
	}
	started := startedAt.DeepCopy()
	decision.StartedAt = started
	timeout, action := normalizedDrainPolicy(policy)
	if now.Sub(started.Time) < timeout {
		return decision
	}
	decision.TimedOut = true
	decision.Reason = "DrainTimedOut"
	if action == t3v1alpha1.DrainTimeoutForce {
		decision.Permit = true
		decision.Forced = true
		decision.Reason = "ContinuityWaived"
	}
	return decision
}

func reportIsFreshAndIdle(report *sidecar.StatusReport, now time.Time, freshness time.Duration) bool {
	if report == nil || report.Validate() != nil || report.ProtocolVersion != render.ProtocolVersion ||
		report.LiveRevision == "" ||
		report.Activity != sidecar.ActivityStateIdle {
		return false
	}
	observedAt, err := time.Parse(time.RFC3339Nano, report.ActivityObservedAt)
	if err != nil || observedAt.After(now.Add(maximumClockSkew)) {
		return false
	}
	return now.Sub(observedAt) <= freshness
}

func drainWaitReason(report *sidecar.StatusReport, now time.Time, freshness time.Duration) string {
	if report == nil || report.Validate() != nil || report.ProtocolVersion != render.ProtocolVersion ||
		report.LiveRevision == "" {
		return "ActivityUnavailable"
	}
	observedAt, err := time.Parse(time.RFC3339Nano, report.ActivityObservedAt)
	if err != nil || observedAt.After(now.Add(maximumClockSkew)) || now.Sub(observedAt) > freshness {
		return "ActivityStale"
	}
	if report.Activity == sidecar.ActivityStateActive {
		return "ActiveWork"
	}
	return "ActivityUnknown"
}

func normalizedDrainPolicy(policy *t3v1alpha1.DrainPolicy) (time.Duration, t3v1alpha1.DrainTimeoutAction) {
	timeout := defaultDrainTimeout
	action := t3v1alpha1.DrainTimeoutBlock
	if policy == nil {
		return timeout, action
	}
	if policy.Timeout != nil && policy.Timeout.Duration > 0 {
		timeout = policy.Timeout.Duration
	}
	if policy.TimeoutAction != "" {
		action = policy.TimeoutAction
	}
	return timeout, action
}
