package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	t3v1alpha1 "github.com/janpuc/t3-code-operator/api/v1alpha1"
)

func TestParseMiseLockArtifact(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	lockfile := []byte(`lockfile_version = 1

[[tools."aqua:BurntSushi/ripgrep"]]
version = "14.1.1"
backend = "aqua:BurntSushi/ripgrep"
specifiers = ["14.1.1"]

[tools."aqua:BurntSushi/ripgrep"."platforms.linux-x64"]
checksum = "` + digest + `"
size = 123
url = "https://example.test/rg.tar.gz"
`)
	artifact, err := parseMiseLockArtifact(
		lockfile,
		"aqua:BurntSushi/ripgrep",
		"14.1.1",
		"linux-x64",
	)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Platform != "linux-x64" || artifact.URL != "https://example.test/rg.tar.gz" || artifact.SHA256 != digest || artifact.Size != 123 {
		t.Fatalf("unexpected artifact: %#v", artifact)
	}
}

func TestParseMiseLockArtifactNormalisesTheVersionPrefix(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	lockfile := []byte(`lockfile_version = 1

[[tools."github:koment-dev/koment"]]
version = "3.2.0"
backend = "github:koment-dev/koment"
specifiers = ["v3.2.0"]

[tools."github:koment-dev/koment"."platforms.linux-x64"]
checksum = "` + digest + `"
url = "https://example.test/koment.tar.gz"
`)
	artifact, err := parseMiseLockArtifact(
		lockfile,
		"github:koment-dev/koment",
		"v3.2.0",
		"linux-x64",
	)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.SHA256 != digest {
		t.Fatalf("unexpected artifact: %#v", artifact)
	}
	if _, err := parseMiseLockArtifact(
		lockfile,
		"github:koment-dev/koment",
		"v3.3.0",
		"linux-x64",
	); err == nil || !strings.Contains(err.Error(), "identity does not match") {
		t.Fatalf("a different version was accepted: %v", err)
	}
}

func TestMiseToolResolverReportsTheResolutionFailureCause(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "mise")
	script := `#!/bin/sh
cat > mise.lock <<'LOCK'
lockfile_version = 1

[[tools."github:koment-dev/koment"]]
version = "9.9.9"
backend = "github:koment-dev/koment"
specifiers = ["9.9.9"]

[tools."github:koment-dev/koment"."platforms.linux-x64"]
checksum = "sha256:` + strings.Repeat("d", 64) + `"
url = "https://example.test/koment.tar.gz"
LOCK
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	resolver := newTestMiseToolResolver(t, binary)
	_, err := resolver.Resolve(context.Background(), []t3v1alpha1.ToolSpec{{
		Name:    "koment",
		Backend: "github:koment-dev/koment",
		Version: "3.2.0",
	}})
	if err == nil || !strings.Contains(err.Error(), "identity does not match") {
		t.Fatalf("the resolution failure cause was not reported: %v", err)
	}
}

func TestMiseToolResolverAcceptsExplicitArtifactsWithoutMise(t *testing.T) {
	resolver := newTestMiseToolResolver(t, filepath.Join(t.TempDir(), "missing-mise"))
	tools, err := resolver.Resolve(context.Background(), []t3v1alpha1.ToolSpec{{
		Name:    "rg",
		Backend: "aqua:BurntSushi/ripgrep",
		Version: "14.1.1",
		Artifacts: []t3v1alpha1.ToolArtifactSpec{{
			Platform: "linux-x64",
			URL:      "https://example.test/rg.tar.gz",
			SHA256:   "sha256:" + strings.Repeat("b", 64),
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || len(tools[0].Artifacts) != 1 {
		t.Fatalf("explicit artifact was not resolved: %#v", tools)
	}
}

func TestMiseToolResolverValidatesBeforeExecutingMise(t *testing.T) {
	resolver := newTestMiseToolResolver(t, filepath.Join(t.TempDir(), "missing-mise"))
	_, err := resolver.Resolve(context.Background(), []t3v1alpha1.ToolSpec{{
		Name:    "rg",
		Backend: "aqua:BurntSushi/ripgrep",
		Version: "14.1.1",
		Options: map[string]string{"bad.key": "value"},
	}})
	if err == nil || !strings.Contains(err.Error(), "invalid option name") {
		t.Fatalf("unsafe option did not fail validation first: %v", err)
	}
}

func TestMiseToolResolverRequiresExplicitHTTPArtifacts(t *testing.T) {
	resolver := newTestMiseToolResolver(t, filepath.Join(t.TempDir(), "missing-mise"))
	_, err := resolver.Resolve(context.Background(), []t3v1alpha1.ToolSpec{{
		Name:    "custom",
		Backend: "http:custom",
		Version: "1.0.0",
	}})
	if err == nil || !strings.Contains(err.Error(), "http tools require pinned artifacts") {
		t.Fatalf("unpinned HTTP tool was accepted: %v", err)
	}
}

func TestMiseToolResolverIntegration(t *testing.T) {
	if os.Getenv("T3_TOOL_RESOLVER_INTEGRATION") != "1" {
		t.Skip("set T3_TOOL_RESOLVER_INTEGRATION=1 to run the real mise resolver test")
	}
	binary := os.Getenv("T3_MISE_BINARY")
	if binary == "" {
		binary = "mise"
	}
	resolver, err := NewMiseToolResolver(MiseToolResolverConfig{
		Binary:         binary,
		CacheDirectory: filepath.Join(t.TempDir(), "cache"),
		Platforms:      []string{"linux-x64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := resolver.Resolve(context.Background(), []t3v1alpha1.ToolSpec{{
		Name:    "rg",
		Backend: "aqua:BurntSushi/ripgrep",
		Version: "14.1.1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || len(tools[0].Artifacts) != 1 {
		t.Fatalf("mise did not resolve one artifact: %#v", tools)
	}
	artifact := tools[0].Artifacts[0]
	if artifact.Platform != "linux-x64" || artifact.URL == "" || artifact.SHA256 == "" {
		t.Fatalf("mise returned an incomplete artifact: %#v", artifact)
	}
}

func newTestMiseToolResolver(t *testing.T, binary string) *MiseToolResolver {
	t.Helper()
	resolver, err := NewMiseToolResolver(MiseToolResolverConfig{
		Binary:         binary,
		CacheDirectory: filepath.Join(t.TempDir(), "cache"),
		Platforms:      []string{"linux-x64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}
