package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/store"
	"github.com/zigsaw-io/zigsaw/internal/version"
)

func TestPassesOver(t *testing.T) {
	skip := &store.Skip{Version: "2", Manifest: "sha256:bb"}
	if res := PassesOver(skip, "sha256:bb"); res == nil || res.Version != "2" {
		t.Fatalf("expected skip with version 2, got %v", res)
	}
	if res := PassesOver(skip, "sha256:cc"); res != nil {
		t.Fatalf("expected nil for different manifest, got %v", res)
	}
	if res := PassesOver(nil, "sha256:bb"); res != nil {
		t.Fatalf("expected nil for nil skip, got %v", res)
	}
}

func TestIsNewer(t *testing.T) {
	if !IsNewer("999.0.0") {
		t.Fatal("expected 999.0.0 to be newer")
	}
	if IsNewer(version.Version) {
		t.Fatalf("expected current version %s not to be newer", version.Version)
	}
	if IsNewer("0.1.0") {
		t.Fatal("expected 0.1.0 not to be newer")
	}
	if IsNewer("not a version") {
		t.Fatal("expected invalid version string not to be newer")
	}
}

func TestListedHash(t *testing.T) {
	a := strings.Repeat("a", 64)
	b := strings.Repeat("B", 64)
	archive := "zigsaw-x86_64-windows.zip"
	sums := a + "  install.ps1\n" + b + " *" + archive + "\r\n"

	hArchive := ListedHash(sums, archive)
	if hArchive == nil || *hArchive != strings.Repeat("b", 64) {
		t.Fatalf("expected %s, got %v", strings.Repeat("b", 64), hArchive)
	}

	hInstall := ListedHash(sums, "install.ps1")
	if hInstall == nil || *hInstall != a {
		t.Fatalf("expected %s, got %v", a, hInstall)
	}

	if hOther := ListedHash(sums, "other"); hOther != nil {
		t.Fatalf("expected nil for 'other', got %v", hOther)
	}

	if hShort := ListedHash("abc  "+archive, archive); hShort != nil {
		t.Fatalf("expected nil for short hex, got %v", hShort)
	}

	if hNonHex := ListedHash(strings.Repeat("z", 64)+"  "+archive, archive); hNonHex != nil {
		t.Fatalf("expected nil for non-hex, got %v", hNonHex)
	}
}

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
