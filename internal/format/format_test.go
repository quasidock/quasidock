package format

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/estargz"
	"github.com/quasidock/quasidock/internal/oci"
)

func TestFormatOfDescriptor(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)

	tarDesc := oci.Descriptor{MediaType: oci.MediaTypeLayerTar, Digest: d, Size: 1}
	f, ok := FormatOfDescriptor(tarDesc)
	if !ok || f != FormatTar {
		t.Fatalf("expected FormatTar, got %v (%v)", f, ok)
	}

	gzDesc := oci.Descriptor{MediaType: oci.MediaTypeLayerTarGzip, Digest: d, Size: 1}
	f, ok = FormatOfDescriptor(gzDesc)
	if !ok || f != FormatGzip {
		t.Fatalf("expected FormatGzip, got %v (%v)", f, ok)
	}

	otherAnn := map[string]string{
		estargz.StoreUncompressedSizeAnnotation: "1",
	}
	f, ok = FormatOfDescriptor(oci.Descriptor{MediaType: oci.MediaTypeLayerTarGzip, Digest: d, Size: 1, Annotations: otherAnn})
	if !ok || f != FormatGzip {
		t.Fatalf("expected FormatGzip without TOC digest, got %v (%v)", f, ok)
	}

	estargzAnn := map[string]string{
		estargz.TOCJSONDigestAnnotation: d,
	}
	f, ok = FormatOfDescriptor(oci.Descriptor{MediaType: oci.MediaTypeLayerTarGzip, Digest: d, Size: 1, Annotations: estargzAnn})
	if !ok || f != FormatEstargz {
		t.Fatalf("expected FormatEstargz, got %v (%v)", f, ok)
	}

	_, ok = FormatOfDescriptor(oci.Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+zstd", Digest: d, Size: 1})
	if ok {
		t.Fatalf("expected zstd to be rejected")
	}
}

func TestDeployTarGzipEstargz(t *testing.T) {
	tmpDir := t.TempDir()
	workDir := filepath.Join(tmpDir, "work")
	os.MkdirAll(workDir, 0o755)

	content := "tool binary data"

	// 1. Plain tar
	tarBlobPath := filepath.Join(workDir, "blob.tar")
	tf, err := os.Create(tarBlobPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewLayerWriter(tf)
	tw.AddDir("bin")
	tw.AddFile("bin/tool.exe", int64(len(content)), strings.NewReader(content))
	tw.Finish()
	tf.Close()

	destTar := filepath.Join(tmpDir, "dest_tar")
	tarDesc := oci.Descriptor{MediaType: oci.MediaTypeLayerTar, Digest: "sha256:1111", Size: 100}
	if err := Deploy(false, tarDesc, tarBlobPath, workDir, destTar); err != nil {
		t.Fatalf("Deploy tar failed: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(destTar, "bin", "tool.exe"))
	if string(got) != content {
		t.Fatalf("content mismatch for tar deploy: got %q, want %q", got, content)
	}

	// 2. Gzip
	gzBlobPath := filepath.Join(workDir, "blob.tar.gz")
	gf, err := os.Create(gzBlobPath)
	if err != nil {
		t.Fatal(err)
	}
	gw, _ := GzipWriter(gf)
	gtw := NewLayerWriter(gw)
	gtw.AddDir("bin")
	gtw.AddFile("bin/tool.exe", int64(len(content)), strings.NewReader(content))
	gtw.Finish()
	gw.Close()
	gf.Close()

	destGz := filepath.Join(tmpDir, "dest_gz")
	gzDesc := oci.Descriptor{MediaType: oci.MediaTypeLayerTarGzip, Digest: "sha256:2222", Size: 100}
	if err := Deploy(false, gzDesc, gzBlobPath, workDir, destGz); err != nil {
		t.Fatalf("Deploy gzip failed: %v", err)
	}
	gotGz, _ := os.ReadFile(filepath.Join(destGz, "bin", "tool.exe"))
	if string(gotGz) != content {
		t.Fatalf("content mismatch for gzip deploy: got %q, want %q", gotGz, content)
	}

	// 3. eStargz
	var tarBuf bytes.Buffer
	st := tar.NewWriter(&tarBuf)
	st.WriteHeader(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	st.WriteHeader(&tar.Header{Name: "bin/tool.exe", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))})
	st.Write([]byte(content))
	st.Close()

	esBlobPath := filepath.Join(workDir, "blob.estargz")
	ef, err := os.Create(esBlobPath)
	if err != nil {
		t.Fatal(err)
	}
	ann, err := estargz.Convert(&tarBuf, ef, nil)
	if err != nil {
		t.Fatal(err)
	}
	ef.Close()

	destEs := filepath.Join(tmpDir, "dest_estargz")
	esDesc := oci.Descriptor{
		MediaType:   oci.MediaTypeLayerTarGzip,
		Digest:      "sha256:3333",
		Size:        100,
		Annotations: ann,
	}
	if err := Deploy(false, esDesc, esBlobPath, workDir, destEs); err != nil {
		t.Fatalf("Deploy estargz failed: %v", err)
	}
	gotEs, _ := os.ReadFile(filepath.Join(destEs, "bin", "tool.exe"))
	if string(gotEs) != content {
		t.Fatalf("content mismatch for estargz deploy: got %q, want %q", gotEs, content)
	}
}

func calcSha256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
