package format

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayerRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "layer.tar")
	destPath := filepath.Join(tmpDir, "out")

	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatalf("Create tar failed: %v", err)
	}

	w := NewLayerWriter(f)
	if err := w.AddDir("bin"); err != nil {
		t.Fatalf("AddDir failed: %v", err)
	}

	content := []byte("MZ fake")
	if err := w.AddFile("bin/tool.exe", int64(len(content)), bytes.NewReader(content)); err != nil {
		t.Fatalf("AddFile failed: %v", err)
	}
	if err := w.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	f.Close()

	if err := Extract(tarPath, destPath); err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destPath, "bin", "tool.exe"))
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("expected %q, got %q", content, got)
	}
}

func TestGzipDeterministicAndGunzip(t *testing.T) {
	// Verify gzip writer produces deterministic output
	var buf1, buf2 bytes.Buffer
	gw1, err := GzipWriter(&buf1)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello deterministic world")
	gw1.Write(data)
	gw1.Close()

	gw2, err := GzipWriter(&buf2)
	if err != nil {
		t.Fatal(err)
	}
	gw2.Write(data)
	gw2.Close()

	if !bytes.Equal(buf1.Bytes(), buf2.Bytes()) {
		t.Fatal("gzip outputs differ")
	}

	// Verify header starts with 1f 8b 08 00 00 00 00 00
	b := buf1.Bytes()
	if len(b) < 10 {
		t.Fatalf("gzip too short: %d", len(b))
	}
	expectedHeader := []byte{0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0}
	if !bytes.Equal(b[:8], expectedHeader) {
		t.Fatalf("unexpected gzip header: %x", b[:8])
	}
}

func TestGunzipMultiMemberAndPadding(t *testing.T) {
	first := bytes.Repeat([]byte("first member data "), 100)
	second := bytes.Repeat([]byte("second member data "), 100)

	var members bytes.Buffer
	for _, chunk := range [][]byte{first, second} {
		gw, err := GzipWriter(&members)
		if err != nil {
			t.Fatal(err)
		}
		gw.Write(chunk)
		gw.Close()
	}

	want := append(first, second...)

	// Test with no padding, 1 zero byte, 1000 zero bytes
	for _, padding := range [][]byte{nil, {0}, bytes.Repeat([]byte{0}, 1000)} {
		input := append(members.Bytes(), padding...)
		var out bytes.Buffer
		if err := Gunzip(bytes.NewReader(input), &out); err != nil {
			t.Fatalf("Gunzip failed with padding len %d: %v", len(padding), err)
		}
		if !bytes.Equal(out.Bytes(), want) {
			t.Fatalf("unexpected decompressed content with padding len %d", len(padding))
		}
	}

	// Test trailing non-zero garbage
	for _, garbage := range [][]byte{[]byte("x"), {0, 0, 'x'}, {0x1f}} {
		input := append(members.Bytes(), garbage...)
		var out bytes.Buffer
		err := Gunzip(bytes.NewReader(input), &out)
		if !errors.Is(err, ErrGzipTrailingGarbage) {
			t.Fatalf("expected ErrGzipTrailingGarbage, got: %v", err)
		}
	}
}

func TestGzipLayerInflateExtract(t *testing.T) {
	tmpDir := t.TempDir()
	gzPath := filepath.Join(tmpDir, "layer.tar.gz")
	tarPath := filepath.Join(tmpDir, "layer.tar")
	destPath := filepath.Join(tmpDir, "out")

	// Write tar.gz layer
	gzFile, err := os.Create(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := GzipWriter(gzFile)
	if err != nil {
		t.Fatal(err)
	}
	lw := NewLayerWriter(gw)
	if err := lw.AddDir("bin"); err != nil {
		t.Fatal(err)
	}
	toolContent := "MZ " + strings.Repeat("fake ", 1000)
	if err := lw.AddFile("bin/tool.exe", int64(len(toolContent)), strings.NewReader(toolContent)); err != nil {
		t.Fatal(err)
	}
	if err := lw.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	gzFile.Close()

	// Inflate
	if err := Inflate(gzPath, tarPath); err != nil {
		t.Fatalf("Inflate failed: %v", err)
	}

	// Extract
	if err := Extract(tarPath, destPath); err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destPath, "bin", "tool.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != toolContent {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(toolContent))
	}
}

func TestTarBadPathsAndSymlinks(t *testing.T) {
	tmpDir := t.TempDir()

	// Symlink test
	tarPath := filepath.Join(tmpDir, "symlink.tar")
	f, _ := os.Create(tarPath)
	tw := NewLayerWriter(f)
	hdr := &tar.Header{
		Name:     "link",
		Typeflag: tar.TypeSymlink,
		Linkname: "target",
	}
	tw.tw.WriteHeader(hdr)
	tw.Finish()
	f.Close()

	err := Extract(tarPath, filepath.Join(tmpDir, "out1"))
	if !errors.Is(err, ErrTarSymlinkUnsupported) {
		t.Fatalf("expected ErrTarSymlinkUnsupported, got %v", err)
	}

	// Bad path test: traversal
	tarPath2 := filepath.Join(tmpDir, "traversal.tar")
	f2, _ := os.Create(tarPath2)
	tw2 := NewLayerWriter(f2)
	hdr2 := &tar.Header{
		Name:     "../escape.txt",
		Typeflag: tar.TypeReg,
		Size:     4,
	}
	tw2.tw.WriteHeader(hdr2)
	tw2.tw.Write([]byte("test"))
	tw2.Finish()
	f2.Close()

	err2 := Extract(tarPath2, filepath.Join(tmpDir, "out2"))
	if !errors.Is(err2, ErrTarBadPath) {
		t.Fatalf("expected ErrTarBadPath, got %v", err2)
	}
}

func hashSha256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
