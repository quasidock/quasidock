package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/lazy"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestSystemPruneConfirmation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	t.Setenv("QUASIDOCK_HOME", root)
	ctx := context.New()
	var out, stderr bytes.Buffer
	ctx.Stdout, ctx.Stderr = &out, &stderr
	for _, args := range [][]string{{"--data"}, {"--all=maybe"}, {"--filter"}, {"--filter=until="}, {"--filter=until=yesterday"}, {"--filter=label=keep"}} {
		if code, err := systemPrune(ctx, args, nil, networkBackend{}); code != 1 || err == nil || stderr.Len() != 0 || out.Len() != 0 {
			t.Fatalf("invalid arguments %v: %d %v %q %q", args, code, err, &out, &stderr)
		}
	}
	for _, answer := range []string{"n\n", "\n", ""} {
		stderr.Reset()
		if code, err := systemPrune(ctx, []string{"--volumes", "--networks"}, strings.NewReader(answer), networkBackend{}); code != 0 || err != nil || out.Len() != 0 {
			t.Fatalf("cancel %q: %d %v %q", answer, code, err, &out)
		}
		if !strings.Contains(stderr.String(), "unused named volumes") || !strings.Contains(stderr.String(), "unused tracked private networks") || strings.Count(stderr.String(), "Continue?") != 1 {
			t.Fatalf("prompt: %q", &stderr)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("validation or cancellation opened the store: %v", err)
	}
	stderr.Reset()
	if code, err := systemPrune(ctx, nil, strings.NewReader(" YeS\r\n"), networkBackend{}); code != 0 || err != nil || out.String() != "Total reclaimed space: 0 B\n" {
		t.Fatalf("confirmed command: %d %v %q", code, err, &out)
	}
	for _, args := range [][]string{{"--force"}, {"--dry-run"}} {
		stderr.Reset()
		if code, err := systemPrune(ctx, args, nil, networkBackend{}); code != 0 || err != nil || stderr.Len() != 0 {
			t.Fatalf("nonprompting arguments %v: %d %v %q", args, code, err, &stderr)
		}
	}
}

func systemPruneFixture(t *testing.T) (*context.Context, *bytes.Buffer) {
	t.Helper()
	t.Setenv("QUASIDOCK_GRANTS_KEY", `Software\Quasidock\Tests\SystemPrune`+time.Now().Format("150405.000000000"))
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	return ctx, out
}

func pruneImageFixture(t *testing.T, st *store.Store, id, name string, catalog bool, layer oci.Descriptor) (string, uint64) {
	t.Helper()
	put := func(value any, mediaType string) oci.Descriptor {
		t.Helper()
		blob, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		desc, err := st.PutBlob(blob, mediaType)
		if err != nil {
			t.Fatal(err)
		}
		return desc
	}
	config := put(oci.AppConfig{ID: id, Version: "1", Entrypoint: []string{"app.exe"}}, oci.MediaTypeConfig)
	manifest := put(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: config, Layers: []oci.Descriptor{layer}}, oci.MediaTypeManifest)
	if catalog {
		if err := st.RegisterImage(store.Ref{ID: id, Version: "1", Manifest: manifest.Digest, Source: name}, name, name != ""); err != nil {
			t.Fatal(err)
		}
	}
	return manifest.Digest, uint64(config.Size + manifest.Size)
}

