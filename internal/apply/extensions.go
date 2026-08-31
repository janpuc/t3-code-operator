package apply

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/janpuc/t3-code-operator/internal/render"
)

const defaultExtensionFetchTimeout = 10 * time.Minute

type ExtensionFetcher interface {
	Fetch(context.Context, render.ExtensionSource, *SecretValue, string) error
}

type CachedExtensionActivation struct {
	Activation render.ExtensionActivation
	CachePath  string
}

type ExtensionInstallerRequest struct {
	Kind       render.InstallerKind
	Previous   []CachedExtensionActivation
	Desired    []CachedExtensionActivation
	Recovering bool
}

type ExtensionInstallerRunner interface {
	Stage(context.Context, ExtensionInstallerRequest) (ExtensionTransaction, error)
}

type CachedExtensionManagerConfig struct {
	DataRoot     string
	FetchTimeout time.Duration
	Fetchers     map[render.ExtensionSourceType]ExtensionFetcher
	Installers   map[render.InstallerKind]ExtensionInstallerRunner
}

type CachedExtensionManager struct {
	dataRoot     string
	cacheRoot    string
	fetchTimeout time.Duration
	fetchers     map[render.ExtensionSourceType]ExtensionFetcher
	installers   map[render.InstallerKind]ExtensionInstallerRunner
}

func NewCachedExtensionManager(config CachedExtensionManagerConfig) (*CachedExtensionManager, error) {
	if config.DataRoot == "" || !filepath.IsAbs(config.DataRoot) {
		return nil, errors.New("data root must be an absolute path")
	}
	dataRoot := filepath.Clean(config.DataRoot)
	fetchTimeout := config.FetchTimeout
	if fetchTimeout <= 0 {
		fetchTimeout = defaultExtensionFetchTimeout
	}
	fetchers := make(map[render.ExtensionSourceType]ExtensionFetcher, len(config.Fetchers))
	for sourceType, fetcher := range config.Fetchers {
		if fetcher == nil {
			return nil, fmt.Errorf("Extension fetcher %s is nil", sourceType)
		}
		fetchers[sourceType] = fetcher
	}
	installers := make(map[render.InstallerKind]ExtensionInstallerRunner, len(config.Installers))
	for kind, installer := range config.Installers {
		if installer == nil {
			return nil, fmt.Errorf("Extension installer %s is nil", kind)
		}
		installers[kind] = installer
	}
	return &CachedExtensionManager{
		dataRoot:     dataRoot,
		cacheRoot:    filepath.Join(dataRoot, "t3-coded", "extensions", "cache"),
		fetchTimeout: fetchTimeout,
		fetchers:     fetchers,
		installers:   installers,
	}, nil
}

func (manager *CachedExtensionManager) Stage(
	ctx context.Context,
	previous []render.ExtensionActivation,
	desired []render.ExtensionActivation,
	secrets map[render.SecretReference]SecretValue,
) (ExtensionTransaction, error) {
	return manager.stage(ctx, previous, desired, secrets, false)
}

func (manager *CachedExtensionManager) StageRecovery(
	ctx context.Context,
	interrupted []render.ExtensionActivation,
	desired []render.ExtensionActivation,
) (ExtensionTransaction, error) {
	return manager.stage(ctx, interrupted, desired, map[render.SecretReference]SecretValue{}, true)
}

