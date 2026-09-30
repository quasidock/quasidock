package tree

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStripComponentsAndNormalize(t *testing.T) {
	s, ok := StripComponents("rg-15/bin/rg.exe", 1)
	if !ok || s != "bin/rg.exe" {
		t.Fatalf("expected bin/rg.exe, got %s (%v)", s, ok)
	}

	s, ok = StripComponents("rg.exe", 0)
	if !ok || s != "rg.exe" {
		t.Fatalf("expected rg.exe, got %s (%v)", s, ok)
	}

	_, ok = StripComponents("rg-15", 1)
	if ok {
		t.Fatalf("expected false for rg-15 strip 1")
	}

	_, ok = StripComponents("README", 1)
	if ok {
		t.Fatalf("expected false for README strip 1")
	}

	if NormalizePath("a/./b//c") != "a/b/c" {
		t.Fatalf("unexpected NormalizePath: %s", NormalizePath("a/./b//c"))
	}
	if NormalizePath("a\\b\\c") != "a/b/c" {
		t.Fatalf("unexpected NormalizePath: %s", NormalizePath("a\\b\\c"))
	}
}

func TestGlobMatchAndCleanupMatches(t *testing.T) {
	if !GlobMatch("*.a", "libz.a") {
		t.Error("expected *.a to match libz.a")
	}
	if GlobMatch("*.a", "libz.a.txt") {
		t.Error("expected *.a not to match libz.a.txt")
	}
	if !GlobMatch("lib*.dll", "LIBZ.DLL") {
		t.Error("expected lib*.dll to match LIBZ.DLL")
	}
	if !GlobMatch("a?c", "abc") {
		t.Error("expected a?c to match abc")
	}
	if GlobMatch("a?c", "ac") {
		t.Error("expected a?c not to match ac")
	}
	if !GlobMatch("*", "") {
		t.Error("expected * to match empty")
	}
	if !GlobMatch("*x*y*", "axbyc") {
		t.Error("expected *x*y* to match axbyc")
	}
	if GlobMatch("*x*y", "axbyc") {
		t.Error("expected *x*y not to match axbyc")
	}

	patterns := []string{"/include", "/lib/pkgconfig", "*.pdb", "__pycache__"}
	if !CleanupMatches(patterns, "include") {
		t.Error("expected include to match")
	}
	if !CleanupMatches(patterns, "Include/zlib.h") {
		t.Error("expected Include/zlib.h to match")
	}
	if CleanupMatches(patterns, "includes/x.h") {
		t.Error("expected includes/x.h not to match")
	}
	if !CleanupMatches(patterns, "lib/pkgconfig/zlib.pc") {
		t.Error("expected lib/pkgconfig/zlib.pc to match")
	}
	if CleanupMatches(patterns, "lib/libz.a") {
		t.Error("expected lib/libz.a not to match")
	}
	if !CleanupMatches(patterns, "bin/sqlite3.PDB") {
		t.Error("expected bin/sqlite3.PDB to match")
	}
	if !CleanupMatches(patterns, "lib/x/__pycache__/a.pyc") {
		t.Error("expected lib/x/__pycache__/a.pyc to match")
	}
	if CleanupMatches(patterns, "bin/sqlite3.exe") {
		t.Error("expected bin/sqlite3.exe not to match")
	}
}