func pruneFixtureBlob(t *testing.T, st *store.Store, value string) oci.Descriptor {
	t.Helper()
	desc, err := st.PutBlob([]byte(value), oci.MediaTypeLayerTar)
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

func writePruneFixture(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setPruneAge(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestSystemPruneCascade(t *testing.T) {
	ctx, out := systemPruneFixture(t)
	st := ctx.Store
	shared := pruneFixtureBlob(t, st, "shared layer")
	buildLayer := pruneFixtureBlob(t, st, "build-only layer")
	orphan := pruneFixtureBlob(t, st, "previously orphaned layer")
	lazyState := lazy.DirPath(st, oci.DigestHex(buildLayer.Digest))
	const lazyPayload = "build-only streaming state"
	writePruneFixture(t, filepath.Join(lazyState, "partial"), lazyPayload)
	named, _ := pruneImageFixture(t, st, "prune-named", "example.test/named:1", true, shared)
	dangling, danglingBytes := pruneImageFixture(t, st, "prune-dangling", "", true, shared)
	leased, _ := pruneImageFixture(t, st, "prune-leased", "", true, shared)
	build, buildBytes := pruneImageFixture(t, st, "prune-build-only", "", false, buildLayer)
	buildResult := strings.Repeat("a", 64)
	if err := st.MarkBuildImage(build); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteBuildCache(buildResult, build); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteBuildCache(strings.Repeat("b", 64), build); err != nil {
		t.Fatal(err)
	}
	cacheBytes := uint64(2*len(build) + len(lazyPayload))
	for _, area := range []string{"downloads", "tools", "work"} {
		writePruneFixture(t, st.Path("cache", area, "entry", "payload"), area)
		cacheBytes += uint64(len(area))
	}
	appLock, imageLock, err := st.BeginImageRun(store.Ref{ID: "prune-leased", Manifest: leased})
	if err != nil {
		t.Fatal(err)
	}
	defer appLock.Release()
	defer imageLock.Release()
	container, err := st.CreateContainer("stopped", store.ContainerRecord{Image: dangling, HostConfig: store.ContainerHostConfig{
		Mounts: []mounts.Spec{{Type: "volume", Source: "cascade-volume", Target: "${data}/volume"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	active, err := st.CreateContainer("busy", store.ContainerRecord{Image: named})
	if err != nil {
		t.Fatal(err)
	}
	runLock, err := store.OpenLock(st.ContainerRunLockPath(active.ID), true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer runLock.Release()
	guarded, err := st.CreateVolume("guarded-volume")
	if err != nil {
		t.Fatal(err)
	}
	writePruneFixture(t, st.Path("cache", "work", "active", "payload"), "active work")
	// A running mount can protect a cache entry and a volume without a saved volume reference.
	session, err := grants.BeginMounts(st, active, oci.SandboxSoft, []grants.MountRequest{{Path: guarded.Mountpoint}, {Path: st.Path("cache", "work", "active")}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stoppedBytes := mounts.TreeSize(st.ContainerDir(container.ID))
	want := stoppedBytes + danglingBytes + buildBytes + uint64(buildLayer.Size) + cacheBytes
	before := imagePruneSnapshot(t, st.Root)
	invoke := func(args ...string) string {
		t.Helper()
		out.Reset()
		code, err := Dispatch(ctx, append([]string{"system", "prune"}, args...))
		if code != 0 || err != nil {
			t.Fatalf("prune %v: %d %v %q", args, code, err, out)
		}
		return out.String()
	}
	preview := invoke("--dry-run")
	for _, item := range []string{container.ID, dangling, "builds/" + buildResult, "images/" + oci.DigestHex(build), "tools/entry", "Would reclaim: " + lifecycle.FormatBytes(want)} {
		if !strings.Contains(preview, item) {
			t.Errorf("preview omitted %q: %s", item, preview)
		}
	}
	if volumePreview := invoke("--dry-run", "--volumes"); !strings.Contains(volumePreview, "cascade-volume") || strings.Contains(volumePreview, "guarded-volume") {
		t.Fatalf("volume preview: %s", volumePreview)
	}
	if !reflect.DeepEqual(before, imagePruneSnapshot(t, st.Root)) {
		t.Fatal("preview changed the store")
	}
	if applied := invoke("-f"); !strings.Contains(applied, "Total reclaimed space: "+lifecycle.FormatBytes(want)) || strings.Contains(applied, "cascade-volume") {
		t.Fatalf("applied: %s", applied)
	}
	for _, removed := range []string{st.ContainerDir(container.ID), st.BlobPath(oci.DigestHex(dangling)), st.BlobPath(oci.DigestHex(build)), st.BlobPath(oci.DigestHex(buildLayer.Digest)), st.Path("cache", "builds", buildResult), lazyState} {
		if store.Exists(removed) {
			t.Errorf("prune left %s", removed)
		}
	}
	for _, kept := range []string{st.ContainerDir(active.ID), st.BlobPath(oci.DigestHex(named)), st.BlobPath(oci.DigestHex(leased)), st.BlobPath(oci.DigestHex(shared.Digest)), st.BlobPath(oci.DigestHex(orphan.Digest)), st.Path("volumes", "cascade-volume"), st.Path("cache", "work", "active", "payload")} {
		if !store.Exists(kept) {
			t.Errorf("prune removed %s", kept)
		}
	}
	if applied := invoke("-f", "--volumes"); !strings.Contains(applied, "cascade-volume") || strings.Contains(applied, "guarded-volume") || store.Exists(st.Path("volumes", "cascade-volume")) {
		t.Fatalf("applied volumes: %s", applied)
	}
}

func TestSystemPruneAgeAndNetworks(t *testing.T) {
	ctx, out := systemPruneFixture(t)
	st := ctx.Store
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	cutoff := old.AddDate(1, 0, 0)
	selected := privateTestNetwork(1, "selected")
	busy := privateTestNetwork(2, "busy")
	newer := privateTestNetwork(3, "newer")
	foreign := privateTestNetwork(4, "foreign")
	for _, n := range []win32.HCNNetwork{selected, busy, newer} {
		if err := st.TrackNetwork(n.ID); err != nil {
			t.Fatal(err)
		}
		age := old
		if n.ID == newer.ID {
			age = cutoff
		}
		setPruneAge(t, st.Path("networks", n.ID), age)
	}
	layer := pruneFixtureBlob(t, st, "retained build layer")
	digest, _ := pruneImageFixture(t, st, "prune-aged", "example.test/aged:1", true, layer)
	idx, err := st.ImageCatalog()
	if err != nil {
		t.Fatal(err)
	}
	ref := idx.Images[digest]
	ref.CachedAt = old.Format(time.RFC3339Nano)
	idx.Images[digest] = ref
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	writePruneFixture(t, st.Path("images", "index.json"), string(data))
	if err := st.MarkBuildImage(digest); err != nil {
		t.Fatal(err)
	}
	setPruneAge(t, st.Path("cache", "images", oci.DigestHex(digest)), old)
	cacheKey := strings.Repeat("a", 64)
	if err := st.WriteBuildCache(cacheKey, digest); err != nil {
		t.Fatal(err)
	}
	setPruneAge(t, st.Path("cache", "builds", cacheKey), cutoff)
	record, err := st.CreateContainer("old", store.ContainerRecord{Image: digest, Created: old.Format(time.RFC3339Nano), HostConfig: store.ContainerHostConfig{
		Networks: map[string]string{selected.ID: selected.Name},
	}})
	if err != nil {
		t.Fatal(err)
	}
	unknownDigest, _ := pruneImageFixture(t, st, "prune-unknown", "", true, layer)
	unknown, err := st.CreateContainer("unknown-age", store.ContainerRecord{Image: unknownDigest, Created: "invalid"})
	if err != nil {
		t.Fatal(err)
	}
	modified := st.Path("cache", "work", "new-child", "child")
	writePruneFixture(t, modified, "fresh")
	setPruneAge(t, modified, cutoff)
	setPruneAge(t, filepath.Dir(modified), old)
	oldVolume, err := st.CreateVolume("old-volume")
	if err != nil {
		t.Fatal(err)
	}
	oldVolume.CreatedAt = old.Format(time.RFC3339Nano)
	data, err = json.Marshal(oldVolume)
	if err != nil {
		t.Fatal(err)
	}
	writePruneFixture(t, filepath.Join(filepath.Dir(oldVolume.Mountpoint), "volume.json"), string(data))
	if _, err := st.CreateVolume("fresh-volume"); err != nil {
		t.Fatal(err)
	}
	options := []string{"--volumes", "--all", "--filter=until=" + old.Format(time.RFC3339), "--filter", "until=" + cutoff.Format(time.RFC3339)}
	if code, err := systemPrune(ctx, append(slices.Clone(options), "--dry-run"), nil, networkBackend{}); code != 0 || err != nil || strings.Contains(out.String(), selected.ID) {
		t.Fatalf("network opt-in: %d %v %q", code, err, out)
	}
	out.Reset()
	var deleted []string
	backend := networkBackend{
		elevated:     func() (bool, error) { return false, nil },
		lockMutation: testNetworkMutationLock,
		list:         func() ([]win32.HCNNetwork, error) { return []win32.HCNNetwork{selected, busy, newer, foreign}, nil },
		hasEndpoints: func(id string) (bool, error) { return id == busy.ID, nil },
		deleteNetwork: func(id string) error {
			deleted = append(deleted, id)
			return nil
		},
	}
	options = append(options, "--networks")
	apply := append(slices.Clone(options), "--force")
	if code, err := systemPrune(ctx, apply, nil, backend); code != 1 || err == nil || !strings.Contains(err.Error(), "elevated") || !store.Exists(st.ContainerDir(record.ID)) {
		t.Fatalf("unelevated preflight: %d %v %q", code, err, out)
	}
	backend.elevated = func() (bool, error) { return true, nil }
	unavailable := backend
	unavailable.list = func() ([]win32.HCNNetwork, error) { return nil, win32.ErrHCNUnavailable }
	if code, err := systemPrune(ctx, apply, nil, unavailable); code != 1 || !errors.Is(err, win32.ErrHCNUnavailable) || !store.Exists(st.ContainerDir(record.ID)) {
		t.Fatalf("HCN preflight: %d %v %q", code, err, out)
	}
	preview := append(slices.Clone(options), "--dry-run")
	for _, args := range [][]string{preview, apply} {
		out.Reset()
		if code, err := systemPrune(ctx, args, nil, backend); code != 0 || err != nil {
			t.Fatalf("filtered prune %v: %d %v %q", args, code, err, out)
		}
		for _, item := range []string{record.ID, digest, selected.ID, "old-volume", "images/" + oci.DigestHex(digest)} {
			if !strings.Contains(out.String(), item) {
				t.Errorf("missing %q in %s", item, out)
			}
		}
		for _, item := range []string{unknown.ID, busy.ID, newer.ID, foreign.ID, "fresh-volume", "work/new-child", "builds/" + cacheKey} {
			if strings.Contains(out.String(), item) {
				t.Errorf("unexpected %q in %s", item, out)
			}
		}
	}
	if !slices.Equal(deleted, []string{selected.ID}) || store.Exists(st.Path("networks", selected.ID)) || !store.Exists(st.Path("networks", busy.ID)) {
		t.Fatalf("network deletion and ownership: %v", deleted)
	}
	if !st.HasBlob(oci.DigestHex(digest)) || !store.Exists(modified) || !store.Exists(st.ContainerDir(unknown.ID)) {
		t.Fatal("prune lost content reachable through a retained marker or record")
	}
}

func TestSystemPrunePartialCacheFailure(t *testing.T) {
	ctx, out := systemPruneFixture(t)
	st := ctx.Store
	layer := pruneFixtureBlob(t, st, "prune failure layer")
	digest, _ := pruneImageFixture(t, st, "prune-failure", "", true, layer)
	if err := st.MarkBuildImage(digest); err != nil {
		t.Fatal(err)
	}
	container, err := st.CreateContainer("stopped", store.ContainerRecord{Image: digest})
	if err != nil {
		t.Fatal(err)
	}
	marker := st.Path("cache", "images", oci.DigestHex(digest))
	lock, err := win32.CreateFile(marker, win32.GENERIC_READ, win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE, nil, win32.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(lock)
	writePruneFixture(t, st.Path("cache", "downloads", "before-failure"), "completed")
	code, err := Dispatch(ctx, []string{"system", "prune", "--force"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "remove build cache entry images/") || !strings.Contains(out.String(), container.ID) || !strings.Contains(out.String(), "downloads/before-failure") {
		t.Fatalf("partial failure: %d %v %q", code, err, out)
	}
	if strings.Contains(out.String(), "Deleted Images") || !store.Exists(marker) || !st.HasBlob(oci.DigestHex(digest)) || !st.HasBlob(oci.DigestHex(layer.Digest)) {
		t.Fatal("prune deleted dependent image content after failing to remove a build marker")
	}
}
