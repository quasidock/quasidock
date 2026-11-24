package estargz

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func buildSampleTar(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := files[name]
		clean := strings.TrimPrefix(name, "/")
		if strings.HasSuffix(clean, "/") {
			if err := tw.WriteHeader(&tar.Header{
				Name:     clean,
				Typeflag: tar.TypeDir,
				Mode:     0o755,
			}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     clean,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestConvertAndReadEstargz(t *testing.T) {
	files := map[string]string{
		"bin/":          "",
		"bin/app.exe":   "MZ " + strings.Repeat("hello world ", 200),
		"share/doc.txt": "Documentation content",
	}

	tarData := buildSampleTar(t, files)

	var estargzBuf bytes.Buffer
	annotations, err := Convert(bytes.NewReader(tarData.Bytes()), &estargzBuf, nil)
	if err != nil {
		t.Fatalf("Convert failed: %v", err)
	}

	tocDigest := annotations[TOCJSONDigestAnnotation]
	if tocDigest == "" || !strings.HasPrefix(tocDigest, "sha256:") {
		t.Fatalf("invalid TOC digest annotation: %s", tocDigest)
	}
	if annotations[StoreUncompressedSizeAnnotation] == "" {
		t.Fatalf("missing uncompressed size annotation")
	}

	// Open with Reader
	sr := io.NewSectionReader(bytes.NewReader(estargzBuf.Bytes()), 0, int64(estargzBuf.Len()))
	reader, err := Open(sr)
	if err != nil {
		t.Fatalf("Open reader failed: %v", err)
	}

	if reader.TOCDigest() != tocDigest {
		t.Fatalf("TOC digest mismatch: reader=%s, annot=%s", reader.TOCDigest(), tocDigest)
	}

	// Check Lookup and OpenFile
	ent, ok := reader.Lookup("bin/app.exe")
	if !ok {
		t.Fatalf("Lookup bin/app.exe failed")
	}
	if ent.Size != int64(len(files["bin/app.exe"])) {
		t.Fatalf("size mismatch: got %d, want %d", ent.Size, len(files["bin/app.exe"]))
	}

	fr, err := reader.OpenFile("bin/app.exe")
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	gotData, err := io.ReadAll(fr)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(gotData) != files["bin/app.exe"] {
		t.Fatalf("content mismatch for bin/app.exe")
	}

	// Extract to directory
	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")
	if err := reader.Extract(outDir, 2); err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	extractedFile, err := os.ReadFile(filepath.Join(outDir, "bin", "app.exe"))
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(extractedFile) != files["bin/app.exe"] {
		t.Fatalf("extracted file mismatch")
	}
}

func TestPlanDeltaUpdate(t *testing.T) {
	// A shared large file (> 64 KiB compressed so it's not coalesced as a tiny gap)
	largeBytes := make([]byte, 80*1024)
	state := uint32(1)
	for i := range largeBytes {
		state = state*1664525 + 1013904223
		largeBytes[i] = byte(state >> 24)
	}
	largeContent := string(largeBytes)

	oldFiles := map[string]string{
		"bin/app.exe":     "old-app-binary",
		"share/large.dat": largeContent,
		"share/z_end.txt": "end file",
	}
	oldTar := buildSampleTar(t, oldFiles)
	var oldEstargz bytes.Buffer
	_, err := Convert(bytes.NewReader(oldTar.Bytes()), &oldEstargz, nil)
	if err != nil {
		t.Fatal(err)
	}

	oldSr := io.NewSectionReader(bytes.NewReader(oldEstargz.Bytes()), 0, int64(oldEstargz.Len()))
	oldReader, err := Open(oldSr)
	if err != nil {
		t.Fatal(err)
	}

	// New version: large.dat and z_end.txt are identical, app.exe changed
	newFiles := map[string]string{
		"bin/app.exe":     "new-app-binary",
		"share/large.dat": largeContent,
		"share/z_end.txt": "end file",
	}
	newTar := buildSampleTar(t, newFiles)
	var newEstargz bytes.Buffer
	_, err = Convert(bytes.NewReader(newTar.Bytes()), &newEstargz, nil)
	if err != nil {
		t.Fatal(err)
	}

	newSr := io.NewSectionReader(bytes.NewReader(newEstargz.Bytes()), 0, int64(newEstargz.Len()))
	newReader, err := Open(newSr)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range oldReader.AllEntries() {
		t.Logf("OLD: %s off=%d next=%d diff=%d digest=%s", e.Name, e.Offset, e.NextOffset(), e.NextOffset()-e.Offset, e.Digest)
	}
	for _, e := range newReader.AllEntries() {
		t.Logf("NEW: %s off=%d next=%d diff=%d digest=%s", e.Name, e.Offset, e.NextOffset(), e.NextOffset()-e.Offset, e.Digest)
	}

	plan, err := MakePlan(newReader, []*Reader{oldReader}, int64(newEstargz.Len()))
	if err != nil {
		t.Fatalf("MakePlan failed: %v", err)
	}

	if plan.Copied() == 0 {
		t.Fatalf("expected some bytes copied from old blob, got 0")
	}

	t.Logf("Plan: %d pieces, copied=%d, fetched=%d, reqs=%d",
		len(plan.Pieces), plan.Copied(), plan.Fetched(), plan.NumRequests())

	plan.Coalesce(int64(newEstargz.Len()) - 51)
	t.Logf("After Coalesce: %d pieces, copied=%d, fetched=%d, reqs=%d",
		len(plan.Pieces), plan.Copied(), plan.Fetched(), plan.NumRequests())

	if plan.Copied() == 0 {
		t.Fatalf("expected copied bytes preserved after coalesce for large gap")
	}
}

func TestPlanAddedFirstFile(t *testing.T) {
	data := make([]byte, 600*1024)
	state := uint32(1)
	for i := range data {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		data[i] = byte(state)
	}
	blobs := make([][]byte, 2)
	readers := make([]*Reader, 2)
	for i := range blobs {
		files := map[string]string{"busybox.exe": string(data)}
		if i == 1 {
			files["a.txt"] = "version 2"
		}
		var blob bytes.Buffer
		if _, err := Convert(buildSampleTar(t, files), &blob, nil); err != nil {
			t.Fatal(err)
		}
		blobs[i] = blob.Bytes()
		var err error
		readers[i], err = Open(io.NewSectionReader(bytes.NewReader(blobs[i]), 0, int64(len(blobs[i]))))
		if err != nil {
			t.Fatal(err)
		}
	}
	plan, err := MakePlan(readers[1], readers[:1], int64(len(blobs[1])))
	if err != nil {
		t.Fatal(err)
	}
	plan.Coalesce(int64(len(blobs[1]) - 16*1024))
	if plan.Copied()*4 < int64(len(blobs[1]))*3 {
		for _, r := range readers {
			for _, e := range r.AllEntries() {
				t.Logf("%s: %s offset=%d next=%d", e.Name, e.Type, e.Offset, e.NextOffset())
			}
		}
		t.Fatalf("copied only %d of %d bytes", plan.Copied(), len(blobs[1]))
	}
	var rebuilt bytes.Buffer
	for _, piece := range plan.Pieces {
		source, offset := blobs[1], piece.Offset
		if piece.Type == PieceCopy {
			source, offset = blobs[piece.SourceIndex], piece.SourceOffset
		}
		rebuilt.Write(source[offset : offset+piece.Length])
	}
	if !bytes.Equal(rebuilt.Bytes(), blobs[1]) {
		t.Fatalf("rebuilt %d bytes, want the exact %d-byte blob", rebuilt.Len(), len(blobs[1]))
	}
}
