package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/janpuc/t3-code-operator/internal/render"
)

const (
	transactionJournalVersion  = 4
	maxTransactionJournalBytes = render.MaxRenderedManifestBytes
)

var transactionDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type transactionPhase string

const (
	transactionPrepared             transactionPhase = "Prepared"
	transactionFilesCommitted       transactionPhase = "FilesCommitted"
	transactionToolsCommitting      transactionPhase = "ToolsCommitting"
	transactionToolsCommitted       transactionPhase = "ToolsCommitted"
	transactionExtensionsCommitting transactionPhase = "ExtensionsCommitting"
	transactionExtensionsCommitted  transactionPhase = "ExtensionsCommitted"
	transactionUpstreamCommitted    transactionPhase = "UpstreamCommitted"
)

type transactionRecord struct {
	Version            int                          `json:"version"`
	DesiredRevision    string                       `json:"desiredRevision"`
	Phase              transactionPhase             `json:"phase"`
	InstanceIDs        []string                     `json:"instanceIds"`
	Files              []transactionFile            `json:"files,omitempty"`
	PreviousExtensions []render.ExtensionActivation `json:"previousExtensions,omitempty"`
	DesiredExtensions  []render.ExtensionActivation `json:"desiredExtensions,omitempty"`
	PreviousTools      []render.ToolActivation      `json:"previousTools,omitempty"`
	DesiredTools       []render.ToolActivation      `json:"desiredTools,omitempty"`
}

type transactionFile struct {
	LogicalPath string           `json:"logicalPath"`
	Scope       render.FileScope `json:"scope,omitempty"`
	InstanceID  string           `json:"instanceId,omitempty"`
	Backup      string           `json:"backup,omitempty"`
	Existed     bool             `json:"existed"`
	Mode        fs.FileMode      `json:"mode,omitempty"`
}

type pendingTransaction struct {
	directory string
	record    transactionRecord
}

func (applier *Applier) prepareJournal(
	manifest render.Manifest,
	files *fileTransaction,
) (*pendingTransaction, error) {
	if err := applier.ensureTransactionRoot(); err != nil {
		return nil, err
	}
	digest := strings.TrimPrefix(manifest.DesiredRevision, "sha256:")
	if !transactionDigestPattern.MatchString(digest) {
		return nil, errors.New("desired revision cannot identify a transaction")
	}
	directory := filepath.Join(applier.transactionsPath(), digest)
	if _, err := os.Lstat(directory); err == nil {
		return nil, errors.New("transaction directory already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	record := transactionRecord{
		Version:           transactionJournalVersion,
		DesiredRevision:   manifest.DesiredRevision,
		Phase:             transactionPrepared,
		InstanceIDs:       affectedInstanceIDs(manifest, applier.liveManifest),
		Files:             make([]transactionFile, 0, len(files.files)),
		DesiredExtensions: append([]render.ExtensionActivation(nil), manifest.Extensions...),
		DesiredTools:      normalizedToolActivations(manifest.Tools),
	}
	if applier.liveManifest != nil {
		record.PreviousExtensions = append([]render.ExtensionActivation(nil), applier.liveManifest.Extensions...)
		record.PreviousTools = normalizedToolActivations(applier.liveManifest.Tools)
	}
	for index, staged := range files.files {
		entry := transactionFile{
			LogicalPath: staged.logicalPath,
			Scope:       staged.scope,
			InstanceID:  staged.instanceID,
			Existed:     staged.existed,
			Mode:        staged.originalMode,
		}
		if staged.existed {
			entry.Backup = fmt.Sprintf("%06d.backup", index)
			backupPath := filepath.Join(directory, entry.Backup)
			if err := os.Link(staged.physicalPath, backupPath); err != nil {
				if writeErr := atomicWriteFile(backupPath, staged.original, 0o600); writeErr != nil {
					_ = cleanupTransactionDirectory(directory)
					return nil, errors.Join(err, writeErr)
				}
			}
		}
		record.Files = append(record.Files, entry)
	}
	journal := &pendingTransaction{directory: directory, record: record}
	if err := journal.write(); err != nil {
		_ = cleanupTransactionDirectory(directory)
		return nil, err
	}
	return journal, nil
}

func (transaction *pendingTransaction) setPhase(phase transactionPhase) error {
	transaction.record.Phase = phase
	return transaction.write()
}

func (transaction *pendingTransaction) write() error {
	raw, err := json.Marshal(transaction.record)
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(transaction.directory, "journal.json"), raw, 0o600)
}

func (transaction *pendingTransaction) cleanup() error {
	return cleanupTransactionDirectory(transaction.directory)
}

func cleanupTransactionDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("transaction path is not a directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("unexpected directory %s in transaction", entry.Name())
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(directory)
}

func (applier *Applier) transactionsPath() string {
	return filepath.Join(applier.dataRoot, "t3-coded", "transactions")
}

