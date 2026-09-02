package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/janpuc/t3-code-operator/internal/render"
)

const nativeInstallerRollbackTimeout = 2 * time.Minute

var nativePluginIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,252}$`)

type nativePluginState struct {
	marketplaces map[string]string
	plugins      map[string]bool
}

type nativePluginCLI interface {
	Snapshot(context.Context, string) (nativePluginState, error)
	AddMarketplace(context.Context, string, string, string) error
	RemoveMarketplace(context.Context, string, string) error
	InstallPlugin(context.Context, string, string) error
	RemovePlugin(context.Context, string, string) error
}

type NativeExtensionInstallerConfig struct {
	DataRoot     string
	CodexBinary  string
	ClaudeBinary string
}

func NewNativeExtensionInstallers(config NativeExtensionInstallerConfig) (map[render.InstallerKind]ExtensionInstallerRunner, error) {
	if config.DataRoot == "" || !filepath.IsAbs(config.DataRoot) {
		return nil, errors.New("data root must be an absolute path")
	}
	dataRoot := filepath.Clean(config.DataRoot)
	codexBinary := config.CodexBinary
	if codexBinary == "" {
		codexBinary = "codex"
	}
	claudeBinary := config.ClaudeBinary
	if claudeBinary == "" {
		claudeBinary = "claude"
	}
	codex := &nativeMarketplaceInstallerRunner{
		dataRoot:        dataRoot,
		cacheRoot:       filepath.Join(dataRoot, "t3-coded", "extensions", "cache"),
		driverDirectory: "codex",
		kinds: map[render.InstallerKind]struct{}{
			render.InstallerCodexMarketplace:   {},
			render.InstallerCodexReleaseBundle: {},
		},
		cli: &codexNativePluginCLI{executor: nativeCLIExecutor{
			binary:      codexBinary,
			homeEnvName: "CODEX_HOME",
		}},
	}
	claude := &nativeMarketplaceInstallerRunner{
		dataRoot:        dataRoot,
		cacheRoot:       filepath.Join(dataRoot, "t3-coded", "extensions", "cache"),
		driverDirectory: "claude",
		kinds: map[render.InstallerKind]struct{}{
			render.InstallerClaudeMarketplace:   {},
			render.InstallerClaudeReleaseBundle: {},
		},
		cli: &claudeNativePluginCLI{executor: nativeCLIExecutor{
			binary:      claudeBinary,
			homeEnvName: "CLAUDE_CONFIG_DIR",
		}},
	}
	return map[render.InstallerKind]ExtensionInstallerRunner{
		render.InstallerCodexMarketplace:    codex,
		render.InstallerCodexReleaseBundle:  codex,
		render.InstallerClaudeMarketplace:   claude,
		render.InstallerClaudeReleaseBundle: claude,
	}, nil
}

type nativeMarketplaceInstallerRunner struct {
	dataRoot        string
	cacheRoot       string
	driverDirectory string
	kinds           map[render.InstallerKind]struct{}
	cli             nativePluginCLI
}

type nativeMarketplaceKey struct {
	instanceID  string
	marketplace string
}

type nativeMarketplaceSpec struct {
	key       nativeMarketplaceKey
	cachePath string
	plugins   map[string]struct{}
}

func (runner *nativeMarketplaceInstallerRunner) Stage(
	ctx context.Context,
	request ExtensionInstallerRequest,
) (ExtensionTransaction, error) {
	if runner == nil || runner.cli == nil {
		return nil, errors.New("native Extension installer is not configured")
	}
	if _, accepted := runner.kinds[request.Kind]; !accepted {
		return nil, fmt.Errorf("native Extension installer cannot handle %s", request.Kind)
	}
	previous, err := runner.collectSpecs(request.Kind, request.Previous)
	if err != nil {
		return nil, fmt.Errorf("stage prior native Extensions: %w", err)
	}
	desired, err := runner.collectSpecs(request.Kind, request.Desired)
	if err != nil {
		return nil, fmt.Errorf("stage desired native Extensions: %w", err)
	}
	keys := nativeMarketplaceKeys(previous, desired)
	instances := make([]string, 0)
	seenInstances := map[string]struct{}{}
	for _, key := range keys {
		if _, exists := seenInstances[key.instanceID]; exists {
			continue
		}
		seenInstances[key.instanceID] = struct{}{}
		instances = append(instances, key.instanceID)
	}
	sort.Strings(instances)
	return &nativeInstallerTransaction{
		ctx:        ctx,
		runner:     runner,
		previous:   previous,
		desired:    desired,
		keys:       keys,
		instances:  instances,
		recovering: request.Recovering,
	}, nil
}

func (runner *nativeMarketplaceInstallerRunner) collectSpecs(
	kind render.InstallerKind,
	activations []CachedExtensionActivation,
) (map[nativeMarketplaceKey]*nativeMarketplaceSpec, error) {
	result := make(map[nativeMarketplaceKey]*nativeMarketplaceSpec)
	for _, cached := range activations {
		activation := cached.Activation
		if !nativePluginIdentifierPattern.MatchString(activation.InstanceID) {
			return nil, fmt.Errorf("provider instance %q is not safe", activation.InstanceID)
		}
		installer := activation.Installer
		if installer == nil || installer.Kind != kind {
			return nil, fmt.Errorf("Extension %s/%s has a mismatched native installer", activation.InstanceID, activation.Name)
		}
		if !nativePluginIdentifierPattern.MatchString(installer.Marketplace) ||
			!nativePluginIdentifierPattern.MatchString(installer.Extension) {
			return nil, fmt.Errorf("Extension %s/%s has an unsafe native plugin identifier", activation.InstanceID, activation.Name)
		}
		if err := validateNativeInstallerSource(kind, activation.Source); err != nil {
			return nil, fmt.Errorf("Extension %s/%s: %w", activation.InstanceID, activation.Name, err)
		}
		cachePath := filepath.Clean(cached.CachePath)
		if !filepath.IsAbs(cachePath) || cachePath == runner.cacheRoot || !isPathWithin(runner.cacheRoot, cachePath) {
			return nil, fmt.Errorf("Extension %s/%s cache path is outside the managed cache", activation.InstanceID, activation.Name)
		}
		info, err := os.Lstat(cachePath)
		if err != nil {
			return nil, fmt.Errorf("inspect Extension %s/%s cache: %w", activation.InstanceID, activation.Name, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("Extension %s/%s cache path is not a directory", activation.InstanceID, activation.Name)
		}
		cachePath, err = descendSingleRootDirectory(cachePath)
		if err != nil {
			return nil, fmt.Errorf("inspect Extension %s/%s cache: %w", activation.InstanceID, activation.Name, err)
		}
		key := nativeMarketplaceKey{instanceID: activation.InstanceID, marketplace: installer.Marketplace}
		spec := result[key]
		if spec == nil {
			spec = &nativeMarketplaceSpec{key: key, cachePath: cachePath, plugins: map[string]struct{}{}}
			result[key] = spec
		} else if spec.cachePath != cachePath {
			return nil, fmt.Errorf("marketplace %s/%s resolves to more than one pinned cache", key.instanceID, key.marketplace)
		}
		pluginID := installer.Extension + "@" + installer.Marketplace
		if _, exists := spec.plugins[pluginID]; exists {
			return nil, fmt.Errorf("native plugin %s appears more than once", pluginID)
		}
		spec.plugins[pluginID] = struct{}{}
	}
	return result, nil
}

func descendSingleRootDirectory(path string) (string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || !entries[0].Type().IsDir() {
		return path, nil
	}
	return filepath.Join(path, entries[0].Name()), nil
}

func validateNativeInstallerSource(kind render.InstallerKind, source render.ExtensionSource) error {
	switch kind {
	case render.InstallerCodexMarketplace, render.InstallerClaudeMarketplace:
		if source.Type != render.ExtensionSourceMarketplace || source.Marketplace == nil {
			return errors.New("Marketplace installer requires a Marketplace source")
		}
	case render.InstallerCodexReleaseBundle, render.InstallerClaudeReleaseBundle:
		if source.Type != render.ExtensionSourceGitHubRelease || source.GitHubRelease == nil {
			return errors.New("release installer requires a GitHubRelease source")
		}
	default:
		return fmt.Errorf("native installer kind %s is not supported", kind)
	}
	return nil
}

func nativeMarketplaceKeys(groups ...map[nativeMarketplaceKey]*nativeMarketplaceSpec) []nativeMarketplaceKey {
	seen := map[nativeMarketplaceKey]struct{}{}
	for _, group := range groups {
		for key := range group {
			seen[key] = struct{}{}
		}
	}
	keys := make([]nativeMarketplaceKey, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].instanceID != keys[j].instanceID {
			return keys[i].instanceID < keys[j].instanceID
		}
		return keys[i].marketplace < keys[j].marketplace
	})
	return keys
}

type nativeInstallerTransaction struct {
	ctx        context.Context
	runner     *nativeMarketplaceInstallerRunner
	previous   map[nativeMarketplaceKey]*nativeMarketplaceSpec
	desired    map[nativeMarketplaceKey]*nativeMarketplaceSpec
	keys       []nativeMarketplaceKey
	instances  []string
	recovering bool

	mutated   bool
	committed bool
	finalized bool
}

func (transaction *nativeInstallerTransaction) Commit() error {
	if transaction.finalized {
		return errors.New("native Extension transaction is finalized")
	}
	if transaction.committed {
		return errors.New("native Extension transaction is already committed")
	}
	ctx := transaction.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if transaction.recovering {
		if len(transaction.keys) != 0 {
			transaction.mutated = true
		}
		for _, instanceID := range transaction.instances {
			if err := transaction.reconcileInstance(ctx, instanceID, transaction.desired); err != nil {
				return fmt.Errorf("recover native plugin state for %s: %w", instanceID, err)
			}
		}
		transaction.committed = true
		return nil
	}
	homes := make(map[string]string, len(transaction.instances))
	for _, instanceID := range transaction.instances {
		home, err := transaction.runner.ensureHome(instanceID)
		if err != nil {
			return err
		}
		homes[instanceID] = home
		state, err := transaction.runner.cli.Snapshot(ctx, home)
		if err != nil {
			return fmt.Errorf("inspect native plugin state for %s: %w", instanceID, err)
		}
		if err := transaction.validateState(instanceID, state, transaction.previous, "baseline"); err != nil {
			return err
		}
	}

	for _, key := range transaction.keys {
		previous := transaction.previous[key]
		desired := transaction.desired[key]
		if nativeMarketplaceSpecsEqual(previous, desired) {
			continue
		}
		transaction.mutated = true
		if err := transaction.applyTransition(ctx, homes[key.instanceID], previous, desired); err != nil {
			return fmt.Errorf("reconcile native marketplace %s/%s: %w", key.instanceID, key.marketplace, err)
		}
	}

	for _, instanceID := range transaction.instances {
		state, err := transaction.runner.cli.Snapshot(ctx, homes[instanceID])
		if err != nil {
			return fmt.Errorf("verify native plugin state for %s: %w", instanceID, err)
		}
		if err := transaction.validateState(instanceID, state, transaction.desired, "final"); err != nil {
			return err
		}
	}
	transaction.committed = true
	return nil
}

func (transaction *nativeInstallerTransaction) applyTransition(
	ctx context.Context,
	home string,
	previous *nativeMarketplaceSpec,
	desired *nativeMarketplaceSpec,
) error {
	sameSource := previous != nil && desired != nil && previous.cachePath == desired.cachePath
	if previous != nil {
		for _, pluginID := range sortedPluginIDs(previous.plugins) {
			if sameSource {
				if _, remains := desired.plugins[pluginID]; remains {
					continue
				}
			}
			if err := transaction.runner.cli.RemovePlugin(ctx, home, pluginID); err != nil {
				return fmt.Errorf("remove plugin %s: %w", pluginID, err)
			}
		}
		if !sameSource {
			if err := transaction.runner.cli.RemoveMarketplace(ctx, home, previous.key.marketplace); err != nil {
				return fmt.Errorf("remove marketplace %s: %w", previous.key.marketplace, err)
			}
		}
	}
	if desired != nil {
		if !sameSource {
			if err := transaction.runner.cli.AddMarketplace(ctx, home, desired.key.marketplace, desired.cachePath); err != nil {
				return fmt.Errorf("add marketplace %s: %w", desired.key.marketplace, err)
			}
		}
		for _, pluginID := range sortedPluginIDs(desired.plugins) {
			if sameSource {
				if _, installed := previous.plugins[pluginID]; installed {
					continue
				}
			}
			if err := transaction.runner.cli.InstallPlugin(ctx, home, pluginID); err != nil {
				return fmt.Errorf("install plugin %s: %w", pluginID, err)
			}
		}
	}
	return nil
}

func (transaction *nativeInstallerTransaction) Rollback() error {
	if !transaction.mutated || transaction.finalized {
		return nil
	}
	base := transaction.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(base), nativeInstallerRollbackTimeout)
	defer cancel()
	var result error
	for _, instanceID := range transaction.instances {
		if err := transaction.reconcileInstance(ctx, instanceID, transaction.previous); err != nil {
			result = errors.Join(result, err)
		}
	}
	if result == nil {
		transaction.mutated = false
		transaction.committed = false
	}
	return result
}

func (transaction *nativeInstallerTransaction) reconcileInstance(
	ctx context.Context,
	instanceID string,
	target map[nativeMarketplaceKey]*nativeMarketplaceSpec,
) error {
	home, err := transaction.runner.ensureHome(instanceID)
	if err != nil {
		return err
	}
	state, err := transaction.runner.cli.Snapshot(ctx, home)
	if err != nil {
		return fmt.Errorf("inspect native plugin state during rollback for %s: %w", instanceID, err)
	}
	managedPaths := transaction.managedCachePaths(instanceID)
	affectedMarkets := transaction.affectedMarketplaces(instanceID)
	for marketplace, source := range cloneStringMap(state.marketplaces) {
		if _, managed := managedPaths[source]; !managed {
			continue
		}
		if _, expectedName := affectedMarkets[marketplace]; expectedName {
			continue
		}
		if len(nativePluginsForMarketplace(state, marketplace)) != 0 {
			return fmt.Errorf("cannot remove unexpected native marketplace %s/%s because it has installed plugins", instanceID, marketplace)
		}
		if err := transaction.runner.cli.RemoveMarketplace(ctx, home, marketplace); err != nil {
			return fmt.Errorf("remove unexpected native marketplace %s/%s: %w", instanceID, marketplace, err)
		}
		delete(state.marketplaces, marketplace)
	}

	for _, key := range transaction.keys {
		if key.instanceID != instanceID {
			continue
		}
		if err := transaction.restoreMarketplace(ctx, home, &state, key, target[key]); err != nil {
			return fmt.Errorf("restore native marketplace %s/%s: %w", instanceID, key.marketplace, err)
		}
	}
	verified, err := transaction.runner.cli.Snapshot(ctx, home)
	if err != nil {
		return fmt.Errorf("verify native plugin rollback for %s: %w", instanceID, err)
	}
	if err := transaction.validateState(instanceID, verified, target, "reconcile"); err != nil {
		return err
	}
	return nil
}

func (transaction *nativeInstallerTransaction) restoreMarketplace(
	ctx context.Context,
	home string,
	state *nativePluginState,
	key nativeMarketplaceKey,
	target *nativeMarketplaceSpec,
) error {
	managedPlugins := transaction.managedPluginIDs(key)
	currentSource, marketplaceExists := state.marketplaces[key.marketplace]
	sourceMatches := target != nil && marketplaceExists && currentSource == target.cachePath

	if target == nil || !sourceMatches {
		for _, pluginID := range sortedPluginIDs(nativePluginsForMarketplace(*state, key.marketplace)) {
			if _, managed := managedPlugins[pluginID]; !managed {
				continue
			}
			if err := transaction.runner.cli.RemovePlugin(ctx, home, pluginID); err != nil {
				return fmt.Errorf("remove plugin %s: %w", pluginID, err)
			}
			delete(state.plugins, pluginID)
		}
		remaining := nativePluginsForMarketplace(*state, key.marketplace)
		if marketplaceExists {
			if len(remaining) != 0 {
				return errors.New("marketplace has user-owned plugins; refusing to replace it")
			}
			if err := transaction.runner.cli.RemoveMarketplace(ctx, home, key.marketplace); err != nil {
				return fmt.Errorf("remove marketplace %s: %w", key.marketplace, err)
			}
			delete(state.marketplaces, key.marketplace)
		}
		if target == nil {
			return nil
		}
		if len(remaining) != 0 {
			return errors.New("plugin registrations collide with the marketplace restoration")
		}
		if err := transaction.runner.cli.AddMarketplace(ctx, home, key.marketplace, target.cachePath); err != nil {
			return fmt.Errorf("add marketplace %s: %w", key.marketplace, err)
		}
		state.marketplaces[key.marketplace] = target.cachePath
	}

	for _, pluginID := range sortedPluginIDs(nativePluginsForMarketplace(*state, key.marketplace)) {
		_, shouldExist := target.plugins[pluginID]
		if _, managed := managedPlugins[pluginID]; !managed || (shouldExist && state.plugins[pluginID]) {
			continue
		}
		if err := transaction.runner.cli.RemovePlugin(ctx, home, pluginID); err != nil {
			return fmt.Errorf("remove plugin %s: %w", pluginID, err)
		}
		delete(state.plugins, pluginID)
	}
	for _, pluginID := range sortedPluginIDs(target.plugins) {
		if state.plugins[pluginID] {
			continue
		}
		if err := transaction.runner.cli.InstallPlugin(ctx, home, pluginID); err != nil {
			return fmt.Errorf("install plugin %s: %w", pluginID, err)
		}
		state.plugins[pluginID] = true
	}
	return nil
}

func (transaction *nativeInstallerTransaction) Finalize() error {
	transaction.finalized = true
	transaction.mutated = false
	return nil
}

func (transaction *nativeInstallerTransaction) validateState(
	instanceID string,
	state nativePluginState,
	expected map[nativeMarketplaceKey]*nativeMarketplaceSpec,
	phase string,
) error {
	for _, key := range transaction.keys {
		if key.instanceID != instanceID {
			continue
		}
		spec := expected[key]
		actualSource, marketplaceExists := state.marketplaces[key.marketplace]
		actualPlugins := nativePluginsForMarketplace(state, key.marketplace)
		if spec == nil {
			if marketplaceExists || len(actualPlugins) != 0 {
				if phase == "baseline" {
					return fmt.Errorf("native marketplace %s/%s has a user-owned collision", instanceID, key.marketplace)
				}
				return fmt.Errorf("native marketplace %s/%s is present after %s", instanceID, key.marketplace, phase)
			}
			continue
		}
		if !marketplaceExists || actualSource != spec.cachePath {
			if phase == "baseline" {
				return fmt.Errorf("native marketplace %s/%s has drifted from its pinned cache", instanceID, key.marketplace)
			}
			return fmt.Errorf("native marketplace %s/%s does not use its pinned cache after %s", instanceID, key.marketplace, phase)
		}
		if len(actualPlugins) != len(spec.plugins) {
			return fmt.Errorf("native marketplace %s/%s has an unexpected plugin set during %s", instanceID, key.marketplace, phase)
		}
		for pluginID := range spec.plugins {
			enabled, exists := state.plugins[pluginID]
			if !exists || !enabled {
				return fmt.Errorf("native plugin %s is missing or disabled during %s", pluginID, phase)
			}
		}
	}
	return nil
}

func (transaction *nativeInstallerTransaction) managedPluginIDs(key nativeMarketplaceKey) map[string]struct{} {
	result := map[string]struct{}{}
	for _, spec := range []*nativeMarketplaceSpec{transaction.previous[key], transaction.desired[key]} {
		if spec == nil {
			continue
		}
		for pluginID := range spec.plugins {
			result[pluginID] = struct{}{}
		}
	}
	return result
}

func (transaction *nativeInstallerTransaction) managedCachePaths(instanceID string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, group := range []map[nativeMarketplaceKey]*nativeMarketplaceSpec{transaction.previous, transaction.desired} {
		for key, spec := range group {
			if key.instanceID == instanceID {
				result[spec.cachePath] = struct{}{}
			}
		}
	}
	return result
}

func (transaction *nativeInstallerTransaction) affectedMarketplaces(instanceID string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, key := range transaction.keys {
		if key.instanceID == instanceID {
			result[key.marketplace] = struct{}{}
		}
	}
	return result
}

func (runner *nativeMarketplaceInstallerRunner) ensureHome(instanceID string) (string, error) {
	logical := "/data/harnesses/" + instanceID + "/" + runner.driverDirectory
	home, err := physicalHarnessPath(runner.dataRoot, logical, instanceID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("create native plugin home for %s: %w", instanceID, err)
	}
	info, err := os.Lstat(home)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("native plugin home for %s is not a directory", instanceID)
	}
	return home, nil
}

func nativeMarketplaceSpecsEqual(left, right *nativeMarketplaceSpec) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if left.key != right.key || left.cachePath != right.cachePath || len(left.plugins) != len(right.plugins) {
		return false
	}
	for pluginID := range left.plugins {
		if _, exists := right.plugins[pluginID]; !exists {
			return false
		}
	}
	return true
}

func nativePluginsForMarketplace(state nativePluginState, marketplace string) map[string]struct{} {
	result := map[string]struct{}{}
	for pluginID := range state.plugins {
		separator := strings.LastIndex(pluginID, "@")
		if separator > 0 && pluginID[separator+1:] == marketplace {
			result[pluginID] = struct{}{}
		}
	}
	return result
}

func sortedPluginIDs(plugins map[string]struct{}) []string {
	result := make([]string, 0, len(plugins))
	for pluginID := range plugins {
		result = append(result, pluginID)
	}
	sort.Strings(result)
	return result
}

func cloneNativePluginState(state nativePluginState) nativePluginState {
	result := nativePluginState{
		marketplaces: cloneStringMap(state.marketplaces),
		plugins:      make(map[string]bool, len(state.plugins)),
	}
	for pluginID, enabled := range state.plugins {
		result.plugins[pluginID] = enabled
	}
	return result
}

func cloneStringMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
