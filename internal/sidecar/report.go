package sidecar

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/janpuc/t3-code-operator/internal/apply"
)

type ActivityState string

const (
	ActivityStateUnknown ActivityState = "Unknown"
	ActivityStateActive  ActivityState = "Active"
	ActivityStateIdle    ActivityState = "Idle"
)

const (
	ReportAPIVersion = "t3code.janpuc.com/sidecar-report/v1alpha1"
	ReportKind       = "T3CodedReport"
	ReportDataKey    = "report.json"
	MaxReportBytes   = 64 * 1024
)

var UpstreamT3Version = "0.0.34"

var (
	reportReasonPattern   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,127}$`)
	reportToolPattern     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
	reportRevisionPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type StatusReport struct {
	APIVersion              string           `json:"apiVersion"`
	Kind                    string           `json:"kind"`
	ProtocolVersion         string           `json:"protocolVersion"`
	T3Version               string           `json:"t3Version"`
	PodRevision             string           `json:"podRevision,omitempty"`
	ManifestResourceVersion string           `json:"manifestResourceVersion,omitempty"`
	DesiredRevision         string           `json:"desiredRevision,omitempty"`
	LiveRevision            string           `json:"liveRevision,omitempty"`
	MaterializationRevision string           `json:"materializationRevision,omitempty"`
	State                   apply.ApplyState `json:"state"`
	Reason                  string           `json:"reason,omitempty"`
	FailedTools             []string         `json:"failedTools,omitempty"`
	Activity                ActivityState    `json:"activity"`
	ActivityObservedAt      string           `json:"activityObservedAt"`
}

func (report StatusReport) Validate() error {
	if report.APIVersion != ReportAPIVersion || report.Kind != ReportKind {
		return errors.New("sidecar report identity is invalid")
	}
	if report.ProtocolVersion == "" || report.T3Version == "" {
		return errors.New("sidecar report versions are required")
	}
	switch report.State {
	case apply.ApplyStateProgrammed, apply.ApplyStateDeferred, apply.ApplyStateFailed:
	default:
		return errors.New("sidecar report state is invalid")
	}
	switch report.Activity {
	case ActivityStateUnknown, ActivityStateActive, ActivityStateIdle:
	default:
		return errors.New("sidecar report activity is invalid")
	}
	observedAt, err := time.Parse(time.RFC3339Nano, report.ActivityObservedAt)
	if err != nil || observedAt.Location() != time.UTC {
		return errors.New("sidecar report activity time is invalid")
	}
	if report.Reason != "" && !reportReasonPattern.MatchString(report.Reason) {
		return errors.New("sidecar report reason is invalid")
	}
	for _, revision := range []string{
		report.PodRevision,
		report.DesiredRevision,
		report.LiveRevision,
		report.MaterializationRevision,
	} {
		if revision != "" && !reportRevisionPattern.MatchString(revision) {
			return errors.New("sidecar report revision is invalid")
		}
	}
	if len(report.FailedTools) > 64 {
		return errors.New("sidecar report has too many failed tools")
	}
	seenTools := make(map[string]struct{}, len(report.FailedTools))
	for _, name := range report.FailedTools {
		if !reportToolPattern.MatchString(name) {
			return errors.New("sidecar report has an invalid failed tool")
		}
		if _, exists := seenTools[name]; exists {
			return errors.New("sidecar report has duplicate failed tools")
		}
		seenTools[name] = struct{}{}
	}
	for _, value := range []string{
		report.ProtocolVersion,
		report.T3Version,
		report.PodRevision,
		report.ManifestResourceVersion,
		report.DesiredRevision,
		report.LiveRevision,
		report.MaterializationRevision,
		report.ActivityObservedAt,
	} {
		if len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("sidecar report field is invalid")
		}
	}
	return nil
}