func (applier *Applier) ensureTransactionRoot() error {
	root := applier.transactionsPath()
	if err := rejectSymlinkComponents(applier.dataRoot, root); err != nil {
		return fmt.Errorf("validate transaction directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create transaction directory: %w", err)
	}
	if err := rejectSymlinkComponents(applier.dataRoot, root); err != nil {
		return fmt.Errorf("validate created transaction directory: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect transaction directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("transaction root is not a directory")
	}
	return nil
}

func (applier *Applier) cleanupCommittedJournal() error {
	if applier.liveManifest == nil {
		return nil
	}
	if err := applier.ensureTransactionRoot(); err != nil {
		return err
	}
	digest := strings.TrimPrefix(applier.liveManifest.DesiredRevision, "sha256:")
	if !transactionDigestPattern.MatchString(digest) {
		return errors.New("live revision cannot identify a transaction")
	}
	directory := filepath.Join(applier.transactionsPath(), digest)
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect committed transaction journal: %w", err)
	}
	if err := cleanupTransactionDirectory(directory); err != nil {
		return fmt.Errorf("clean committed transaction journal: %w", err)
	}
	return nil
}

func (applier *Applier) loadPendingTransaction() error {
	if err := applier.ensureTransactionRoot(); err != nil {
		return err
	}
	root := applier.transactionsPath()
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read transaction directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if !entry.IsDir() {
			return fmt.Errorf("unexpected transaction entry %s", entry.Name())
		}
		directory := filepath.Join(root, entry.Name())
		raw, err := readTransactionJournal(filepath.Join(directory, "journal.json"))
		if errors.Is(err, os.ErrNotExist) {
			if cleanupErr := cleanupTransactionDirectory(directory); cleanupErr != nil {
				return cleanupErr
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("read transaction journal: %w", err)
		}
		var record transactionRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("parse transaction journal: %w", err)
		}
		if err := validateTransactionRecord(record); err != nil {
			return err
		}
		transaction := &pendingTransaction{directory: directory, record: record}
		if applier.liveManifest != nil && applier.liveManifest.DesiredRevision == record.DesiredRevision {
			if err := transaction.cleanup(); err != nil {
				return err
			}
			continue
		}
		if applier.pendingTransaction != nil {
			return errors.New("more than one unfinished transaction exists")
		}
		applier.pendingTransaction = transaction
	}
	return nil
}

func readTransactionJournal(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("transaction journal is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxTransactionJournalBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxTransactionJournalBytes {
		return nil, errors.New("transaction journal exceeds its size limit")
	}
	return raw, nil
}

func validateTransactionRecord(record transactionRecord) error {
	if record.Version != 3 && record.Version != transactionJournalVersion {
		return fmt.Errorf("unsupported transaction journal version %d", record.Version)
	}
	if !transactionDigestPattern.MatchString(strings.TrimPrefix(record.DesiredRevision, "sha256:")) {
		return errors.New("transaction has an invalid desired revision")
	}
	switch record.Phase {
	case transactionPrepared,
		transactionFilesCommitted,
		transactionToolsCommitting,
		transactionToolsCommitted,
		transactionExtensionsCommitting,
		transactionExtensionsCommitted,
		transactionUpstreamCommitted:
	default:
		return fmt.Errorf("transaction has an invalid phase %q", record.Phase)
	}
	if err := validateExtensionActivations(record.PreviousExtensions); err != nil {
		return fmt.Errorf("transaction has invalid prior Extensions: %w", err)
	}
	if err := validateExtensionActivations(record.DesiredExtensions); err != nil {
		return fmt.Errorf("transaction has invalid desired Extensions: %w", err)
	}
	if err := render.ValidateToolActivations(record.PreviousTools); err != nil {
		return fmt.Errorf("transaction has invalid prior tools: %w", err)
	}
	if err := render.ValidateToolActivations(record.DesiredTools); err != nil {
		return fmt.Errorf("transaction has invalid desired tools: %w", err)
	}
	for index, file := range record.Files {
		scope := normalizedFileScope(file.Scope)
		switch scope {
		case render.FileScopeHarness:
			resolvedInstanceID, err := instanceIDFromManagedPath(file.LogicalPath)
			if err != nil {
				return err
			}
			if file.InstanceID == "" || resolvedInstanceID != file.InstanceID {
				return fmt.Errorf("transaction file %d has a mismatched instance ID", index)
			}
		case render.FileScopeWorkstation:
			if file.InstanceID != "" {
				return fmt.Errorf("transaction file %d has an unexpected instance ID", index)
			}
			if _, exists := workstationFileContracts[file.LogicalPath]; !exists {
				return fmt.Errorf("transaction file %d has a disallowed Workstation path", index)
			}
		default:
			return fmt.Errorf("transaction file %d has an invalid scope", index)
		}
		if file.Existed && file.Backup != fmt.Sprintf("%06d.backup", index) {
			return fmt.Errorf("transaction file %d has an invalid backup name", index)
		}
	}
	return nil
}

