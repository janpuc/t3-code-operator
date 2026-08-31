package t3client

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectVersionReturnsOnlyTheValidatedVersion(t *testing.T) {
	binary := writeVersionCommand(t, "#!/bin/sh\nprintf 't3 v0.0.34\\n'\n")
	version, err := DetectVersion(context.Background(), binary)
	if err != nil {
		t.Fatal(err)
	}
	if version != "0.0.34" {
		t.Fatalf("unexpected t3 version %q", version)
	}
}

func TestDetectVersionRejectsUnexpectedOutput(t *testing.T) {
	binary := writeVersionCommand(t, "#!/bin/sh\nprintf 'secret-canary\\n'\n")
	version, err := DetectVersion(context.Background(), binary)
	if err == nil || version != "" || err.Error() == "secret-canary" {
		t.Fatalf("unsafe t3 version output was accepted or disclosed: version=%q err=%v", version, err)
	}
}

func writeVersionCommand(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t3")
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
