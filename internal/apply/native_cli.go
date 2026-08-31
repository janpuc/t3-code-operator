package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	nativeCLIOutputLimit           = 256 << 10
	nativeCLIErrorLimit            = 32 << 10
	defaultNativeCLICommandTimeout = 2 * time.Minute
)

type nativeCLIExecutor struct {
	binary      string
	homeEnvName string
	timeout     time.Duration
}

func (executor nativeCLIExecutor) output(
	ctx context.Context,
	home string,
	arguments ...string,
) ([]byte, error) {
	timeout := executor.timeout
	if timeout <= 0 {
		timeout = defaultNativeCLICommandTimeout
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandContext, executor.binary, arguments...)
	command.Dir = home
	command.Env = nativeCLIEnvironment(executor.homeEnvName, home)
	command.Stdin = strings.NewReader("")
	standardOutput := &limitedNativeCLIBuffer{limit: nativeCLIOutputLimit}
	standardError := &limitedNativeCLIBuffer{limit: nativeCLIErrorLimit}
	command.Stdout = standardOutput
	command.Stderr = standardError
	if err := command.Run(); err != nil {
		if ctxErr := commandContext.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		message := strings.TrimSpace(standardError.String())
		if message == "" {
			message = strings.TrimSpace(standardOutput.String())
		}
		if message == "" {
			return nil, fmt.Errorf("native plugin command failed: %w", err)
		}
		return nil, fmt.Errorf("native plugin command failed: %w: %s", err, message)
	}
	if standardOutput.truncated {
		return nil, errors.New("native plugin command output exceeded its size limit")
	}
	return append([]byte(nil), standardOutput.Bytes()...), nil
}

func nativeCLIEnvironment(homeEnvironmentName, home string) []string {
	result := make([]string, 0, 10)
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR", "TMPDIR"} {
		if value, exists := os.LookupEnv(name); exists {
			result = append(result, name+"="+value)
		}
	}
	return append(result,
		homeEnvironmentName+"="+home,
		"CI=1",
		"NO_COLOR=1",
		"TERM=dumb",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	)
}

type limitedNativeCLIBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *limitedNativeCLIBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := buffer.limit - buffer.Len()
	if len(value) > remaining {
		buffer.truncated = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	_, _ = buffer.Buffer.Write(value)
	return originalLength, nil
}

type codexNativePluginCLI struct {
	executor nativeCLIExecutor
}

func (cli *codexNativePluginCLI) Snapshot(ctx context.Context, home string) (nativePluginState, error) {
	marketplaces, err := cli.executor.output(ctx, home, "plugin", "marketplace", "list", "--json")
	if err != nil {
		return nativePluginState{}, fmt.Errorf("list Codex marketplaces: %w", err)
	}
	plugins, err := cli.executor.output(ctx, home, "plugin", "list", "--json")
	if err != nil {
		return nativePluginState{}, fmt.Errorf("list Codex plugins: %w", err)
	}
	return parseCodexPluginState(marketplaces, plugins)
}

func (cli *codexNativePluginCLI) AddMarketplace(
	ctx context.Context,
	home string,
	marketplace string,
	source string,
) error {
	raw, err := cli.executor.output(ctx, home, "plugin", "marketplace", "add", source, "--json")
	if err != nil {
		return err
	}
	var result struct {
		MarketplaceName string `json:"marketplaceName"`
		InstalledRoot   string `json:"installedRoot"`
		AlreadyAdded    bool   `json:"alreadyAdded"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("parse Codex marketplace add result: %w", err)
	}
	if result.AlreadyAdded || result.MarketplaceName != marketplace || !nativeSourcesEqual(result.InstalledRoot, source) {
		return fmt.Errorf("Codex added marketplace %q from an unexpected source", result.MarketplaceName)
	}
	return nil
}

func (cli *codexNativePluginCLI) RemoveMarketplace(ctx context.Context, home, marketplace string) error {
	_, err := cli.executor.output(ctx, home, "plugin", "marketplace", "remove", marketplace, "--json")
	return err
}

func (cli *codexNativePluginCLI) InstallPlugin(ctx context.Context, home, pluginID string) error {
	raw, err := cli.executor.output(ctx, home, "plugin", "add", pluginID, "--json")
	if err != nil {
		return err
	}
	var result struct {
		PluginID string `json:"pluginId"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("parse Codex plugin add result: %w", err)
	}
	if result.PluginID != pluginID {
		return fmt.Errorf("Codex installed unexpected plugin %q", result.PluginID)
	}
	return nil
}