func TestTreeAddOverlayAndCaseInsensitivity(t *testing.T) {
	tr := New()
	tmpDir := t.TempDir()

	f1 := filepath.Join(tmpDir, "file1.txt")
	os.WriteFile(f1, []byte("version1"), 0o644)

	f2 := filepath.Join(tmpDir, "file2.txt")
	os.WriteFile(f2, []byte("version2"), 0o644)

	// Add file under "Bin/App.exe"
	if err := tr.AddFile("Bin/App.exe", Content{Kind: ContentFile, FilePath: f1, FileSize: 8}); err != nil {
		t.Fatalf("AddFile failed: %v", err)
	}

	// Overwrite with different case: "bin/app.exe"
	if err := tr.AddFile("bin/app.exe", Content{Kind: ContentFile, FilePath: f2, FileSize: 8}); err != nil {
		t.Fatalf("AddFile overwrite failed: %v", err)
	}

	// Should preserve original casing: "Bin/App.exe"
	p, ok := tr.FilePath("bin/APP.EXE")
	if !ok || p != "Bin/App.exe" {
		t.Fatalf("expected preserved path Bin/App.exe, got %s (%v)", p, ok)
	}

	// Try adding dir where file exists -> conflict
	err := tr.AddDir("Bin/App.exe/sub")
	if !errors.Is(err, ErrPathConflict) {
		t.Fatalf("expected ErrPathConflict, got %v", err)
	}

	// Try adding file where dir exists -> conflict
	err = tr.AddFile("Bin", Content{Kind: ContentFile, FilePath: f1, FileSize: 8})
	if !errors.Is(err, ErrPathConflict) {
		t.Fatalf("expected ErrPathConflict, got %v", err)
	}
}

func TestTarSymlinkResolution(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Add target file
	tw.WriteHeader(&tar.Header{
		Name:     "bin/zstd.exe",
		Typeflag: tar.TypeReg,
		Mode:     0o755,
		Size:     10,
	})
	tw.Write([]byte("zstd-bytes"))

	// Add symlink to file
	tw.WriteHeader(&tar.Header{
		Name:     "bin/unzstd.exe",
		Typeflag: tar.TypeSymlink,
		Linkname: "zstd.exe",
	})

	// Add symlink to symlink
	tw.WriteHeader(&tar.Header{
		Name:     "bin/zcat.exe",
		Typeflag: tar.TypeSymlink,
		Linkname: "unzstd.exe",
	})
	tw.Close()

	reader := bytes.NewReader(buf.Bytes())
	entries, err := ListTar(reader, int64(buf.Len()))
	if err != nil {
		t.Fatalf("ListTar failed: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	for _, e := range entries {
		if e.IsDir {
			t.Errorf("entry %s should not be dir", e.Name)
		}
		if e.Size != 10 {
			t.Errorf("entry %s expected size 10, got %d", e.Name, e.Size)
		}
	}
}

func TestTreeWriteFilesAndSync(t *testing.T) {
	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "source.txt")
	os.WriteFile(srcFile, []byte("hello tree"), 0o644)

	tr := New()
	tr.AddFile("dir/sub/test.txt", Content{
		Kind:     ContentFile,
		FilePath: srcFile,
		FileSize: 10,
	})

	destDir := filepath.Join(tmpDir, "dest")
	if err := tr.WriteFiles(destDir); err != nil {
		t.Fatalf("WriteFiles failed: %v", err)
	}

	readBack, err := os.ReadFile(filepath.Join(destDir, "dir", "sub", "test.txt"))
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(readBack) != "hello tree" {
		t.Fatalf("content mismatch: %q", string(readBack))
	}

	// Test SyncFiles: should detect identical file and not rewrite it
	synced, err := tr.SyncFiles(destDir, []string{"dir/sub/test.txt", "obsolete.txt"})
	if err != nil {
		t.Fatalf("SyncFiles failed: %v", err)
	}
	if synced.Written != 0 {
		t.Fatalf("expected 0 written on unchanged sync, got %d", synced.Written)
	}

	// Test ContentHash & WriteTar
	var tarBuf bytes.Buffer
	if err := tr.WriteTar(&tarBuf); err != nil {
		t.Fatalf("WriteTar failed: %v", err)
	}
	_, hexDigest, err := tr.ContentHash()
	if err != nil {
		t.Fatalf("ContentHash failed: %v", err)
	}
	if hexDigest == "" {
		t.Fatalf("expected non-empty hex digest")
	}
}
