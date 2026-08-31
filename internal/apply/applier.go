package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/janpuc/t3-code-operator/internal/render"
)

type Applier struct {
	dataRoot               string
	workspaceRoot          string
	repositoryScanDepth    int
	repositoryScanInterval time.Duration
	secrets                SecretResolver
	upstream               UpstreamClient
	activity               ActivityReader
	extensions             ExtensionManager
	tools                  ToolManager
	now                    func() time.Time
	lastRepositoryScan     time.Time
	safeDirectories        []string

	mutex                       sync.Mutex
	liveManifest                *render.Manifest
	liveSettings                *ManagedSettings
	liveMaterializationRevision string
	pendingTransaction          *pendingTransaction
}

type persistedState struct {
	Manifest                render.Manifest `json:"manifest"`
	MaterializationRevision string          `json:"materializationRevision"`
}

func New(config Config) (*Applier, error) {
	if config.DataRoot == "" || !filepath.IsAbs(config.DataRoot) {
		return nil, errors.New("data root must be an absolute path")
	}
	if config.Secrets == nil || config.Upstream == nil || config.Activity == nil {
		return nil, errors.New("Secret resolver, upstream client, and activity reader are required")
	}
	dataRoot := filepath.Clean(config.DataRoot)
	workspaceRoot := config.WorkspaceRoot
	if workspaceRoot == "" {
		workspaceRoot = "/workspace"
	}
	if !filepath.IsAbs(workspaceRoot) {
		return nil, errors.New("workspace root must be an absolute path")
	}
	repositoryScanDepth := config.RepositoryScanDepth
	if repositoryScanDepth <= 0 {
		repositoryScanDepth = DefaultRepositoryScanDepth
	}
	repositoryScanInterval := config.RepositoryScanInterval
	if repositoryScanInterval <= 0 {
		repositoryScanInterval = DefaultRepositoryScanInterval
	}
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create data root: %w", err)
	}
	applier := &Applier{
		dataRoot:               dataRoot,
		workspaceRoot:          filepath.Clean(workspaceRoot),
		repositoryScanDepth:    repositoryScanDepth,
		repositoryScanInterval: repositoryScanInterval,
		secrets:                config.Secrets,
		upstream:               config.Upstream,
		activity:               config.Activity,
		extensions:             config.Extensions,
		tools:                  config.Tools,
		now:                    time.Now,
	}
	if err := applier.loadState(); err != nil {
		return nil, err
	}
	if err := applier.loadPendingTransaction(); err != nil {
		return nil, err
	}
	return applier, nil
}

