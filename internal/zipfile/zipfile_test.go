package zipfile

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListAndReadSampleZip(t *testing.T) {
	samplePath := filepath.Join("..", "..", "src", "testdata", "sample.zip")
	data, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatalf("ReadFile sample.zip failed: %v", err)
	}

	reader := bytes.NewReader(data)
	entries, err := List(reader, int64(len(data)))
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}

	expectedNames := []string{"top", "top/a.txt", "top/sub/b.txt"}
	if len(names) != len(expectedNames) {
		t.Fatalf("expected %d entries, got %d: %v", len(expectedNames), len(names), names)
	}
	for i, want := range expectedNames {
		if names[i] != want {
			t.Errorf("entry %d: got %s, want %s", i, names[i], want)
		}
	}

	if !entries[0].IsDir {
		t.Errorf("expected top to be dir")
	}

	wants := []string{strings.Repeat("deflated ", 20), "stored\n"}
	for i, e := range entries[1:] {
		rc, err := e.Open()
		if err != nil {
			t.Fatalf("Open %s failed: %v", e.Name, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("ReadAll %s failed: %v", e.Name, err)
		}
		if string(got) != wants[i] {
			t.Errorf("%s content mismatch: got %q, want %q", e.Name, string(got), wants[i])
		}
	}
}

func TestZipBadPathAndDirectorySize(t *testing.T) {
	// Traversal
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.Create("../escape.txt")
	zw.Close()

	r := bytes.NewReader(buf.Bytes())
	_, err := List(r, int64(buf.Len()))
	if !errors.Is(err, ErrZipBadFilename) {
		t.Fatalf("expected ErrZipBadFilename, got: %v", err)
	}

	// Bad directory with non-zero size
	var buf2 bytes.Buffer
	zw2 := zip.NewWriter(&buf2)
	zw2.Create("dir/")
	zw2.Close()

	// Patch central directory uncompressed size to non-zero
	b2 := buf2.Bytes()
	cdSig := []byte{0x50, 0x4b, 0x01, 0x02}
	idx := bytes.Index(b2, cdSig)
	if idx != -1 {
		b2[idx+24] = 10 // uncompressed size = 10
	}

	r2 := bytes.NewReader(b2)
	_, err = List(r2, int64(len(b2)))
	if !errors.Is(err, ErrZipBadDirectorySize) {
		t.Fatalf("expected ErrZipBadDirectorySize, got: %v", err)
	}
}