func (applier *Applier) recoverPending(ctx context.Context) (bool, error) {
	transaction := applier.pendingTransaction
	if transaction == nil {
		return false, nil
	}
	active, err := applier.activity.ActiveInstances(ctx, transaction.record.InstanceIDs)
	if err != nil {
		return false, fmt.Errorf("read activity for transaction recovery: %w", err)
	}
	if len(active) != 0 {
		return true, nil
	}
	if transactionNeedsExtensionRecovery(transaction.record.Phase) {
		if err := applier.recoverExtensions(ctx, transaction.record); err != nil {
			return false, err
		}
	}
	if transactionNeedsToolRecovery(transaction.record.Phase) {
		if err := applier.recoverTools(ctx, transaction.record); err != nil {
			return false, err
		}
	}

	for _, file := range transaction.record.Files {
		physicalPath, err := applier.physicalManagedPath(file.LogicalPath, file.Scope, file.InstanceID)
		if err != nil {
			return false, err
		}
		if file.Existed {
			backupPath := filepath.Join(transaction.directory, file.Backup)
			raw, err := os.ReadFile(backupPath)
			if err != nil {
				return false, fmt.Errorf("read transaction backup: %w", err)
			}
			if err := atomicWriteFile(physicalPath, raw, file.Mode); err != nil {
				return false, fmt.Errorf("restore %s: %w", file.LogicalPath, err)
			}
		} else {
			if err := os.Remove(physicalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, fmt.Errorf("remove uncommitted file %s: %w", file.LogicalPath, err)
			}
		}
	}

	settings := ManagedSettings{ProviderInstances: map[string]ProviderInstance{}}
	if applier.liveManifest != nil {
		materialized, err := materializeManifest(ctx, applier.secrets, *applier.liveManifest)
		if err != nil {
			return false, fmt.Errorf("materialize last-known-good settings during recovery: %w", err)
		}
		settings = materialized.settings
	}
	if err := applier.upstream.ApplyManagedSettings(ctx, settings); err != nil {
		return false, fmt.Errorf("restore last-known-good settings during recovery: %w", err)
	}
	liveSettings := cloneManagedSettings(settings)
	applier.liveSettings = &liveSettings
	if err := transaction.cleanup(); err != nil {
		return false, err
	}
	applier.pendingTransaction = nil
	return false, nil
}

func transactionNeedsExtensionRecovery(phase transactionPhase) bool {
	switch phase {
	case transactionExtensionsCommitting, transactionExtensionsCommitted, transactionUpstreamCommitted:
		return true
	default:
		return false
	}
}

func transactionNeedsToolRecovery(phase transactionPhase) bool {
	switch phase {
	case transactionToolsCommitting,
		transactionToolsCommitted,
		transactionExtensionsCommitting,
		transactionExtensionsCommitted,
		transactionUpstreamCommitted:
		return true
	default:
		return false
	}
}

func (applier *Applier) recoverExtensions(ctx context.Context, record transactionRecord) error {
	if len(record.PreviousExtensions) == 0 && len(record.DesiredExtensions) == 0 {
		return nil
	}
	manager, ok := applier.extensions.(ExtensionRecoveryManager)
	if !ok {
		return errors.New("Extension manager does not support crash recovery")
	}
	transaction, err := manager.StageRecovery(ctx, record.DesiredExtensions, record.PreviousExtensions)
	if err != nil {
		return fmt.Errorf("stage Extension crash recovery: %w", err)
	}
	if transaction == nil {
		return errors.New("Extension manager returned no crash recovery transaction")
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("restore Extensions after crash: %w", err)
	}
	if err := transaction.Finalize(); err != nil {
		return fmt.Errorf("finalize Extension crash recovery: %w", err)
	}
	return nil
}

func (applier *Applier) recoverTools(ctx context.Context, record transactionRecord) error {
	if len(record.PreviousTools) == 0 && len(record.DesiredTools) == 0 {
		return nil
	}
	manager, ok := applier.tools.(ToolRecoveryManager)
	if !ok {
		return errors.New("tool manager does not support crash recovery")
	}
	transaction, err := manager.StageRecovery(ctx, record.DesiredTools, record.PreviousTools)
	if err != nil {
		return fmt.Errorf("stage tool crash recovery: %w", err)
	}
	if transaction == nil {
		return errors.New("tool manager returned no crash recovery transaction")
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("restore tools after crash: %w", err)
	}
	if err := transaction.Finalize(); err != nil {
		return fmt.Errorf("finalize tool crash recovery: %w", err)
	}
	return nil
}

func instanceIDFromManagedPath(path string) (string, error) {
	const prefix = "/data/harnesses/"
	if !strings.HasPrefix(path, prefix) {
		return "", fmt.Errorf("managed path %s is outside the harness root", path)
	}
	remainder := strings.TrimPrefix(path, prefix)
	instanceID, _, found := strings.Cut(remainder, "/")
	if !found || instanceID == "" {
		return "", fmt.Errorf("managed path %s has no provider instance", path)
	}
	return instanceID, nil
}
