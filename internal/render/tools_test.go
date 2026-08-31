package render

import (
	"strings"
	"testing"
)

func TestRenderToolsPinsArtifactsAndSorts(t *testing.T) {
	manifest, err := Render(ResolvedWorkstation{
		Namespace: "agents",
		Name:      "primary",
		UID:       "workstation-uid",
		Tools: []ResolvedTool{
			{
				Name:    "zeta",
				Backend: "github:example/zeta",
				Version: "v2.0.0",
				Artifacts: []ToolArtifact{
					toolArtifact("macos-arm64", "zeta-macos.tar.gz", "b"),
					toolArtifact("linux-x64", "zeta-linux.tar.gz", "a"),
				},
			},
			{
				Name:      "alpha",
				Backend:   "aqua:example/alpha",
				Version:   "1.2.3",
				Options:   map[string]string{"matching": "musl"},
				Artifacts: []ToolArtifact{toolArtifact("linux-x64", "alpha.tar.gz", "c")},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Tools) != 2 || manifest.Tools[0].Name != "alpha" || manifest.Tools[1].Name != "zeta" {
		t.Fatalf("tools were not sorted: %#v", manifest.Tools)
	}
	if manifest.Tools[1].Artifacts[0].Platform != "linux-x64" {
		t.Fatalf("artifacts were not sorted: %#v", manifest.Tools[1].Artifacts)
	}
	if manifest.Tools[0].Apply != toolApplyPolicy() {
		t.Fatalf("unexpected tool apply policy: %#v", manifest.Tools[0].Apply)
	}
	if err := VerifyManifest(manifest); err != nil {
		t.Fatal(err)
	}
}

func TestRenderRejectsUnresolvedOrUnsafeTools(t *testing.T) {
	tests := []struct {
		name string
		tool ResolvedTool
		want string
	}{
		{
			name: "backend without full lock support",
			tool: ResolvedTool{Name: "go", Backend: "go:example/tool", Version: "1.2.3", Artifacts: []ToolArtifact{toolArtifact("linux-x64", "go.tar.gz", "a")}},
			want: "backend",
		},
		{
			name: "latest version",
			tool: ResolvedTool{Name: "rg", Backend: "aqua:BurntSushi/ripgrep", Version: "latest", Artifacts: []ToolArtifact{toolArtifact("linux-x64", "rg.tar.gz", "a")}},
			want: "version",
		},
		{
			name: "missing artifact",
			tool: ResolvedTool{Name: "rg", Backend: "aqua:BurntSushi/ripgrep", Version: "14.1.1"},
			want: "artifacts",
		},
		{
			name: "credential-like URL",
			tool: ResolvedTool{
				Name:    "rg",
				Backend: "aqua:BurntSushi/ripgrep",
				Version: "14.1.1",
				Artifacts: []ToolArtifact{{
					Platform: "linux-x64",
					URL:      "https://example.test/rg.tar.gz?token=value",
					SHA256:   "sha256:" + strings.Repeat("a", 64),
				}},
			},
			want: "url",
		},
		{
			name: "sensitive option",
			tool: ResolvedTool{
				Name:      "rg",
				Backend:   "aqua:BurntSushi/ripgrep",
				Version:   "14.1.1",
				Options:   map[string]string{"token": "value"},
				Artifacts: []ToolArtifact{toolArtifact("linux-x64", "rg.tar.gz", "a")},
			},
			want: "sensitive",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Render(ResolvedWorkstation{
				Namespace: "agents",
				Name:      "primary",
				UID:       "workstation-uid",
				Tools:     []ResolvedTool{test.tool},
			})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func toolArtifact(platform, file, digestCharacter string) ToolArtifact {
	return ToolArtifact{
		Platform: platform,
		URL:      "https://example.test/" + file,
		SHA256:   "sha256:" + strings.Repeat(digestCharacter, 64),
	}
}