func (manager *CachedExtensionManager) stage(
	ctx context.Context,
	previous []render.ExtensionActivation,
	desired []render.ExtensionActivation,
	secrets map[render.SecretReference]SecretValue,
	recovering bool,
) (ExtensionTransaction, error) {
	if err := validateExtensionActivations(previous); err != nil {
		return nil, fmt.Errorf("validate previous Extensions: %w", err)
	}
	if err := validateExtensionActivations(desired); err != nil {
		return nil, fmt.Errorf("validate desired Extensions: %w", err)
	}

	cachePaths := make(map[string]string)
	orderedDesired := append([]render.ExtensionActivation(nil), desired...)
	sortExtensionActivations(orderedDesired)
	for _, activation := range orderedDesired {
		if _, exists := cachePaths[activation.CacheKey]; exists {
			continue
		}
		path, exists, err := manager.loadCache(activation.Source, activation.CacheKey)
		if err != nil {
			return nil, fmt.Errorf("load Extension %s/%s cache: %w", activation.InstanceID, activation.Name, err)
		}
		if exists {
			cachePaths[activation.CacheKey] = path
			continue
		}
		credential, err := extensionCredential(activation.Source, secrets)
		if err != nil {
			return nil, fmt.Errorf("stage Extension %s/%s: %w", activation.InstanceID, activation.Name, err)
		}
		path, err = manager.ensureCache(ctx, activation.Source, activation.CacheKey, credential)
		if err != nil {
			return nil, fmt.Errorf("cache Extension %s/%s: %w", activation.InstanceID, activation.Name, err)
		}
		cachePaths[activation.CacheKey] = path
	}

	orderedPrevious := append([]render.ExtensionActivation(nil), previous...)
	sortExtensionActivations(orderedPrevious)
	for _, activation := range orderedPrevious {
		if _, exists := cachePaths[activation.CacheKey]; exists {
			continue
		}
		path, exists, err := manager.loadCache(activation.Source, activation.CacheKey)
		if err != nil {
			return nil, fmt.Errorf("load prior Extension %s/%s: %w", activation.InstanceID, activation.Name, err)
		}
		if !exists {
			return nil, fmt.Errorf("prior Extension %s/%s has no cached source", activation.InstanceID, activation.Name)
		}
		cachePaths[activation.CacheKey] = path
	}

	links, err := manager.stageExtensionLinks(previous, desired, cachePaths, recovering)
	if err != nil {
		return nil, err
	}
	transactions := []ExtensionTransaction{links}
	installerTransactions, err := manager.stageInstallers(ctx, previous, desired, cachePaths, recovering)
	if err != nil {
		_ = rollbackExtensionTransactions(transactions)
		return nil, err
	}
	transactions = append(transactions, installerTransactions...)
	return &compositeExtensionTransaction{transactions: transactions}, nil
}

func validateExtensionActivations(activations []render.ExtensionActivation) error {
	identities := make(map[string]struct{}, len(activations))
	destinations := make(map[string]struct{})
	for _, activation := range activations {
		if activation.InstanceID == "" || activation.Name == "" {
			return errors.New("Extension instance ID and name are required")
		}
		identity := activation.InstanceID + "\x00" + activation.Name
		if _, exists := identities[identity]; exists {
			return fmt.Errorf("Extension %s/%s appears more than once", activation.InstanceID, activation.Name)
		}
		identities[identity] = struct{}{}
		expected, err := render.ExtensionCacheKey(activation.Source)
		if err != nil {
			return fmt.Errorf("calculate cache key for %s/%s: %w", activation.InstanceID, activation.Name, err)
		}
		if activation.CacheKey != expected {
			return fmt.Errorf("Extension %s/%s has a mismatched cache key", activation.InstanceID, activation.Name)
		}
		if (len(activation.Destinations) == 0) == (activation.Installer == nil) {
			return fmt.Errorf("Extension %s/%s must select destinations or one installer", activation.InstanceID, activation.Name)
		}
		for _, destination := range activation.Destinations {
			if destination.Mode != render.WriteModeReplace {
				return fmt.Errorf("Extension destination %s must use Replace mode", destination.Path)
			}
			key := activation.InstanceID + "\x00" + destination.Path
			if _, exists := destinations[key]; exists {
				return fmt.Errorf("Extension destination %s appears more than once", destination.Path)
			}
			destinations[key] = struct{}{}
		}
		if activation.Installer != nil && activation.Installer.Kind == "" {
			return fmt.Errorf("Extension %s/%s has no installer kind", activation.InstanceID, activation.Name)
		}
	}
	return nil
}

func sortExtensionActivations(activations []render.ExtensionActivation) {
	sort.Slice(activations, func(i, j int) bool {
		if activations[i].InstanceID != activations[j].InstanceID {
			return activations[i].InstanceID < activations[j].InstanceID
		}
		return activations[i].Name < activations[j].Name
	})
}