func (cli *codexNativePluginCLI) RemovePlugin(ctx context.Context, home, pluginID string) error {
	raw, err := cli.executor.output(ctx, home, "plugin", "remove", pluginID, "--json")
	if err != nil {
		return err
	}
	var result struct {
		PluginID string `json:"pluginId"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("parse Codex plugin remove result: %w", err)
	}
	if result.PluginID != pluginID {
		return fmt.Errorf("Codex removed unexpected plugin %q", result.PluginID)
	}
	return nil
}

type claudeNativePluginCLI struct {
	executor nativeCLIExecutor
}

func (cli *claudeNativePluginCLI) Snapshot(ctx context.Context, home string) (nativePluginState, error) {
	marketplaces, err := cli.executor.output(ctx, home, "plugin", "marketplace", "list", "--json")
	if err != nil {
		return nativePluginState{}, fmt.Errorf("list Claude marketplaces: %w", err)
	}
	plugins, err := cli.executor.output(ctx, home, "plugin", "list", "--json")
	if err != nil {
		return nativePluginState{}, fmt.Errorf("list Claude plugins: %w", err)
	}
	return parseClaudePluginState(marketplaces, plugins)
}

func (cli *claudeNativePluginCLI) AddMarketplace(
	ctx context.Context,
	home string,
	marketplace string,
	source string,
) error {
	if _, err := cli.executor.output(ctx, home, "plugin", "marketplace", "add", source, "--scope", "user"); err != nil {
		return err
	}
	state, err := cli.Snapshot(ctx, home)
	if err != nil {
		return err
	}
	if !nativeSourcesEqual(state.marketplaces[marketplace], source) {
		return fmt.Errorf("Claude did not add marketplace %s from its pinned source", marketplace)
	}
	return nil
}

func (cli *claudeNativePluginCLI) RemoveMarketplace(ctx context.Context, home, marketplace string) error {
	_, err := cli.executor.output(ctx, home, "plugin", "marketplace", "remove", marketplace, "--scope", "user")
	return err
}

func (cli *claudeNativePluginCLI) InstallPlugin(ctx context.Context, home, pluginID string) error {
	if _, err := cli.executor.output(ctx, home, "plugin", "install", pluginID, "--scope", "user", "--yes"); err != nil {
		return err
	}
	state, err := cli.Snapshot(ctx, home)
	if err != nil {
		return err
	}
	if !state.plugins[pluginID] {
		return fmt.Errorf("Claude did not enable plugin %s", pluginID)
	}
	return nil
}

func (cli *claudeNativePluginCLI) RemovePlugin(ctx context.Context, home, pluginID string) error {
	_, err := cli.executor.output(
		ctx,
		home,
		"plugin", "uninstall", pluginID, "--scope", "user", "--yes", "--keep-data",
	)
	return err
}

func parseCodexPluginState(marketplaceJSON, pluginJSON []byte) (nativePluginState, error) {
	state := nativePluginState{marketplaces: map[string]string{}, plugins: map[string]bool{}}
	var marketplaceList struct {
		Marketplaces []struct {
			Name              string `json:"name"`
			Root              string `json:"root"`
			MarketplaceSource struct {
				Source string `json:"source"`
			} `json:"marketplaceSource"`
		} `json:"marketplaces"`
	}
	if err := json.Unmarshal(marketplaceJSON, &marketplaceList); err != nil {
		return nativePluginState{}, fmt.Errorf("parse Codex marketplace list: %w", err)
	}
	for _, marketplace := range marketplaceList.Marketplaces {
		source := marketplace.Root
		if source == "" {
			source = marketplace.MarketplaceSource.Source
		}
		if marketplace.Name == "" || source == "" {
			return nativePluginState{}, errors.New("Codex marketplace list contains an incomplete entry")
		}
		if _, exists := state.marketplaces[marketplace.Name]; exists {
			return nativePluginState{}, fmt.Errorf("Codex marketplace %s appears more than once", marketplace.Name)
		}
		state.marketplaces[marketplace.Name] = normalizeNativeSource(source)
	}
	var pluginList struct {
		Installed []struct {
			PluginID  string `json:"pluginId"`
			Installed bool   `json:"installed"`
			Enabled   bool   `json:"enabled"`
		} `json:"installed"`
	}
	if err := json.Unmarshal(pluginJSON, &pluginList); err != nil {
		return nativePluginState{}, fmt.Errorf("parse Codex plugin list: %w", err)
	}
	for _, plugin := range pluginList.Installed {
		if !plugin.Installed {
			continue
		}
		if plugin.PluginID == "" {
			return nativePluginState{}, errors.New("Codex plugin list contains an incomplete entry")
		}
		if _, exists := state.plugins[plugin.PluginID]; exists {
			return nativePluginState{}, fmt.Errorf("Codex plugin %s appears more than once", plugin.PluginID)
		}
		state.plugins[plugin.PluginID] = plugin.Enabled
	}
	return state, nil
}

func parseClaudePluginState(marketplaceJSON, pluginJSON []byte) (nativePluginState, error) {
	state := nativePluginState{marketplaces: map[string]string{}, plugins: map[string]bool{}}
	var marketplaces []struct {
		Name            string `json:"name"`
		Path            string `json:"path"`
		InstallLocation string `json:"installLocation"`
	}
	if err := json.Unmarshal(marketplaceJSON, &marketplaces); err != nil {
		return nativePluginState{}, fmt.Errorf("parse Claude marketplace list: %w", err)
	}
	for _, marketplace := range marketplaces {
		source := marketplace.Path
		if source == "" {
			source = marketplace.InstallLocation
		}
		if marketplace.Name == "" || source == "" {
			return nativePluginState{}, errors.New("Claude marketplace list contains an incomplete entry")
		}
		if _, exists := state.marketplaces[marketplace.Name]; exists {
			return nativePluginState{}, fmt.Errorf("Claude marketplace %s appears more than once", marketplace.Name)
		}
		state.marketplaces[marketplace.Name] = normalizeNativeSource(source)
	}
	var plugins []struct {
		ID      string `json:"id"`
		Scope   string `json:"scope"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(pluginJSON, &plugins); err != nil {
		return nativePluginState{}, fmt.Errorf("parse Claude plugin list: %w", err)
	}
	for _, plugin := range plugins {
		if plugin.Scope != "" && plugin.Scope != "user" {
			continue
		}
		if plugin.ID == "" {
			return nativePluginState{}, errors.New("Claude plugin list contains an incomplete entry")
		}
		if _, exists := state.plugins[plugin.ID]; exists {
			return nativePluginState{}, fmt.Errorf("Claude plugin %s appears more than once", plugin.ID)
		}
		state.plugins[plugin.ID] = plugin.Enabled
	}
	return state, nil
}

func normalizeNativeSource(source string) string {
	if filepath.IsAbs(source) {
		return filepath.Clean(source)
	}
	return source
}

func nativeSourcesEqual(left, right string) bool {
	return normalizeNativeSource(left) == normalizeNativeSource(right)
}
