package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ulikunitz/xz"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/store"
)

func TestPlainTarCompression(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	content := []byte("source fixture")
	if err := tw.WriteHeader(&tar.Header{Name: "bin/tool.exe", Mode: 0755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []recipe.Kind{recipe.KindTar, recipe.KindTarGz, recipe.KindTarXz} {
		t.Run(string(kind), func(t *testing.T) {
			st, err := store.Open(t.TempDir(), false, "")
			if err != nil {
				t.Fatal(err)
			}
			var compressed bytes.Buffer
			var writer io.WriteCloser
			switch kind {
			case recipe.KindTar:
				compressed.Write(archive.Bytes())
			case recipe.KindTarGz:
				writer = gzip.NewWriter(&compressed)
			case recipe.KindTarXz:
				writer, err = xz.NewWriter(&compressed)
				if err != nil {
					t.Fatal(err)
				}
			}
			if writer != nil {
				if _, err := writer.Write(archive.Bytes()); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			file := filepath.Join(t.TempDir(), "source."+string(kind))
			if err := os.WriteFile(file, compressed.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			hash, err := store.Sha256File(file)
			if err != nil {
				t.Fatal(err)
			}
			s := &Sources{ctx: &context.Context{Store: st}}
			plain, err := s.plainTar(file, kind, hash.Hex)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(plain)
			if err != nil || !bytes.Equal(got, archive.Bytes()) {
				t.Fatalf("decoded archive differs: %v", err)
			}
			if kind != recipe.KindTar {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if cached, err := s.plainTar(file, kind, hash.Hex); err != nil || cached != plain {
					t.Fatalf("cached tar unavailable: %q %v", cached, err)
				}
			}
		})
	}
}

func TestPlainTarRejectsTruncatedXZ(t *testing.T) {
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	w, err := xz.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("archive data")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "truncated.tar.xz")
	if err := os.WriteFile(file, compressed.Bytes()[:compressed.Len()-1], 0600); err != nil {
		t.Fatal(err)
	}
	hash, err := store.Sha256File(file)
	if err != nil {
		t.Fatal(err)
	}
	s := &Sources{ctx: &context.Context{Store: st}}
	if _, err := s.plainTar(file, recipe.KindTarXz, hash.Hex); err == nil {
		t.Fatal("accepted truncated XZ stream")
	}
	if store.Exists(st.Path("cache", "downloads", hash.Hex+".tar")) {
		t.Fatal("cached a partial tar")
	}
}
