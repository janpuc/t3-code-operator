package controller

import (
	"testing"
	"time"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
	"github.com/janpuc/t3-code-operator/internal/apply"
	"github.com/janpuc/t3-code-operator/internal/render"
	"github.com/janpuc/t3-code-operator/internal/sidecar"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDrainRequiresAFreshStableIdleReport(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	report := controllerTestReport(now, sidecar.ActivityStateIdle)
	if decision := evaluateDrain(&report, now, nil, nil, 15*time.Second); !decision.Permit || decision.Forced {
		t.Fatalf("fresh idle did not permit drain: %#v", decision)
	}

	report.Activity = sidecar.ActivityStateActive
	decision := evaluateDrain(&report, now, nil, nil, 15*time.Second)
	if decision.Permit || decision.Reason != "ActiveWork" || decision.StartedAt == nil {
		t.Fatalf("active work did not start a blocked drain: %#v", decision)
	}

	report = controllerTestReport(now.Add(-time.Minute), sidecar.ActivityStateIdle)
	decision = evaluateDrain(&report, now, nil, nil, 15*time.Second)
	if decision.Permit || decision.Reason != "ActivityStale" {
		t.Fatalf("stale idle permitted drain: %#v", decision)
	}
}

func TestDrainTimeoutBlocksUnlessForceIsExplicit(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	started := metav1.NewTime(now.Add(-time.Minute))
	report := controllerTestReport(now, sidecar.ActivityStateActive)
	timeout := metav1.Duration{Duration: 30 * time.Second}

	blocked := evaluateDrain(&report, now, &started, &t3v1alpha1.DrainPolicy{
		Timeout:       &timeout,
		TimeoutAction: t3v1alpha1.DrainTimeoutBlock,
	}, 15*time.Second)
	if blocked.Permit || !blocked.TimedOut || blocked.Reason != "DrainTimedOut" {
		t.Fatalf("default timeout did not block: %#v", blocked)
	}

	forced := evaluateDrain(&report, now, &started, &t3v1alpha1.DrainPolicy{
		Timeout:       &timeout,
		TimeoutAction: t3v1alpha1.DrainTimeoutForce,
	}, 15*time.Second)
	if !forced.Permit || !forced.Forced || !forced.TimedOut || forced.Reason != "ContinuityWaived" {
		t.Fatalf("explicit force did not permit drain: %#v", forced)
	}
}

func TestDrainAcceptsAContractCompatibleNightlySidecar(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	report := controllerTestReport(now, sidecar.ActivityStateIdle)
	report.T3Version = "0.0.36-nightly.20260828.1209"
	decision := evaluateDrain(&report, now, nil, nil, 15*time.Second)
	if !decision.Permit || decision.Reason != "StableIdle" {
		t.Fatalf("contract-compatible nightly report did not permit drain: %#v", decision)
	}
}

func TestDrainRejectsAReportFromAnotherManagedPodRevision(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	report := controllerTestReport(now, sidecar.ActivityStateIdle)
	report.PodRevision = "sha256:" + repeatControllerHex("a")
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			podRevisionAnnotation: "sha256:" + repeatControllerHex("b"),
		}},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{podRevisionAnnotation: "sha256:" + repeatControllerHex("b")},
		}}},
	}
	decision := evaluateDrain(reportForDeployment(&report, deployment), now, nil, nil, 15*time.Second)
	if decision.Permit || decision.Reason != "ActivityUnavailable" {
		t.Fatalf("another Pod's report permitted drain: %#v", decision)
	}
}

func TestDrainPermitsAFreshIdleReportBeforeAnyLiveRevision(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	report := controllerTestReport(now.Add(-5*time.Second), sidecar.ActivityStateIdle)
	report.LiveRevision = ""
	report.State = apply.ApplyStateFailed
	report.Reason = "ExtensionCommitFailed"
	decision := evaluateDrain(&report, now, nil, nil, 15*time.Second)
	if !decision.Permit || decision.Reason != "StableIdle" {
		t.Fatalf("a never-applied sidecar cannot receive its fix: %#v", decision)
	}
}

func controllerTestReport(observedAt time.Time, activity sidecar.ActivityState) sidecar.StatusReport {
	return sidecar.StatusReport{
		APIVersion:         sidecar.ReportAPIVersion,
		Kind:               sidecar.ReportKind,
		ProtocolVersion:    render.ProtocolVersion,
		T3Version:          sidecar.UpstreamT3Version,
		LiveRevision:       "sha256:" + repeatControllerHex("a"),
		State:              apply.ApplyStateProgrammed,
		Activity:           activity,
		ActivityObservedAt: observedAt.UTC().Format(time.RFC3339Nano),
	}
}

func repeatControllerHex(value string) string {
	result := ""
	for len(result) < 64 {
		result += value
	}
	return result[:64]
}
