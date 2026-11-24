package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
)

func seededStore(t *testing.T) (*Store, string) {
	t.Helper()
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	for _, dir := range []string{"home", "empty", "mask"} {
		if err := os.MkdirAll(filepath.Join(data, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for file, value := range map[string]string{"home/file": "first", "deleted": "remove", "mask/file": "hidden"} {
		if err := os.WriteFile(filepath.Join(data, filepath.FromSlash(file)), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seed, err := st.WriteDataLayer(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	// Times and enumeration order do not affect canonical file snapshots.
	second, err := st.WriteDataLayer(context.Background(), data)
	if err != nil || seed.Digest != second.Digest {
		t.Fatalf("nondeterministic seed: %+v %+v %v", seed, second, err)
	}
	var archive bytes.Buffer
	lw := format.NewLayerWriter(&archive)
	if err := lw.AddFile("app.exe", 3, bytes.NewBufferString("app")); err != nil {
		t.Fatal(err)
	}
	if err := lw.Finish(); err != nil {
		t.Fatal(err)
	}
	app, err := st.PutBlob(archive.Bytes(), oci.MediaTypeLayerTar)
	if err != nil {
		t.Fatal(err)
	}
	cfg := oci.AppConfig{FormatVersion: 4, ID: "snapshot-fixture", Version: "1", Entrypoint: []string{"app.exe"}, DataLayer: seed.Digest}
	b, _ := oci.ToJSON(cfg)
	config, err := st.PutBlob(b, oci.MediaTypeConfigV4)
	if err != nil {
		t.Fatal(err)
	}
	m := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: config, Layers: []oci.Descriptor{seed, app}}
	b, _ = oci.ToJSON(m)
	manifest, err := st.PutBlob(b, oci.MediaTypeManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterImage(Ref{ID: cfg.ID, Version: cfg.Version, Manifest: manifest.Digest}, "", false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, l := range m.Layers {
			if _, err := st.DeleteDeployment(l.Digest); err != nil {
				t.Error(err)
			}
		}
	})
	return st, manifest.Digest
}

func TestSeededDataDiffAndMountMask(t *testing.T) {
	st, image := seededStore(t)
	c, err := st.CreateContainer("seeded", ContainerRecord{Image: image, HostConfig: ContainerHostConfig{Mounts: []mounts.Spec{{Type: "volume", Source: "masked", Target: "${data}/mask"}}}}, st.SeedContainerData)
	if err != nil {
		t.Fatal(err)
	}
	if changes, err := st.ContainerDiff(c.ID); err != nil || len(changes) != 0 {
		t.Fatalf("initial changes: %v %v", changes, err)
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "mask")); !os.IsNotExist(err) {
		t.Fatalf("copied masked content: %v", err)
	}
	if err := os.WriteFile(filepath.Join(c.DataDir, "home", "file"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.DataDir, "deleted")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.DataDir, "added"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.DataDir, "empty")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.DataDir, "empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	want := []DiffEntry{{'A', "/added"}, {'D', "/deleted"}, {'C', "/empty"}, {'C', "/home/file"}}
	if got, err := st.ContainerDiff(c.ID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("changes: %v %v want %v", got, err, want)
	}
	root, err := st.CreateContainer("root-mounted", ContainerRecord{Image: image, HostConfig: ContainerHostConfig{Mounts: []mounts.Spec{{Type: "volume", Source: "root", Target: "${data}"}}}}, st.SeedContainerData)
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root.DataDir); err != nil || len(entries) != 0 {
		t.Fatalf("root-mounted container seeded: %v %v", entries, err)
	}
	if changes, err := st.ContainerDiff(root.ID); err != nil || len(changes) != 0 {
		t.Fatalf("root mount diff: %v %v", changes, err)
	}
}

func TestSeedFailureNotPublished(t *testing.T) {
	st, image := seededStore(t)
	img, _ := st.ReadImage("fixture", image)
	if err := os.Remove(st.BlobPath(oci.DigestHex(img.Config.DataLayer))); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateContainer("failed", ContainerRecord{Image: image}, st.SeedContainerData); err == nil {
		t.Fatal("accepted missing seed")
	}
	if _, err := st.ResolveContainer("failed"); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("published failed initialization: %v", err)
	}
	entries, err := os.ReadDir(st.Path("containers"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if oci.IsSha256Hex(entry.Name()) {
			t.Fatalf("orphaned container directory: %s", entry.Name())
		}
	}
}

func TestCaptureRefusesReparseAndCancellation(t *testing.T) {
	source := t.TempDir()
	outside := t.TempDir()
	dest := t.TempDir()
	link := mounts.Mount{Source: outside, Destination: filepath.Join(source, "junction")}
	if err := mounts.Attach(source, link); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mounts.Detach(source, link) }()
	if err := CopyData(context.Background(), source, dest, nil); err == nil {
		t.Fatal("followed unconfigured junction")
	}
	if err := CopyData(context.Background(), source, dest, map[string]bool{"junction": true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CopyData(ctx, source, dest, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
}