func (applier *Applier) Apply(ctx context.Context, manifest render.Manifest) (Report, error) {
	applier.mutex.Lock()
	defer applier.mutex.Unlock()

	report := Report{
		DesiredRevision:         manifest.DesiredRevision,
		LiveRevision:            applier.liveRevision(),
		MaterializationRevision: applier.liveMaterializationRevision,
		State:                   ApplyStateFailed,
	}
	if err := applier.cleanupCommittedJournal(); err != nil {
		report.Reason = "CommitCleanupFailed"
		return report, err
	}
	deferred, err := applier.recoverPending(ctx)
	if err != nil {
		report.Reason = "RecoveryFailed"
		return report, err
	}
	if deferred {
		report.State = ApplyStateDeferred
		report.Reason = "RecoveryActiveWork"
		return report, nil
	}
	if err := render.VerifyManifest(manifest); err != nil {
		report.Reason = "InvalidManifest"
		return report, err
	}
	manifest, err = cloneAppliedManifest(manifest)
	if err != nil {
		report.Reason = "InvalidManifest"
		return report, fmt.Errorf("clone desired manifest: %w", err)
	}
	materialized, err := materializeManifest(ctx, applier.secrets, manifest)
	if err != nil {
		report.Reason = "SecretResolutionFailed"
		return report, err
	}
	preparedFiles, err := applier.prepareFileTargets(ctx, materialized.files)
	if err != nil {
		report.Reason = "FileStageFailed"
		return report, err
	}
	files, err := applier.stageFiles(manifest, preparedFiles)
	if err != nil {
		report.Reason = "FileStageFailed"
		return report, err
	}
	tools, err := applier.stageTools(ctx, manifest)
	if err != nil {
		report.Reason = "ToolStageFailed"
		report.FailedTools = FailedToolNames(err)
		return report, err
	}
	extensions, err := applier.stageExtensions(ctx, manifest, materialized.secrets)
	if err != nil {
		toolRollbackErr := tools.Rollback()
		report.Reason = "ExtensionStageFailed"
		return report, errors.Join(err, joinError("clean staged tools", toolRollbackErr))
	}

	instanceIDs := affectedInstanceIDs(manifest, applier.liveManifest)
	active, err := applier.activity.ActiveInstances(ctx, instanceIDs)
	if err != nil {
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		report.Reason = "ActivityReadFailed"
		return report, errors.Join(
			fmt.Errorf("read upstream activity: %w", err),
			joinError("clean staged Extensions", extensionRollbackErr),
			joinError("clean staged tools", toolRollbackErr),
		)
	}
	if len(active) != 0 {
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		if extensionRollbackErr != nil || toolRollbackErr != nil {
			report.Reason = "StageRollbackFailed"
			return report, errors.Join(extensionRollbackErr, toolRollbackErr)
		}
		report.State = ApplyStateDeferred
		report.Reason = "ActiveWork"
		return report, nil
	}

	previousSettings := ManagedSettings{ProviderInstances: map[string]ProviderInstance{}}
	if applier.liveSettings != nil {
		previousSettings = cloneManagedSettings(*applier.liveSettings)
	} else if applier.liveManifest != nil {
		previous, previousErr := materializeManifest(ctx, applier.secrets, *applier.liveManifest)
		if previousErr != nil {
			extensionRollbackErr := extensions.Rollback()
			toolRollbackErr := tools.Rollback()
			report.Reason = "RollbackStageFailed"
			return report, errors.Join(
				fmt.Errorf("stage last-known-good settings: %w", previousErr),
				joinError("clean staged Extensions", extensionRollbackErr),
				joinError("clean staged tools", toolRollbackErr),
			)
		}
		previousSettings = previous.settings
	}
	journal, err := applier.prepareJournal(manifest, files)
	if err != nil {
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		report.Reason = "JournalPrepareFailed"
		return report, errors.Join(
			fmt.Errorf("prepare apply journal: %w", err),
			joinError("clean staged Extensions", extensionRollbackErr),
			joinError("clean staged tools", toolRollbackErr),
		)
	}
	if err := files.Commit(); err != nil {
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		cleanupErr := journal.cleanup()
		report.Reason = "FileCommitFailed"
		return report, errors.Join(
			err,
			joinError("clean staged Extensions", extensionRollbackErr),
			joinError("clean staged tools", toolRollbackErr),
			joinError("clean failed transaction journal", cleanupErr),
		)
	}
	if err := journal.setPhase(transactionFilesCommitted); err != nil {
		fileRollbackErr := files.Rollback()
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		var cleanupErr error
		if fileRollbackErr == nil && extensionRollbackErr == nil && toolRollbackErr == nil {
			cleanupErr = journal.cleanup()
		}
		report.Reason = "JournalCommitFailed"
		return report, errors.Join(
			fmt.Errorf("record file commit: %w", err),
			joinError("restore last-known-good files", fileRollbackErr),
			joinError("clean staged Extensions", extensionRollbackErr),
			joinError("clean staged tools", toolRollbackErr),
			joinError("clean failed transaction journal", cleanupErr),
		)
	}
	if err := journal.setPhase(transactionToolsCommitting); err != nil {
		fileRollbackErr := files.Rollback()
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		var cleanupErr error
		if fileRollbackErr == nil && extensionRollbackErr == nil && toolRollbackErr == nil {
			cleanupErr = journal.cleanup()
		}
		report.Reason = "JournalCommitFailed"
		return report, errors.Join(
			fmt.Errorf("record tool commit start: %w", err),
			joinError("restore last-known-good files", fileRollbackErr),
			joinError("clean staged Extensions", extensionRollbackErr),
			joinError("restore last-known-good tools", toolRollbackErr),
			joinError("clean failed transaction journal", cleanupErr),
		)
	}
	if err := tools.Commit(); err != nil {
		toolRollbackErr := tools.Rollback()
		extensionRollbackErr := extensions.Rollback()
		fileRollbackErr := files.Rollback()
		var cleanupErr error
		if fileRollbackErr == nil && extensionRollbackErr == nil && toolRollbackErr == nil {
			cleanupErr = journal.cleanup()
		}
		report.Reason = "ToolCommitFailed"
		return report, errors.Join(
			fmt.Errorf("commit tools: %w", err),
			joinError("restore last-known-good tools", toolRollbackErr),
			joinError("clean staged Extensions", extensionRollbackErr),
			joinError("restore last-known-good files", fileRollbackErr),
			joinError("clean failed transaction journal", cleanupErr),
		)
	}
	if err := journal.setPhase(transactionToolsCommitted); err != nil {
		toolRollbackErr := tools.Rollback()
		extensionRollbackErr := extensions.Rollback()
		fileRollbackErr := files.Rollback()
		var cleanupErr error
		if fileRollbackErr == nil && extensionRollbackErr == nil && toolRollbackErr == nil {
			cleanupErr = journal.cleanup()
		}
		report.Reason = "JournalCommitFailed"
		return report, errors.Join(
			fmt.Errorf("record tool commit: %w", err),
			joinError("restore last-known-good tools", toolRollbackErr),
			joinError("clean staged Extensions", extensionRollbackErr),
			joinError("restore last-known-good files", fileRollbackErr),
			joinError("clean failed transaction journal", cleanupErr),
		)
	}
	if err := journal.setPhase(transactionExtensionsCommitting); err != nil {
		fileRollbackErr := files.Rollback()
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		var cleanupErr error
		if fileRollbackErr == nil && extensionRollbackErr == nil && toolRollbackErr == nil {
			cleanupErr = journal.cleanup()
		}
		report.Reason = "JournalCommitFailed"
		return report, errors.Join(
			fmt.Errorf("record Extension commit start: %w", err),
			joinError("restore last-known-good files", fileRollbackErr),
			joinError("clean staged Extensions", extensionRollbackErr),
			joinError("restore last-known-good tools", toolRollbackErr),
			joinError("clean failed transaction journal", cleanupErr),
		)
	}
	if err := extensions.Commit(); err != nil {
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		fileRollbackErr := files.Rollback()
		var cleanupErr error
		if fileRollbackErr == nil && extensionRollbackErr == nil && toolRollbackErr == nil {
			cleanupErr = journal.cleanup()
		}
		report.Reason = "ExtensionCommitFailed"
		return report, errors.Join(
			fmt.Errorf("commit Extensions: %w", err),
			joinError("restore last-known-good Extensions", extensionRollbackErr),
			joinError("restore last-known-good tools", toolRollbackErr),
			joinError("restore last-known-good files", fileRollbackErr),
			joinError("clean failed transaction journal", cleanupErr),
		)
	}
	if err := journal.setPhase(transactionExtensionsCommitted); err != nil {
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		fileRollbackErr := files.Rollback()
		var cleanupErr error
		if fileRollbackErr == nil && extensionRollbackErr == nil && toolRollbackErr == nil {
			cleanupErr = journal.cleanup()
		}
		report.Reason = "JournalCommitFailed"
		return report, errors.Join(
			fmt.Errorf("record Extension commit: %w", err),
			joinError("restore last-known-good Extensions", extensionRollbackErr),
			joinError("restore last-known-good tools", toolRollbackErr),
			joinError("restore last-known-good files", fileRollbackErr),
			joinError("clean failed transaction journal", cleanupErr),
		)
	}
	if err := applier.upstream.ApplyManagedSettings(ctx, materialized.settings); err != nil {
		settingsRollbackErr := applier.upstream.ApplyManagedSettings(ctx, previousSettings)
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		fileRollbackErr := files.Rollback()
		var cleanupErr error
		if settingsRollbackErr == nil && fileRollbackErr == nil && extensionRollbackErr == nil && toolRollbackErr == nil {
			cleanupErr = journal.cleanup()
		}
		report.Reason = "UpstreamApplyFailed"
		return report, errors.Join(
			fmt.Errorf("apply managed upstream settings: %w", err),
			joinError("restore last-known-good upstream settings", settingsRollbackErr),
			joinError("restore last-known-good files", fileRollbackErr),
			joinError("restore last-known-good Extensions", extensionRollbackErr),
			joinError("restore last-known-good tools", toolRollbackErr),
			joinError("clean failed transaction journal", cleanupErr),
		)
	}
	if err := journal.setPhase(transactionUpstreamCommitted); err != nil {
		settingsRollbackErr := applier.upstream.ApplyManagedSettings(ctx, previousSettings)
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		fileRollbackErr := files.Rollback()
		var cleanupErr error
		if settingsRollbackErr == nil && fileRollbackErr == nil && extensionRollbackErr == nil && toolRollbackErr == nil {
			cleanupErr = journal.cleanup()
		}
		report.Reason = "JournalCommitFailed"
		return report, errors.Join(
			fmt.Errorf("record upstream commit: %w", err),
			joinError("restore last-known-good upstream settings", settingsRollbackErr),
			joinError("restore last-known-good files", fileRollbackErr),
			joinError("restore last-known-good Extensions", extensionRollbackErr),
			joinError("restore last-known-good tools", toolRollbackErr),
			joinError("clean failed transaction journal", cleanupErr),
		)
	}
	if err := applier.persistState(manifest, materialized.materializationRevision); err != nil {
		settingsRollbackErr := applier.upstream.ApplyManagedSettings(ctx, previousSettings)
		extensionRollbackErr := extensions.Rollback()
		toolRollbackErr := tools.Rollback()
		fileRollbackErr := files.Rollback()
		var cleanupErr error
		if settingsRollbackErr == nil && fileRollbackErr == nil && extensionRollbackErr == nil && toolRollbackErr == nil {
			cleanupErr = journal.cleanup()
		}
		report.Reason = "StateCommitFailed"
		return report, errors.Join(
			fmt.Errorf("persist live state: %w", err),
			joinError("restore last-known-good upstream settings", settingsRollbackErr),
			joinError("restore last-known-good files", fileRollbackErr),
			joinError("restore last-known-good Extensions", extensionRollbackErr),
			joinError("restore last-known-good tools", toolRollbackErr),
			joinError("clean failed transaction journal", cleanupErr),
		)
	}

	applier.liveManifest = &manifest
	liveSettings := cloneManagedSettings(materialized.settings)
	applier.liveSettings = &liveSettings
	applier.liveMaterializationRevision = materialized.materializationRevision
	report.LiveRevision = manifest.DesiredRevision
	report.MaterializationRevision = materialized.materializationRevision
	report.State = ApplyStateProgrammed
	report.Reason = ""
	if err := errors.Join(
		joinError("finalize committed Extensions", extensions.Finalize()),
		joinError("finalize committed tools", tools.Finalize()),
		joinError("clean committed transaction journal", journal.cleanup()),
	); err != nil {
		return report, err
	}
	return report, nil
}

