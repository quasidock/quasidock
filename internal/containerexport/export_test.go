package containerexport

import (
	"archive/tar"
	"bytes"
	stdcontext "context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/estargz"
	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

func fixture(t *testing.T) (*context.Context, *store.ContainerRecord, oci.Descriptor) {
	t.Helper()
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.New()
	ctx.Store = st
	app := layer(t, st, format.FormatGzip, "program.exe", []byte("\x00application\xff"))
	a := layer(t, st, format.FormatEstargz, "runtime.dll", []byte("shared runtime"))
	b := layer(t, st, format.FormatTar, "runtime.dll", []byte("other runtime"))
	seed := layer(t, st, format.FormatGzip, "deleted.txt", []byte("old seed"))
	cfg := oci.AppConfig{FormatVersion: 4, ID: "export-fixture", Version: "1", Entrypoint: []string{"program.exe"}, DataLayer: seed.Digest,
		From: map[string]oci.Runtime{
			"alpha": {ID: "alpha", Version: "1", Image: a.Digest, Layer: a.Digest},
			"beta":  {ID: "beta", Version: "1", Image: b.Digest, Layer: b.Digest},
			"con":   {ID: "con", Version: "1", Image: a.Digest, Layer: a.Digest},
		}}
	config := jsonBlob(t, st, cfg, oci.MediaTypeConfigV4)
	manifest := jsonBlob(t, st, oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: config,
		Layers: []oci.Descriptor{b, a, a, seed, app}}, oci.MediaTypeManifest)
	name := "example.test/export-fixture:1"
	if err := st.RegisterImage(store.Ref{ID: cfg.ID, Version: cfg.Version, Manifest: manifest.Digest, Source: name}, name, true); err != nil {
		t.Fatal(err)
	}
	c, err := st.CreateContainer("export-source", store.ContainerRecord{Image: manifest.Digest, AppID: cfg.ID})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(c.DataDir, "home", "note.txt"), []byte("current private data"))
	if err := os.MkdirAll(filepath.Join(c.DataDir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Export uses current data, and does not require the original seed blob.
	if err := os.Remove(st.BlobPath(oci.DigestHex(seed.Digest))); err != nil {
		t.Fatal(err)
	}
	return ctx, c, app
}

func jsonBlob(t *testing.T, st *store.Store, value any, kind string) oci.Descriptor {
	t.Helper()
	b, err := oci.ToJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.PutBlob(b, kind)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func layer(t *testing.T, st *store.Store, kind format.Format, name string, data []byte) oci.Descriptor {
	t.Helper()
	dir := t.TempDir()
	blob, err := format.WriteLayer(filepath.Join(dir, "blob"), filepath.Join(dir, "layer.tar"), kind, func(w io.Writer) error {
		lw := format.NewLayerWriter(w)
		if err := lw.AddDir("empty"); err != nil {
			return err
		}
		if err := lw.AddFile(name, int64(len(data)), bytes.NewReader(data)); err != nil {
			return err
		}
		return lw.Finish()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.PutBlobFile(blob.Path, store.FileHash{Hex: oci.DigestHex(blob.Digest), Size: blob.Size}, blob.MediaType)
	if err != nil {
		t.Fatal(err)
	}
	d.Annotations = blob.Annotations
	return d
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func members(t *testing.T, data []byte) map[string]string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	result := map[string]string{}
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		mode := int64(0o644)
		if h.Typeflag == tar.TypeDir {
			mode = 0o755
		} else if h.Typeflag != tar.TypeReg {
			t.Fatalf("unexpected member type: %+v", h)
		}
		if h.Mode != mode || !h.ModTime.Equal(time.Unix(0, 0)) || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Fatalf("noncanonical metadata: %+v", h)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := result[h.Name]; exists {
			t.Fatalf("duplicate member %q", h.Name)
		}
		result[h.Name] = string(content)
		names = append(names, h.Name)
	}
	if !slices.IsSorted(names) {
		t.Fatalf("unsorted members: %v", names)
	}
	return result
}

func TestExportFilesystem(t *testing.T) {
	ctx, c, _ := fixture(t)
	var out bytes.Buffer
	if err := Export(stdcontext.Background(), ctx, Options{Container: c.Name, Pause: true}, &out); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"app/": "", "app/empty/": "", "app/program.exe": "\x00application\xff", "data/": "", "data/empty/": "",
		"data/home/": "", "data/home/note.txt": "current private data", "runtimes/": ""}
	for alias, content := range map[string]string{"alpha": "shared runtime", "beta": "other runtime", "con": "shared runtime"} {
		base := "runtimes/runtime-" + alias + "/"
		want[base], want[base+"empty/"], want[base+"runtime.dll"] = "", "", content
	}
	if got := members(t, out.Bytes()); !reflect.DeepEqual(got, want) {
		t.Fatalf("members:\ngot: %v\nwant: %v", got, want)
	}
	first := bytes.Clone(out.Bytes())
	out.Reset()
	if err := Export(stdcontext.Background(), ctx, Options{Container: c.ShortID()}, &out); err != nil || !bytes.Equal(first, out.Bytes()) {
		t.Fatal("repeated export is not deterministic", err)
	}
	entries, err := os.ReadDir(ctx.Store.Path("tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatal("leaked staging", entries, err)
	}
	entries, err = os.ReadDir(ctx.Store.Path("deploy"))
	if err != nil || len(entries) != 0 {
		t.Fatal("export changed deployments", entries, err)
	}
}

func TestExportMountExclusions(t *testing.T) {
	for _, root := range []bool{false, true} {
		t.Run(map[bool]string{false: "subpath", true: "root"}[root], func(t *testing.T) {
			ctx, c, _ := fixture(t)
			target := "${data}/hidden"
			if root {
				target = "${data}"
			}
			_, err := ctx.Store.UpdateContainer(c.ID, func(c *store.ContainerRecord) error {
				c.HostConfig.Mounts = []mounts.Spec{{Type: "bind", Source: t.TempDir(), Target: target}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(c.DataDir, "hidden", "private.txt"), []byte("hidden"))
			var out bytes.Buffer
			if err := Export(stdcontext.Background(), ctx, Options{Container: c.ID}, &out); err != nil {
				t.Fatal(err)
			}
			for name := range members(t, out.Bytes()) {
				if strings.HasPrefix(name, "data/hidden") || root && name != "data/" && strings.HasPrefix(name, "data/") {
					t.Fatalf("captured mount destination %s", name)
				}
			}
		})
	}
}

func TestExportMissingAndCorruptBlobs(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "corrupt", true: "missing"}[missing], func(t *testing.T) {
			ctx, c, desc := fixture(t)
			path := ctx.Store.BlobPath(oci.DigestHex(desc.Digest))
			want := store.ErrCorruptBlob
			if missing {
				want = store.ErrMissingBlob
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFile(t, path, bytes.Repeat([]byte{'x'}, int(desc.Size)))
			}
			var out bytes.Buffer
			err := Export(stdcontext.Background(), ctx, Options{Container: c.ID}, &out)
			if !errors.Is(err, want) || out.Len() != 0 {
				t.Fatalf("preflight: %v, output %d", err, out.Len())
			}
			if missing && !strings.Contains(err.Error(), "quasidock pull example.test/export-fixture@"+c.Image) {
				t.Fatal("missing exact pull guidance", err)
			}
		})
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestExportFailuresAndCancellation(t *testing.T) {
	ctx, c, _ := fixture(t)
	failure := errors.New("output failed")
	if err := Export(stdcontext.Background(), ctx, Options{Container: c.ID}, writerFunc(func([]byte) (int, error) { return 0, failure })); !errors.Is(err, failure) {
		t.Fatal("lost writer error", err)
	}
	cancel, stop := stdcontext.WithCancel(stdcontext.Background())
	if err := Export(cancel, ctx, Options{Container: c.ID}, writerFunc(func(p []byte) (int, error) { stop(); return len(p), nil })); !errors.Is(err, stdcontext.Canceled) {
		t.Fatal("ignored output cancellation", err)
	}
	lock, err := ctx.Store.LockContainer(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	cancel, stop = stdcontext.WithTimeout(stdcontext.Background(), 50*time.Millisecond)
	defer stop()
	if err := Export(cancel, ctx, Options{Container: c.ID}, io.Discard); !errors.Is(err, stdcontext.DeadlineExceeded) {
		t.Fatal("ignored lock-wait cancellation", err)
	}
	entries, err := os.ReadDir(ctx.Store.Path("tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatal("leaked staging", entries, err)
	}
}

func TestExportMissingMetadataGuidance(t *testing.T) {
	for _, kind := range []string{"manifest", "config"} {
		t.Run(kind, func(t *testing.T) {
			ctx, c, _ := fixture(t)
			image, err := ctx.Store.ReadImage(c.Image, c.Image)
			if err != nil {
				t.Fatal(err)
			}
			digest := c.Image
			if kind == "config" {
				digest = image.Manifest.Config.Digest
			}
			if err := os.Remove(ctx.Store.BlobPath(oci.DigestHex(digest))); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err = Export(stdcontext.Background(), ctx, Options{Container: c.ID}, &out)
			if !errors.Is(err, store.ErrMissingBlob) || !strings.Contains(err.Error(), "quasidock pull example.test/export-fixture@"+c.Image) || out.Len() != 0 {
				t.Fatal("missing metadata guidance", err)
			}
		})
	}
}

func TestExportRejectsInconsistentDescriptorSizes(t *testing.T) {
	ctx, c, _ := fixture(t)
	image, err := ctx.Store.ReadImage(c.Image, c.Image)
	if err != nil {
		t.Fatal(err)
	}
	image.Manifest.Layers[2].Size++ // Same digest as layers[1], conflicting size.
	manifest := jsonBlob(t, ctx.Store, image.Manifest, oci.MediaTypeManifest)
	if _, err := ctx.Store.UpdateContainer(c.ID, func(c *store.ContainerRecord) error { c.Image = manifest.Digest; return nil }); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Export(stdcontext.Background(), ctx, Options{Container: c.ID}, &out); !errors.Is(err, store.ErrCorruptBlob) || out.Len() != 0 {
		t.Fatal("accepted conflicting descriptor sizes", err)
	}
}

func TestExportFileRenameFailure(t *testing.T) {
	ctx, c, _ := fixture(t)
	parent := t.TempDir()
	path := filepath.Join(parent, "existing-dir")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ExportFile(stdcontext.Background(), ctx, Options{Container: c.ID}, path); err == nil {
		t.Fatal("replaced a directory with an archive")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatal("rename failure changed output or leaked staging", entries, err)
	}
}

func TestExportFileAtomicAndOverlap(t *testing.T) {
	ctx, c, desc := fixture(t)
	path := filepath.Join(t.TempDir(), "export.tar")
	if err := ExportFile(stdcontext.Background(), ctx, Options{Container: c.ID}, path); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ctx.Store.BlobPath(oci.DigestHex(desc.Digest))); err != nil {
		t.Fatal(err)
	}
	if err := ExportFile(stdcontext.Background(), ctx, Options{Container: c.ID}, path); !errors.Is(err, store.ErrMissingBlob) {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
		t.Fatal("failed export replaced archive", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("leaked output staging", entries, err)
	}
	for _, root := range []string{c.DataDir, ctx.Store.Path("blobs"), ctx.Store.Path("tmp")} {
		if err := ExportFile(stdcontext.Background(), ctx, Options{Container: c.ID}, filepath.Join(root, "output.tar")); err == nil || !strings.Contains(err.Error(), "overlaps") {
			t.Fatal("allowed output overlap", err)
		}
	}
	alias := mounts.Mount{Source: c.DataDir, Destination: filepath.Join(filepath.Dir(path), "alias")}
	if err := mounts.Attach(filepath.Dir(path), alias); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mounts.Detach(filepath.Dir(path), alias) }()
	if err := ExportFile(stdcontext.Background(), ctx, Options{Container: c.ID}, filepath.Join(alias.Destination, "output.tar")); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatal("allowed output through data alias", err)
	}
}

func TestExportRejectsReparsePoint(t *testing.T) {
	ctx, c, _ := fixture(t)
	m := mounts.Mount{Source: t.TempDir(), Destination: filepath.Join(c.DataDir, "link")}
	if err := mounts.Attach(c.DataDir, m); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mounts.Detach(c.DataDir, m) }()
	var out bytes.Buffer
	if err := Export(stdcontext.Background(), ctx, Options{Container: c.ID}, &out); err == nil || out.Len() != 0 {
		t.Fatal("followed unconfigured reparse point", err)
	}
}

func TestExtractLayerRejectsUnsafeEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		head tar.Header
	}{
		{"traversal", tar.Header{Name: "../outside", Typeflag: tar.TypeReg}},
		{"absolute", tar.Header{Name: "/outside", Typeflag: tar.TypeReg}},
		{"backslash", tar.Header{Name: `dir\file`, Typeflag: tar.TypeReg}},
		{"device", tar.Header{Name: "NUL", Typeflag: tar.TypeReg}},
		{"stream", tar.Header{Name: "file:stream", Typeflag: tar.TypeReg}},
		{"symlink", tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "outside"}},
		{"hardlink", tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "outside"}},
		{"fifo", tar.Header{Name: "fifo", Typeflag: tar.TypeFifo}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, _ := fixture(t)
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			if err := tw.WriteHeader(&tc.head); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			desc, err := ctx.Store.PutBlob(archive.Bytes(), oci.MediaTypeLayerTar)
			if err != nil {
				t.Fatal(err)
			}
			if err := extractLayer(stdcontext.Background(), ctx.Store, desc, t.TempDir(), filepath.Join(t.TempDir(), "dest")); err == nil {
				t.Fatal("accepted unsafe layer entry")
			}
		})
	}
}

