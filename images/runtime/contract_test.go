package runtimeimage_test

import (
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

type runtimePackage struct {
	Dependencies map[string]string `json:"dependencies"`
}

type runtimePackageLock struct {
	Packages map[string]struct {
		Version   string            `json:"version"`
		Resolved  string            `json:"resolved"`
		Integrity string            `json:"integrity"`
		Depends   map[string]string `json:"dependencies"`
	} `json:"packages"`
}

func TestRuntimeNPMLocksPinEveryDirectPackage(t *testing.T) {
	channels := map[string]string{
		"stable":  "0.0.34",
		"nightly": "0.0.36-nightly.20260828.1209",
	}
	for channel, expectedT3 := range channels {
		t.Run(channel, func(t *testing.T) {
			var specification runtimePackage
			readJSON(t, "npm/"+channel+"/package.json", &specification)
			var lock runtimePackageLock
			readJSON(t, "npm/"+channel+"/package-lock.json", &lock)
			if specification.Dependencies["t3"] != expectedT3 {
				t.Fatalf("unexpected t3 pin: %q", specification.Dependencies["t3"])
			}
			root := lock.Packages[""]
			for name, version := range specification.Dependencies {
				if root.Depends[name] != version {
					t.Fatalf("root lock does not pin %s@%s", name, version)
				}
				locked, exists := lock.Packages["node_modules/"+name]
				if !exists || locked.Version != version {
					t.Fatalf("lock does not resolve %s@%s: %#v", name, version, locked)
				}
				parsed, err := url.Parse(locked.Resolved)
				if err != nil || parsed.Scheme != "https" || parsed.Host == "" || locked.Integrity == "" {
					t.Fatalf("lock has no trusted source for %s: %#v", name, locked)
				}
			}
		})
	}
}

func TestBaselineMiseLockPinsBothImageArchitectures(t *testing.T) {
	var configuration struct {
		Tools map[string]string `toml:"tools"`
	}
	readTOML(t, "baseline/mise.toml", &configuration)
	if len(configuration.Tools) != 17 {
		t.Fatalf("unexpected fixed baseline size: %d", len(configuration.Tools))
	}
	for name, version := range configuration.Tools {
		if version == "" || strings.EqualFold(version, "latest") {
			t.Fatalf("baseline tool %s is not pinned: %q", name, version)
		}
	}

	var lock struct {
		Tools map[string][]map[string]any `toml:"tools"`
	}
	readTOML(t, "baseline/mise.lock", &lock)
	if len(lock.Tools) != len(configuration.Tools) {
		t.Fatalf("baseline lock has %d tools, want %d", len(lock.Tools), len(configuration.Tools))
	}
	sha256Pattern := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	for name, version := range configuration.Tools {
		entries := lock.Tools[name]
		if len(entries) != 1 {
			t.Fatalf("baseline tool %s has %d lock entries", name, len(entries))
		}
		lockedVersion, _ := entries[0]["version"].(string)
		if strings.TrimPrefix(version, "v") != lockedVersion {
			t.Fatalf("baseline tool %s lock version is %q, want %q", name, lockedVersion, version)
		}
		for _, platform := range []string{"linux-arm64", "linux-x64"} {
			artifact, ok := entries[0]["platforms."+platform].(map[string]any)
			if !ok {
				t.Fatalf("baseline tool %s has no %s artifact", name, platform)
			}
			rawURL, _ := artifact["url"].(string)
			checksum, _ := artifact["checksum"].(string)
			parsed, err := url.Parse(rawURL)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" || !sha256Pattern.MatchString(checksum) {
				t.Fatalf("baseline tool %s has an invalid %s artifact: %#v", name, platform, artifact)
			}
		}
	}
}

func TestDockerfilePinsSourcesAndRunsAsNonRoot(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(raw)
	for _, expected := range []string{
		"ARG GO_IMAGE=golang:1.26.7-bookworm@sha256:",
		"ARG NODE_IMAGE=node:24.10.0-bookworm-slim@sha256:",
		"ARG T3_VERSION=0.0.34",
		"ARG CODEX_VERSION=0.149.0",
		"ARG CLAUDE_VERSION=2.1.241",
		"ARG CURSOR_VERSION=2026.08.11-e8db854",
		"ARG GROK_VERSION=1.0.5",
		"ARG OPENCODE_VERSION=1.18.21",
		"ARG GH_VERSION=2.98.0",
		"UpstreamT3Version=${T3_VERSION}",
		"ln -sfn /data/t3-coded/machine-info /etc/machine-info",
		"XDG_CONFIG_HOME=/data/home/.config",
		"GH_CONFIG_DIR=/data/t3-coded/gh",
		"-o /out/t3-smbd ./cmd/t3-smbd",
		"samba tini",
		"COPY --from=go-builder /out/t3-smbd /usr/local/bin/t3-smbd",
		"COPY images/runtime/verify-smb-runtime.sh /usr/local/bin/verify-t3-smb-runtime",
		"verify-t3-smb-runtime",
		"USER node",
	} {
		if !strings.Contains(dockerfile, expected) {
			t.Fatalf("Dockerfile lacks %q", expected)
		}
	}
	for _, forbidden := range []string{"@latest", "curl | bash", "curl|bash"} {
		if strings.Contains(dockerfile, forbidden) {
			t.Fatalf("Dockerfile contains moving installer input %q", forbidden)
		}
	}
}