func (applier *Applier) LiveManifest() (render.Manifest, bool, error) {
	applier.mutex.Lock()
	defer applier.mutex.Unlock()
	if applier.liveManifest == nil {
		return render.Manifest{}, false, nil
	}
	manifest, err := cloneAppliedManifest(*applier.liveManifest)
	return manifest, err == nil, err
}

func (applier *Applier) Refresh(ctx context.Context) (Report, bool, error) {
	applier.mutex.Lock()
	defer applier.mutex.Unlock()
	report := Report{
		LiveRevision:            applier.liveRevision(),
		MaterializationRevision: applier.liveMaterializationRevision,
		State:                   ApplyStateFailed,
	}
	if applier.liveManifest == nil {
		report.Reason = "NoLiveRevision"
		return report, true, nil
	}
	report.DesiredRevision = applier.liveManifest.DesiredRevision
	materialized, err := materializeManifest(ctx, applier.secrets, *applier.liveManifest)
	if err != nil {
		report.Reason = "SecretResolutionFailed"
		return report, true, err
	}
	if materialized.materializationRevision != applier.liveMaterializationRevision {
		report.State = ApplyStateDeferred
		report.Reason = "SecretChanged"
		return report, true, nil
	}
	preparedFiles, err := applier.prepareFileTargets(ctx, materialized.files)
	if err != nil {
		report.Reason = "StatusRefreshFailed"
		return report, false, err
	}
	files, err := applier.stageFiles(*applier.liveManifest, preparedFiles)
	if err != nil {
		report.Reason = "StatusRefreshFailed"
		return report, false, err
	}
	if len(files.files) != 0 {
		report.Reason = "DriftDetected"
		return report, true, nil
	}
	matches, err := applier.upstream.ManagedSettingsMatch(ctx, materialized.settings)
	if err != nil {
		report.Reason = "StatusRefreshFailed"
		return report, false, err
	}
	if !matches {
		report.Reason = "DriftDetected"
		return report, true, nil
	}
	liveSettings := cloneManagedSettings(materialized.settings)
	applier.liveSettings = &liveSettings
	report.State = ApplyStateProgrammed
	return report, false, nil
}

