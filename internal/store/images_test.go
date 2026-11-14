package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
)

func catalogFixture(t *testing.T, st *Store, version string) Ref {
	t.Helper()
	config := oci.AppConfig{ID: "test.images.app", Version: version, Command: "app.exe"}
	cfg, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.PutBlob(cfg, oci.MediaTypeConfig)
	if err != nil {
		t.Fatal(err)
	}
	l, err := st.PutBlob([]byte("layer"), oci.MediaTypeLayerTar)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: c, Layers: []oci.Descriptor{l}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.PutBlob(b, oci.MediaTypeManifest)
	if err != nil {
		t.Fatal(err)
	}
	return Ref{ID: config.ID, Version: version, Manifest: m.Digest, Source: "localhost:5000/app:" + version}
}

func TestImageCatalogVersionsNamesAndRemoval(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	a, b := catalogFixture(t, st, "1"), catalogFixture(t, st, "2")
	for _, r := range []Ref{a, b} {
		if err := st.RegisterImage(r, r.Source, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []Ref{a, b} {
		got, err := st.ResolveLocalImage("", r.Source)
		if err != nil || got.Manifest != r.Manifest {
			t.Fatalf("exact selection: %v %v", got, err)
		}
	}
	if err := st.TagImage("", a.Manifest, "localhost:5000/app:latest"); err != nil {
		t.Fatal(err)
	}
	if err := st.TagImage("", b.Manifest, "localhost:5000/app:latest"); err != nil {
		t.Fatal(err)
	}
	def, _ := st.ReadRef(a.ID)
	if def == nil || def.Manifest != b.Manifest {
		t.Fatalf("tagging changed default: %v", def)
	}
	for _, text := range []string{a.Manifest, "localhost:5000/app@" + a.Manifest} {
		got, err := st.ResolveLocalImage("", text)
		if err != nil || got.Manifest != a.Manifest {
			t.Fatalf("digest selection: %v %v", got, err)
		}
	}
	if _, last, err := st.RemoveImage("", b.Source); err != nil || last {
		t.Fatalf("untag: %v %v", last, err)
	}
	if _, err := st.ResolveLocalImage("", b.Manifest); err != nil {
		t.Fatal("remaining alias lost image:", err)
	}
	if _, last, err := st.RemoveImage("", b.Manifest); err != nil || last {
		t.Fatalf("remove second version: %v %v", last, err)
	}
	if _, err := st.ResolveLocalImage("", b.Manifest); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("removed image resolved: %v", err)
	}
	if _, last, err := st.RemoveImage("", a.Manifest); err != nil || !last {
		t.Fatalf("remove final version: %v %v", last, err)
	}
}

func TestImageRunLeasesAndCrashState(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	a, b := catalogFixture(t, st, "1"), catalogFixture(t, st, "2")
	for _, r := range []Ref{a, b} {
		if err := st.RegisterImage(r, r.Source, true); err != nil {
			t.Fatal(err)
		}
	}
	x, y, err := st.BeginImageRun(a)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Release()
	defer y.Release()
	x2, y2, err := st.BeginImageRun(a)
	if err != nil {
		t.Fatal("same version cannot join:", err)
	}
	if _, _, err := st.BeginImageRun(b); err == nil {
		t.Fatal("conflicting version started")
	}
	if _, _, err := st.RemoveImage("", a.Manifest); err == nil {
		t.Fatal("running image removed")
	}
	x.Release()
	y.Release()
	if _, _, err := st.BeginImageRun(b); err == nil {
		t.Fatal("second running lease was ignored")
	}
	x2.Release()
	y2.Release()
	// The active-manifest file intentionally remains, as it does after a crash.
	x3, y3, err := st.BeginImageRun(b)
	if err != nil {
		t.Fatal("stale state blocked next run:", err)
	}
	x3.Release()
	y3.Release()
}

func TestImageCatalogMigrationIsOneTimeAndRequiresIdleStore(t *testing.T) {
	root := t.TempDir()
	st, err := Open(root, false, "")
	if err != nil {
		t.Fatal(err)
	}
	r := catalogFixture(t, st, "1")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(st.refPath(r.ID), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(st.Path("images", "index.json")); err != nil {
		t.Fatal(err)
	}
	lease, err := OpenLock(st.RunsLockPath(r.ID), false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, false, ""); err == nil || !strings.Contains(err.Error(), "finish existing runs") {
		t.Fatalf("active migration: %v", err)
	}
	lease.Release()
	st, err = Open(root, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.ReadRef(r.ID); err != nil || got == nil || got.Manifest != r.Manifest {
		t.Fatalf("migration: %v %v", got, err)
	}
	if _, _, err := st.RemoveImage("", r.Manifest); err != nil {
		t.Fatal(err)
	}
	st, err = Open(root, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if refs, err := st.ListImages(); err != nil || len(refs) != 0 {
		t.Fatalf("legacy refs resurrected: %v %v", refs, err)
	}
	if _, err := os.Stat(filepath.Join(root, "refs", r.ID+".json")); err != nil {
		t.Fatal("legacy metadata was lost:", err)
	}
}

func TestImageCatalogConcurrentWritersAndPruneDryRun(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	r := catalogFixture(t, st, "1")
	if err := st.RegisterImage(r, r.Source, true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := st.TagImage("", r.Manifest, "localhost:5000/tag"+strings.Repeat("a", i+1)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	idx, err := st.ImageCatalog()
	if err != nil || len(idx.Names) != 13 {
		t.Fatalf("lost catalog update: %v %v", idx, err)
	}
	old := catalogFixture(t, st, "0")
	if err := st.RegisterImage(old, "localhost:5000/moving:latest", false); err != nil {
		t.Fatal(err)
	}
	if err := st.TagImage("", r.Manifest, "localhost:5000/moving:latest"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(st.Path("images", "index.json"))
	removed, err := st.PruneImages(true)
	if err != nil || len(removed) != 1 || removed[0].Manifest != old.Manifest {
		t.Fatalf("dry run: %v %v", removed, err)
	}
	after, _ := os.ReadFile(st.Path("images", "index.json"))
	if string(before) != string(after) {
		t.Fatal("dry run rewrote catalog")
	}
	if _, err := st.PruneImages(false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResolveLocalImage("", old.Manifest); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("unnamed image survived prune: %v", err)
	}
	if _, err := st.ResolveLocalImage("", r.Manifest); err != nil {
		t.Fatal("named image was pruned:", err)
	}
}

func TestBuiltImageDigestSurvivesTagMovement(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	a, b := catalogFixture(t, st, "1"), catalogFixture(t, st, "2")
	a.Source = `C:\recipes\app.json`
	b.Source = a.Source
	for _, ref := range []Ref{a, b} {
		if err := st.RegisterImage(ref, "localhost:5000/built:latest", true); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ResolveLocalImage("", "localhost:5000/built@"+a.Manifest)
	if err != nil || got.Manifest != a.Manifest {
		t.Fatalf("built digest lookup after tag movement: %v %v", got, err)
	}
	if _, err := st.ResolveLocalImage("", "localhost:5000/unrelated@"+a.Manifest); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("invented repository provenance: %v", err)
	}
}