func TestRuntimeVerifierChecksSMBBinaries(t *testing.T) {
	raw, err := os.ReadFile("verify-runtime.sh")
	if err != nil {
		t.Fatal(err)
	}
	verifier := string(raw)
	for _, expected := range []string{
		"test -x /usr/local/bin/t3-smbd",
		"test -x /usr/sbin/smbd",
		"test -x /usr/bin/smbpasswd",
		"test -x /usr/bin/net",
	} {
		if !strings.Contains(verifier, expected) {
			t.Fatalf("runtime verifier lacks %q", expected)
		}
	}
}

func TestSMBRuntimeVerifierStartsTheNativeServer(t *testing.T) {
	raw, err := os.ReadFile("verify-smb-runtime.sh")
	if err != nil {
		t.Fatal(err)
	}
	verifier := string(raw)
	for _, expected := range []string{
		"/usr/local/bin/t3-smbd",
		"--server-identity runtime-image-contract",
		"socket.create_connection",
		"/usr/bin/net --configfile=",
		"getlocalsid",
	} {
		if !strings.Contains(verifier, expected) {
			t.Fatalf("SMB runtime verifier lacks %q", expected)
		}
	}
}

func TestImageVersionSourcesStaySynchronized(t *testing.T) {
	var stable runtimePackage
	readJSON(t, "npm/stable/package.json", &stable)
	var nightly runtimePackage
	readJSON(t, "npm/nightly/package.json", &nightly)
	dockerfileRaw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(dockerfileRaw)
	bakeRaw, err := os.ReadFile("../../docker-bake.hcl")
	if err != nil {
		t.Fatal(err)
	}
	bake := string(bakeRaw)

	if got := dockerArgument(t, dockerfile, "T3_CHANNEL"); got != "stable" {
		t.Fatalf("Dockerfile defaults to T3 channel %q", got)
	}
	if got, want := dockerArgument(t, dockerfile, "T3_VERSION"), stable.Dependencies["t3"]; got != want {
		t.Fatalf("Dockerfile T3 version is %q, want stable lock %q", got, want)
	}
	for argument, dependency := range map[string]string{
		"CODEX_VERSION":    "@openai/codex",
		"CLAUDE_VERSION":   "@anthropic-ai/claude-code",
		"OPENCODE_VERSION": "opencode-ai",
	} {
		got := dockerArgument(t, dockerfile, argument)
		if got != stable.Dependencies[dependency] || got != nightly.Dependencies[dependency] {
			t.Fatalf("%s is %q, but channel locks have stable=%q nightly=%q", argument, got, stable.Dependencies[dependency], nightly.Dependencies[dependency])
		}
	}
	for channel, specification := range map[string]runtimePackage{"stable": stable, "nightly": nightly} {
		if got := bakeArgument(t, bake, channel, "T3_CHANNEL"); got != channel {
			t.Fatalf("bake target %s selects channel %q", channel, got)
		}
		if got, want := bakeArgument(t, bake, channel, "T3_VERSION"), specification.Dependencies["t3"]; got != want {
			t.Fatalf("bake target %s pins T3 %q, want %q", channel, got, want)
		}
	}

	var baseline struct {
		Tools map[string]string `toml:"tools"`
	}
	readTOML(t, "baseline/mise.toml", &baseline)
	if got, want := dockerArgument(t, dockerfile, "GH_VERSION"), strings.TrimPrefix(baseline.Tools["github:cli/cli"], "v"); got != want {
		t.Fatalf("Dockerfile gh contract is %q, want baseline %q", got, want)
	}
}

func dockerArgument(t *testing.T, dockerfile, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^ARG ` + regexp.QuoteMeta(name) + `=([^[:space:]]+)$`)
	matches := pattern.FindAllStringSubmatch(dockerfile, -1)
	if len(matches) == 0 {
		t.Fatalf("Dockerfile lacks ARG %s", name)
	}
	value := matches[0][1]
	for _, match := range matches[1:] {
		if match[1] != value {
			t.Fatalf("Dockerfile ARG %s has conflicting values %q and %q", name, value, match[1])
		}
	}
	return value
}

func bakeArgument(t *testing.T, bake, target, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?s)target "` + regexp.QuoteMeta(target) + `" \{.*?` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]+)"`)
	match := pattern.FindStringSubmatch(bake)
	if len(match) != 2 {
		t.Fatalf("bake target %s lacks %s", target, name)
	}
	return match[1]
}

func readJSON(t *testing.T, path string, output any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, output); err != nil {
		t.Fatal(err)
	}
}

func readTOML(t *testing.T, path string, output any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := toml.Unmarshal(raw, output); err != nil {
		t.Fatal(err)
	}
}