func cloneAppliedManifest(manifest render.Manifest) (render.Manifest, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return render.Manifest{}, err
	}
	var result render.Manifest
	if err := json.Unmarshal(raw, &result); err != nil {
		return render.Manifest{}, err
	}
	return result, nil
}

func (applier *Applier) stageExtensions(
	ctx context.Context,
	manifest render.Manifest,
	secrets map[render.SecretReference]SecretValue,
) (ExtensionTransaction, error) {
	var previous []render.ExtensionActivation
	if applier.liveManifest != nil {
		previous = applier.liveManifest.Extensions
	}
	if len(previous) == 0 && len(manifest.Extensions) == 0 {
		return noOpExtensionTransaction{}, nil
	}
	if applier.extensions == nil {
		return nil, errors.New("Extension manager is required for Extension activations")
	}
	return applier.extensions.Stage(ctx, previous, manifest.Extensions, secrets)
}

func (applier *Applier) stageTools(
	ctx context.Context,
	manifest render.Manifest,
) (ToolTransaction, error) {
	var previous []render.ToolActivation
	if applier.liveManifest != nil {
		previous = applier.liveManifest.Tools
	}
	if len(previous) == 0 && len(manifest.Tools) == 0 {
		return noOpToolTransaction{}, nil
	}
	if applier.tools == nil {
		return nil, errors.New("tool manager is required for tool activations")
	}
	return applier.tools.Stage(ctx, previous, manifest.Tools)
}

