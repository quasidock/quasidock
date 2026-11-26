package imageimport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

type entry struct {
	name, data string
	kind       byte
}

func fixtureConfig() oci.AppConfig {
	return oci.AppConfig{ID: "import-fixture", Version: "1", Entrypoint: []string{"program.exe"}}
}

func configJSON(t *testing.T, cfg oci.AppConfig) []byte {
	t.Helper()
	b, err := oci.ToJSON(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureArchive() []entry {
	return []entry{
		{"app/", "", tar.TypeDir}, {"data/", "", tar.TypeDir}, {"runtimes/", "", tar.TypeDir},
		{"app/program.exe", "\x00binary\xff", tar.TypeReg}, {"app/empty/", "", tar.TypeDir},
		{"data/home/", "", tar.TypeDir}, {"data/home/note", "private", tar.TypeReg},
	}
}

func archive(t *testing.T, entries []entry) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.kind, Mode: 0o777, ModTime: time.Now()}
		if e.kind == tar.TypeReg {
			h.Size = int64(len(e.data))
		}
		if e.kind == tar.TypeSymlink || e.kind == tar.TypeLink {
			h.Linkname = "../outside"
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.kind == tar.TypeReg {
			if _, err := io.WriteString(tw, e.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func compressed(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	if _, err := gw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func fixture(t *testing.T) *context.Context {
	t.Helper()
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.New()
	ctx.Store = st
	t.Cleanup(func() {
		entries, err := os.ReadDir(st.Path("deploy"))
		if err != nil {
			t.Error(err)
			return
		}
		for _, entry := range entries {
			if entry.IsDir() && oci.IsSha256Hex(entry.Name()) {
				if _, err := st.DeleteDeployment("sha256:" + entry.Name()); err != nil {
					t.Error(err)
				}
			}
		}
	})
	return ctx
}

func mustImport(t *testing.T, ctx *context.Context, b []byte, cfg oci.AppConfig, opts Options) string {
	t.Helper()
	digest, err := Import(stdcontext.Background(), ctx, bytes.NewReader(b), bytes.NewReader(configJSON(t, cfg)), opts)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func checkClean(t *testing.T, ctx *context.Context) {
	t.Helper()
	staged, err := filepath.Glob(ctx.Store.Path("tmp", "image-import-*"))
	if err != nil || len(staged) != 0 {
		t.Fatal("import leaked staging", staged, err)
	}
	if store.IsLocked(ctx.Store.Path("lock")) {
		t.Fatal("import leaked its operation lock")
	}
}

func TestImportLayersAndRegistration(t *testing.T) {
	ctx := fixture(t)
	cfg := fixtureConfig()
	cfg.Entrypoint = []string{"${alpha}/program.exe"}
	cfg.Workdir = "${data}/home"
	cfg.DataLayer = "old seed"
	cfg.From = map[string]oci.Runtime{}
	entries := fixtureArchive()
	for _, alias := range []string{"alpha", "con"} {
		cfg.From[alias] = oci.Runtime{ID: alias, Version: "2", Image: "sha256:" + strings.Repeat("a", 64), Layer: "old layer", Path: []string{"bin"}, Env: map[string]string{"ROOT": "${app}"}}
		entries = append(entries, entry{"runtimes/runtime-" + alias + "/", "", tar.TypeDir}, entry{"runtimes/runtime-" + alias + "/program.exe", "runtime bytes", tar.TypeReg})
	}
	opts := Options{Target: "example.test/different:alias", Source: "original.tar", Changes: []string{`ENV SAVED=first`, `ENV SAVED=second`, `LABEL purpose=import`, `CMD ["argument"]`}}
	b := archive(t, entries)
	digest := mustImport(t, ctx, b, cfg, opts)
	img, err := ctx.Store.ReadImage(cfg.ID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if img.Config.ID != cfg.ID || img.Config.Version != cfg.Version || img.Config.FormatVersion != 4 || img.Config.Env["SAVED"] != "second" || img.Config.Labels["purpose"] != "import" || !reflect.DeepEqual(img.Config.Cmd, []string{"argument"}) {
		t.Fatalf("imported config: %+v", img.Config)
	}
	if len(img.Manifest.Layers) != 4 || img.Manifest.Layers[0].Digest != img.Manifest.Layers[1].Digest || img.Config.From["con"].Image != cfg.From["con"].Image || img.Config.From["alpha"].Layer != img.Manifest.Layers[0].Digest {
		t.Fatalf("runtime reconstruction: %+v %+v", img.Manifest, img.Config.From)
	}
	if err := oci.ValidateLayers("test", &img.Manifest, &img.Config); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		desc          oci.Descriptor
		path, content string
	}{
		{img.Manifest.Layers[0], "program.exe", "runtime bytes"},
		{oci.DataDescriptor(&img.Manifest, &img.Config), "home/note", "private"},
		{oci.OwnLayer(&img.Manifest), "program.exe", "\x00binary\xff"},
	} {
		dir, err := ctx.Store.Deploy(item.desc)
		if err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(item.path))); err != nil || string(data) != item.content {
			t.Fatal("imported bytes changed", data, err)
		}
	}
	idx, err := ctx.Store.ImageCatalog()
	if err != nil || idx.Names[opts.Target] != digest || idx.Defaults[cfg.ID] != digest || idx.Images[digest].CachedAt == "" {
		t.Fatal("registration failed", idx, err)
	}
	// Equivalent trees, reordered input and fresh metadata produce the same ID.
	slices.Reverse(entries)
	opts.Source = "different.tar"
	if repeated := mustImport(t, ctx, compressed(t, archive(t, entries)), cfg, opts); repeated != digest {
		t.Fatal("import is not deterministic", repeated, digest)
	}
	after, _ := ctx.Store.ImageCatalog()
	if after.Images[digest].CachedAt != idx.Images[digest].CachedAt || after.Images[digest].Source != "original.tar" {
		t.Fatal("repeat import replaced local provenance or cache date", after.Images[digest])
	}
	checkClean(t, ctx)
}

func TestImportNameConflictsAndDefaults(t *testing.T) {
	ctx := fixture(t)
	cfg := fixtureConfig()
	b := archive(t, fixtureArchive())
	opts := Options{Target: "example.test/alias:latest"}
	first := mustImport(t, ctx, b, cfg, opts)
	cfg.Version = "2"
	before, _ := os.ReadFile(ctx.Store.Path("images", "index.json"))
	if _, err := Import(stdcontext.Background(), ctx, bytes.NewReader(b), bytes.NewReader(configJSON(t, cfg)), opts); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatal("name conflict was not rejected", err)
	}
	if after, _ := os.ReadFile(ctx.Store.Path("images", "index.json")); !bytes.Equal(before, after) {
		t.Fatal("failed import changed the catalog")
	}
	opts.Force = true
	second := mustImport(t, ctx, b, cfg, opts)
	idx, _ := ctx.Store.ImageCatalog()
	if first == second || idx.Defaults[cfg.ID] != first || idx.Names[opts.Target] != second || len(idx.Images) != 2 {
		t.Fatal("force changed defaults or discarded the previous image", idx)
	}
	cfg.Version = "3"
	unnamed := mustImport(t, ctx, b, cfg, Options{})
	idx, _ = ctx.Store.ImageCatalog()
	if len(idx.Names) != 1 || idx.Images[unnamed].Version != "3" || idx.Defaults[cfg.ID] != first {
		t.Fatal("unnamed import invented a name or moved the default", idx)
	}
	checkClean(t, ctx)
}

func TestImportRejectsMalformedArchives(t *testing.T) {
	valid := archive(t, fixtureArchive())
	badGzip := compressed(t, valid)
	badGzip[len(badGzip)-8] ^= 0xff
	cases := map[string][]byte{
		"empty": nil, "no end markers": valid[:len(valid)-1024], "one end marker": valid[:len(valid)-512],
		"truncated file": valid[:4*512+2], "truncated gzip": compressed(t, valid)[:12],
		"bad gzip checksum": badGzip, "trailing data": append(slices.Clone(valid), bytes.Repeat([]byte{'x'}, 512)...),
		"partial padding": append(slices.Clone(valid), 0), "second tar": append(slices.Clone(valid), valid...),
		"missing app": archive(t, fixtureArchive()[1:]),
	}
	for name, e := range map[string]entry{
		"traversal": {"app/../outside", "x", tar.TypeReg}, "absolute": {"/outside", "x", tar.TypeReg},
		"drive path": {"C:/outside", "x", tar.TypeReg}, "backslash": {`app\outside`, "x", tar.TypeReg},
		"device": {"data/NUL", "x", tar.TypeReg}, "stream": {"data/file:ads", "x", tar.TypeReg},
		"trailing dot": {"data/file.", "x", tar.TypeReg}, "trailing space": {"data/file ", "x", tar.TypeReg},
		"symlink": {"data/link", "", tar.TypeSymlink}, "hardlink": {"data/link", "", tar.TypeLink},
		"fifo": {"data/pipe", "", tar.TypeFifo}, "unknown root": {"other/file", "x", tar.TypeReg},
		"duplicate": {"app/program.exe", "x", tar.TypeReg}, "case duplicate": {"app/PROGRAM.EXE", "x", tar.TypeReg},
		"file as directory": {"app/program.exe/child", "x", tar.TypeReg},
		"extra runtime":     {"runtimes/runtime-extra/", "", tar.TypeDir},
	} {
		cases[name] = archive(t, append(fixtureArchive(), e))
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := fixture(t)
			before, _ := os.ReadFile(ctx.Store.Path("images", "index.json"))
			if _, err := Import(stdcontext.Background(), ctx, bytes.NewReader(b), bytes.NewReader(configJSON(t, fixtureConfig())), Options{}); err == nil {
				t.Fatal("accepted malformed input")
			}
			if after, _ := os.ReadFile(ctx.Store.Path("images", "index.json")); !bytes.Equal(before, after) {
				t.Fatal("failed validation changed the catalog")
			}
			if blobs, err := os.ReadDir(ctx.Store.Path("blobs", "sha256")); err != nil || len(blobs) != 0 {
				t.Fatal("failed validation published blobs", blobs, err)
			}
			checkClean(t, ctx)
		})
	}
}

func TestImportConfigurationAndRuntimeValidation(t *testing.T) {
	ctx := fixture(t)
	b := archive(t, fixtureArchive())
	for _, cfg := range []string{"null", "{}", "[]", "{", strings.Repeat(" ", maxConfigSize+1)} {
		if _, err := Import(stdcontext.Background(), ctx, bytes.NewReader(b), strings.NewReader(cfg), Options{}); err == nil {
			t.Fatal("accepted invalid config")
		}
	}
	for _, target := range []string{"sha256:" + strings.Repeat("a", 64), "example.test/app@sha256:" + strings.Repeat("a", 64)} {
		if _, err := Import(stdcontext.Background(), ctx, bytes.NewReader(b), bytes.NewReader(configJSON(t, fixtureConfig())), Options{Target: target}); err == nil {
			t.Fatal("accepted digest target")
		}
	}
	cfg := fixtureConfig()
	cfg.From = map[string]oci.Runtime{"missing": {ID: "missing", Version: "1", Image: "sha256:" + strings.Repeat("a", 64)}}
	if _, err := Import(stdcontext.Background(), ctx, bytes.NewReader(b), bytes.NewReader(configJSON(t, cfg)), Options{}); err == nil || !strings.Contains(err.Error(), "runtime-missing") {
		t.Fatal("accepted missing runtime", err)
	}
	for _, workdir := range []string{"absent", "${data}/absent", "program.exe"} {
		cfg := fixtureConfig()
		cfg.Workdir = workdir
		if _, err := Import(stdcontext.Background(), ctx, bytes.NewReader(b), bytes.NewReader(configJSON(t, cfg)), Options{}); err == nil || !strings.Contains(err.Error(), "WORKDIR") {
			t.Fatal("accepted invalid working directory", err)
		}
	}
	cfg = fixtureConfig()
	cfg.Entrypoint = nil
	mustImport(t, ctx, b, cfg, Options{Changes: []string{`CMD ["program.exe"]`}})
	checkClean(t, ctx)
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestImportReadErrorsCancellationAndCorruptDestination(t *testing.T) {
	ctx := fixture(t)
	cfg := fixtureConfig()
	b := archive(t, fixtureArchive())
	broken := errors.New("broken input")
	for _, prefix := range [][]byte{nil, b[:4*512+1], b} {
		src := io.MultiReader(bytes.NewReader(prefix), errorReader{broken})
		if _, err := Import(stdcontext.Background(), ctx, src, bytes.NewReader(configJSON(t, cfg)), Options{}); !errors.Is(err, broken) {
			t.Fatal("late read error was lost", err)
		}
		checkClean(t, ctx)
	}
	cancelled, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()
	if _, err := Import(cancelled, ctx, bytes.NewReader(b), bytes.NewReader(configJSON(t, cfg)), Options{}); !errors.Is(err, stdcontext.Canceled) {
		t.Fatal("cancellation was lost", err)
	}
	digest := mustImport(t, ctx, b, cfg, Options{})
	img, _ := ctx.Store.ReadImage(cfg.ID, digest)
	app := oci.OwnLayer(&img.Manifest)
	if err := os.WriteFile(ctx.Store.BlobPath(oci.DigestHex(app.Digest)), bytes.Repeat([]byte{'x'}, int(app.Size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(stdcontext.Background(), ctx, bytes.NewReader(b), bytes.NewReader(configJSON(t, cfg)), Options{}); !errors.Is(err, store.ErrCorruptBlob) {
		t.Fatal("reused corrupt destination blob", err)
	}
	checkClean(t, ctx)
}

func TestImportConcurrentNameConflict(t *testing.T) {
	ctx := fixture(t)
	b := archive(t, fixtureArchive())
	configs := [][]byte{}
	for _, version := range []string{"1", "2"} {
		cfg := fixtureConfig()
		cfg.Version = version
		configs = append(configs, configJSON(t, cfg))
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, cfg := range configs {
		go func() {
			<-start
			_, err := Import(stdcontext.Background(), ctx, bytes.NewReader(b), bytes.NewReader(cfg), Options{Target: "example.test/app:conflict"})
			results <- err
		}()
	}
	close(start)
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			success++
		case strings.Contains(err.Error(), "--force"):
			conflict++
		default:
			t.Fatal(err)
		}
	}
	idx, err := ctx.Store.ImageCatalog()
	if err != nil || success != 1 || conflict != 1 || len(idx.Images) != 1 || len(idx.Names) != 1 {
		t.Fatal("concurrent import was not atomic", success, conflict, idx, err)
	}
	checkClean(t, ctx)
}
