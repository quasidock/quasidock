package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/store"
)

func TestFormatBytes(t *testing.T) {
	if s := formatBytes(500); s != "500 B" {
		t.Fatalf("expected '500 B', got %q", s)
	}
	if s := formatBytes(1024); !strings.HasPrefix(s, "1.0 KiB") {
		t.Fatalf("expected '1.0 KiB', got %q", s)
	}
	if s := formatBytes(1048576); !strings.HasPrefix(s, "1.0 MiB") {
		t.Fatalf("expected '1.0 MiB', got %q", s)
	}
}

func TestTreeSize(t *testing.T) {
	tmpDir := t.TempDir()
	file1 := filepath.Join(tmpDir, "f1.txt")
	file2 := filepath.Join(tmpDir, "sub", "f2.txt")
	_ = os.MkdirAll(filepath.Dir(file2), 0755)

	_ = os.WriteFile(file1, []byte("hello"), 0644)
	_ = os.WriteFile(file2, []byte("world!"), 0644)

	sz, err := treeSize(tmpDir)
	if err != nil {
		t.Fatalf("treeSize error: %v", err)
	}
	if sz != 11 {
		t.Fatalf("expected 11 bytes, got %d", sz)
	}
}

func TestIsInstalledAndManifest(t *testing.T) {
	refs := []store.Ref{
		{ID: "app1", Manifest: "sha256:111"},
		{ID: "app2", Manifest: "sha256:222"},
	}

	if !isInstalled(refs, "APP1") {
		t.Fatal("expected APP1 to match case-insensitively")
	}
	if isInstalled(refs, "app3") {
		t.Fatal("expected app3 not to be installed")
	}

	if !isInstalledManifest(refs, "sha256:111") {
		t.Fatal("expected sha256:111 to match")
	}
	if isInstalledManifest(refs, "sha256:333") {
		t.Fatal("expected sha256:333 not to match")
	}
}