type noOpExtensionTransaction struct{}

func (noOpExtensionTransaction) Commit() error   { return nil }
func (noOpExtensionTransaction) Rollback() error { return nil }
func (noOpExtensionTransaction) Finalize() error { return nil }

func (applier *Applier) liveRevision() string {
	if applier.liveManifest == nil {
		return ""
	}
	return applier.liveManifest.DesiredRevision
}

func (applier *Applier) statePath() string {
	return filepath.Join(applier.dataRoot, "t3-coded", "state.json")
}

func (applier *Applier) loadState() error {
	raw, err := os.ReadFile(applier.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read t3-coded state: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("parse t3-coded state: %w", err)
	}
	if err := render.VerifyManifest(state.Manifest); err != nil {
		return fmt.Errorf("verify t3-coded state: %w", err)
	}
	applier.liveManifest = &state.Manifest
	applier.liveMaterializationRevision = state.MaterializationRevision
	return nil
}

func (applier *Applier) persistState(manifest render.Manifest, materializationRevision string) error {
	raw, err := json.Marshal(persistedState{
		Manifest:                manifest,
		MaterializationRevision: materializationRevision,
	})
	if err != nil {
		return err
	}
	return atomicWriteFile(applier.statePath(), raw, 0o600)
}

func affectedInstanceIDs(manifest render.Manifest, previous *render.Manifest) []string {
	instances := make(map[string]struct{}, len(manifest.ProviderInstances))
	for instanceID := range manifest.ProviderInstances {
		instances[instanceID] = struct{}{}
	}
	if previous != nil {
		for instanceID := range previous.ProviderInstances {
			instances[instanceID] = struct{}{}
		}
	}
	result := make([]string, 0, len(instances))
	for instanceID := range instances {
		result = append(result, instanceID)
	}
	sort.Strings(result)
	return result
}

func joinError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