func TestExtractLayerDuplicateTruncationAndTOC(t *testing.T) {
	ctx, _, _ := fixture(t)
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, name := range []string{"first", "FIRST"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	desc, err := ctx.Store.PutBlob(archive.Bytes(), oci.MediaTypeLayerTar)
	if err != nil {
		t.Fatal(err)
	}
	if err := extractLayer(stdcontext.Background(), ctx.Store, desc, t.TempDir(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatal("accepted case duplicate", err)
	}
	archive.Reset()
	tw = tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "truncated", Mode: 0o644, Size: 1024, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	// A digest can describe malformed input; extraction still checks payloads.
	desc, err = ctx.Store.PutBlob(archive.Bytes(), oci.MediaTypeLayerTar)
	if err != nil {
		t.Fatal(err)
	}
	if err := extractLayer(stdcontext.Background(), ctx.Store, desc, t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("accepted truncated payload")
	}
	desc = layer(t, ctx.Store, format.FormatEstargz, "file", []byte("data"))
	desc.Annotations[estargz.TOCJSONDigestAnnotation] = "sha256:" + strings.Repeat("0", 64)
	if err := extractLayer(stdcontext.Background(), ctx.Store, desc, t.TempDir(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "TOC") {
		t.Fatal("accepted incorrect TOC annotation", err)
	}
}