func extensionCredential(
	source render.ExtensionSource,
	secrets map[render.SecretReference]SecretValue,
) (*SecretValue, error) {
	var reference *render.SecretReference
	switch source.Type {
	case render.ExtensionSourceGit:
		if source.Git != nil {
			reference = source.Git.CredentialSecretRef
		}
	case render.ExtensionSourceOCI:
		if source.OCI != nil {
			reference = source.OCI.CredentialSecretRef
		}
	case render.ExtensionSourceMarketplace:
		if source.Marketplace != nil {
			reference = source.Marketplace.CredentialSecretRef
		}
	case render.ExtensionSourceGitHubRelease:
		if source.GitHubRelease != nil {
			reference = source.GitHubRelease.CredentialSecretRef
		}
	}
	if reference == nil {
		return nil, nil
	}
	value, exists := secrets[*reference]
	if !exists {
		return nil, fmt.Errorf("credential Secret %s/%s key %s is unresolved", reference.Namespace, reference.Name, reference.Key)
	}
	return &value, nil
}

func (manager *CachedExtensionManager) stageInstallers(
	ctx context.Context,
	previous []render.ExtensionActivation,
	desired []render.ExtensionActivation,
	cachePaths map[string]string,
	recovering bool,
) ([]ExtensionTransaction, error) {
	type installerGroup struct {
		previous []CachedExtensionActivation
		desired  []CachedExtensionActivation
	}
	groups := make(map[render.InstallerKind]*installerGroup)
	add := func(activations []render.ExtensionActivation, prior bool) {
		for _, activation := range activations {
			if activation.Installer == nil {
				continue
			}
			kind := activation.Installer.Kind
			group := groups[kind]
			if group == nil {
				group = &installerGroup{}
				groups[kind] = group
			}
			cached := CachedExtensionActivation{
				Activation: activation,
				CachePath:  cachePaths[activation.CacheKey],
			}
			if prior {
				group.previous = append(group.previous, cached)
			} else {
				group.desired = append(group.desired, cached)
			}
		}
	}
	add(previous, true)
	add(desired, false)

	kinds := make([]render.InstallerKind, 0, len(groups))
	for kind := range groups {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	transactions := make([]ExtensionTransaction, 0, len(kinds))
	for _, kind := range kinds {
		runner := manager.installers[kind]
		if runner == nil {
			_ = rollbackExtensionTransactions(transactions)
			return nil, fmt.Errorf("Extension installer %s is unavailable", kind)
		}
		group := groups[kind]
		sortCachedActivations(group.previous)
		sortCachedActivations(group.desired)
		transaction, err := runner.Stage(ctx, ExtensionInstallerRequest{
			Kind:       kind,
			Previous:   group.previous,
			Desired:    group.desired,
			Recovering: recovering,
		})
		if err != nil {
			_ = rollbackExtensionTransactions(transactions)
			return nil, fmt.Errorf("stage Extension installer %s: %w", kind, err)
		}
		if transaction == nil {
			_ = rollbackExtensionTransactions(transactions)
			return nil, fmt.Errorf("Extension installer %s returned no transaction", kind)
		}
		transactions = append(transactions, transaction)
	}
	return transactions, nil
}

func sortCachedActivations(activations []CachedExtensionActivation) {
	sort.Slice(activations, func(i, j int) bool {
		left := activations[i].Activation
		right := activations[j].Activation
		if left.InstanceID != right.InstanceID {
			return left.InstanceID < right.InstanceID
		}
		return left.Name < right.Name
	})
}

type compositeExtensionTransaction struct {
	transactions []ExtensionTransaction
}

func (transaction *compositeExtensionTransaction) Commit() error {
	for _, child := range transaction.transactions {
		if err := child.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (transaction *compositeExtensionTransaction) Rollback() error {
	return rollbackExtensionTransactions(transaction.transactions)
}

func (transaction *compositeExtensionTransaction) Finalize() error {
	var result error
	for _, child := range transaction.transactions {
		if err := child.Finalize(); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func rollbackExtensionTransactions(transactions []ExtensionTransaction) error {
	var result error
	for index := len(transactions) - 1; index >= 0; index-- {
		if err := transactions[index].Rollback(); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func isPathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
