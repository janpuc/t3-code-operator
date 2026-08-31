package apply

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/janpuc/t3-code-operator/internal/render"
)

type SecretValue struct {
	Value   string
	Version string
}

type SecretResolver interface {
	Resolve(context.Context, render.SecretReference) (SecretValue, error)
}

type ActivityReader interface {
	ActiveInstances(context.Context, []string) ([]string, error)
}

type UpstreamClient interface {
	ApplyManagedSettings(context.Context, ManagedSettings) error
	ManagedSettingsMatch(context.Context, ManagedSettings) (bool, error)
}

type ExtensionManager interface {
	Stage(
		context.Context,
		[]render.ExtensionActivation,
		[]render.ExtensionActivation,
		map[render.SecretReference]SecretValue,
	) (ExtensionTransaction, error)
}

type ExtensionRecoveryManager interface {
	StageRecovery(
		context.Context,
		[]render.ExtensionActivation,
		[]render.ExtensionActivation,
	) (ExtensionTransaction, error)
}

type ExtensionTransaction interface {
	Commit() error
	Rollback() error
	Finalize() error
}

type ToolManager interface {
	Stage(context.Context, []render.ToolActivation, []render.ToolActivation) (ToolTransaction, error)
}

type ToolRecoveryManager interface {
	StageRecovery(context.Context, []render.ToolActivation, []render.ToolActivation) (ToolTransaction, error)
}

type ToolTransaction interface {
	Commit() error
	Rollback() error
	Finalize() error
}

type ProviderInstance struct {
	Driver      string                `json:"driver"`
	DisplayName string                `json:"displayName,omitempty"`
	AccentColor string                `json:"accentColor,omitempty"`
	Enabled     bool                  `json:"enabled"`
	Environment []ProviderEnvironment `json:"environment,omitempty"`
	Config      json.RawMessage       `json:"config,omitempty"`
}

type ManagedSettings struct {
	EnableProviderUpdateChecks bool                        `json:"enableProviderUpdateChecks"`
	ProviderInstances          map[string]ProviderInstance `json:"providerInstances"`
}

type ProviderEnvironment struct {
	Name      string `json:"name"`
	Value     string `json:"value,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
}

type Config struct {
	DataRoot               string
	WorkspaceRoot          string
	RepositoryScanDepth    int
	RepositoryScanInterval time.Duration
	Secrets                SecretResolver
	Upstream               UpstreamClient
	Activity               ActivityReader
	Extensions             ExtensionManager
	Tools                  ToolManager
}

type ApplyState string

const (
	ApplyStateProgrammed ApplyState = "Programmed"
	ApplyStateDeferred   ApplyState = "Deferred"
	ApplyStateFailed     ApplyState = "Failed"
)

type Report struct {
	DesiredRevision         string     `json:"desiredRevision,omitempty"`
	LiveRevision            string     `json:"liveRevision,omitempty"`
	MaterializationRevision string     `json:"materializationRevision,omitempty"`
	State                   ApplyState `json:"state"`
	Reason                  string     `json:"reason,omitempty"`
	FailedTools             []string   `json:"failedTools,omitempty"`
}

type ToolFailure struct {
	Names []string
	Err   error
}

func (failure *ToolFailure) Error() string {
	if failure.Err == nil {
		return "tool operation failed"
	}
	return failure.Err.Error()
}

func (failure *ToolFailure) Unwrap() error {
	return failure.Err
}

func FailedToolNames(err error) []string {
	var failure *ToolFailure
	if !errors.As(err, &failure) {
		return nil
	}
	result := append([]string(nil), failure.Names...)
	sort.Strings(result)
	return result
}
