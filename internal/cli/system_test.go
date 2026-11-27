package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/version"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestSystemDispatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	t.Setenv("QUASIDOCK_HOME", root)
	t.Setenv("QUASIDOCK_REGISTRY", "example.test/quasidock")
	ctx := context.New()
	var out bytes.Buffer
	ctx.Stdout = &out
	for _, args := range [][]string{{"system", "--help"}, {"system", "info", "-h"}, {"system", "df", "--help"}, {"system", "prune", "--help"}} {
		out.Reset()
		if code, err := Dispatch(ctx, args); code != 0 || err != nil || !strings.Contains(out.String(), "usage: quasidock system") {
			t.Fatalf("%v returned %d, %v, %q", args, code, err, &out)
		}
	}
	for _, args := range [][]string{
		{"system"}, {"system", "prune", "target"}, {"system", "info", "target"}, {"system", "info", "-v"},
		{"system", "df", "--format"}, {"system", "df", "--format", "{{"}, {"system", "df", "--format", "table "},
		{"system", "df", "-v", "--format=json"}, {"system", "info", "--format="},
	} {
		out.Reset()
		if code, err := Dispatch(ctx, args); code != 1 || err == nil || out.Len() != 0 {
			t.Fatalf("%v returned %d, %v, %q", args, code, err, &out)
		}
	}
	if code, err := Dispatch(ctx, []string{"info"}); code != 1 || err == nil || !strings.Contains(err.Error(), "image info") {
		t.Fatalf("top-level info returned %d, %v", code, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("argument handling touched the store: %v", err)
	}

	invoke := func(args ...string) string {
		t.Helper()
		out.Reset()
		if code, err := Dispatch(ctx, append([]string{"system"}, args...)); code != 0 || err != nil {
			t.Fatalf("%v returned %d, %v", args, code, err)
		}
		return out.String()
	}
	var info systemInfoReport
	if err := json.Unmarshal([]byte(invoke("info", "--format=json")), &info); err != nil {
		t.Fatal(err)
	}
	if info.Version != version.Version || info.OSType != "windows" || info.OSVersion == "" || info.Architecture == "" ||
		info.CPUs == 0 || info.MemTotal == 0 || info.StoreRoot != ctx.Store.FullRoot() || info.Registry != "example.test/quasidock" ||
		info.Images != 0 || info.Containers != 0 || info.Volumes != 0 || info.TrackedNetworks != 0 {
		t.Fatalf("empty-store info: %+v", info)
	}
	if got := invoke("info"); !strings.Contains(got, "Store Root: "+info.StoreRoot) || !strings.Contains(got, "Containers: 0") {
		t.Fatalf("text info: %q", got)
	}
	if got := invoke("info", "-f", "{{.Registry}}:{{.Containers}}"); got != "example.test/quasidock:0\n" {
		t.Fatalf("info template: %q", got)
	}
	df := invoke("df", "--format", "json")
	lines := strings.Split(strings.TrimSpace(df), "\n")
	categories := []string{"Images", "Containers", "Local Volumes", "Build Cache"}
	if len(lines) != len(categories) {
		t.Fatalf("expected four JSON lines: %q", df)
	}
	for i, line := range lines {
		var row systemDFRow
		if err := json.Unmarshal([]byte(line), &row); err != nil || row != (systemDFRow{categories[i], "0", "0", "0 B", "0 B"}) {
			t.Fatalf("unexpected df row %d: %+v, %v", i, row, err)
		}
	}
	if got := invoke("df", "--format", "{{.Type}}={{.TotalCount}}"); got != "Images=0\nContainers=0\nLocal Volumes=0\nBuild Cache=0\n" {
		t.Fatalf("df template: %q", got)
	}
	if got := invoke("df", "--format", "table {{.Type}}\t{{.Size}}"); !strings.Contains(got, "TYPE") || !strings.Contains(got, "SIZE") || !strings.Contains(got, "Build Cache") {
		t.Fatalf("custom table: %q", got)
	}
	if got := invoke("df"); !strings.Contains(got, "RECLAIMABLE") || !strings.Contains(got, "Local Volumes") || strings.Count(got, "\n") != 5 {
		t.Fatalf("default summary: %q", got)
	}
	if got := invoke("df", "-v", "--format=table"); !strings.Contains(got, "SHARED SIZE") || !strings.Contains(got, "Build Cache space usage") {
		t.Fatalf("verbose output: %q", got)
	}
	if got := invoke("events", "--help"); !strings.Contains(got, eventsUsage) {
		t.Fatalf("events help: %q", got)
	}
}

func TestSystemDiskUsage(t *testing.T) {
	ledger := fmt.Sprintf(`Software\Quasidock\Tests\SystemDF%d`, time.Now().UnixNano())
	t.Setenv("QUASIDOCK_GRANTS_KEY", ledger)
	t.Cleanup(func() {
		root := regkey.Key{Handle: win32.HKEY_CURRENT_USER}
		_ = root.DeleteTree(ledger)
	})
	ctx, out := fixtureCLI(t)
	st := ctx.Store
	t.Setenv("QUASIDOCK_HOME", st.Root)
	put := func(data, mediaType string) oci.Descriptor {
		t.Helper()
		blob, err := st.PutBlob([]byte(data), mediaType)
		if err != nil {
			t.Fatal(err)
		}
		return blob
	}
	makeImage := func(name string, layer oci.Descriptor, catalog bool) (store.Ref, uint64) {
		t.Helper()
		cfg := oci.AppConfig{ID: "system.fixture", Version: name, Entrypoint: []string{"app.exe"}}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		config := put(string(data), oci.MediaTypeConfig)
		data, err = json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: config, Layers: []oci.Descriptor{layer}})
		if err != nil {
			t.Fatal(err)
		}
		manifest := put(string(data), oci.MediaTypeManifest)
		ref := store.Ref{ID: cfg.ID, Version: name, Manifest: manifest.Digest, Source: "example.test/system:" + name}
		if catalog {
			err = st.RegisterImage(ref, ref.Source, false)
		} else {
			err = st.MarkBuildImage(ref.Manifest)
		}
		if err != nil {
			t.Fatal(err)
		}
		return ref, uint64(config.Size + manifest.Size)
	}
	write := func(path, data string) uint64 {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return uint64(len(data))
	}
	hold := func(path string) {
		t.Helper()
		lock, err := store.OpenLock(path, true, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(lock.Release)
	}

	shared := put("shared layer data", oci.MediaTypeLayerTar)
	borrowed := put("catalog and build dependency", oci.MediaTypeLayerTar)
	buildOnly := put("build-only layer", oci.MediaTypeLayerTar)
	layerLeased := put("live deployment", oci.MediaTypeLayerTar)
	lazy := oci.Descriptor{Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("remote-only"))), Size: 100000, MediaType: oci.MediaTypeLayerTarGzip}
	kept, keptMeta := makeImage("kept", shared, true)
	sharedImage, sharedMeta := makeImage("shared", shared, true)
	_, lazyMeta := makeImage("lazy", lazy, true)
	_, borrowedMeta := makeImage("borrowed", borrowed, true)
	_, layerLeaseMeta := makeImage("layer-lease", layerLeased, true)
	imageLeased, imageLeaseMeta := makeImage("image-lease", shared, true)
	_, borrowedBuildMeta := makeImage("borrowed-build", borrowed, false)
	build, buildMeta := makeImage("build-only", buildOnly, false)
	if err := st.TagImage("", kept.Manifest, "example.test/system:alias"); err != nil {
		t.Fatal(err)
	}
	hold(st.Path("images", "use-"+oci.DigestHex(imageLeased.Manifest)+".lock"))
	deploy, err := st.DeployPath(layerLeased.Digest)
	if err != nil {
		t.Fatal(err)
	}
	hold(store.LockPathFor(deploy))
	leasedDeploy := write(filepath.Join(deploy, "file"), "leased deployment bytes")
	sharedDeploy := write(st.Path("deploy", oci.DigestHex(shared.Digest), "file"), "unpacked shared bytes")
	lazyBytes := write(st.Path("lazy", oci.DigestHex(lazy.Digest), "tail"), "local streaming state")
	buildDeploy := write(st.Path("deploy", oci.DigestHex(buildOnly.Digest), "file"), "unpacked build layer")
	buildLazy := write(st.Path("lazy", oci.DigestHex(buildOnly.Digest), "tail"), "build streaming state")
	orphan := put("orphaned bytes", oci.MediaTypeLayerTar)
	write(st.Path("deploy", oci.DigestHex(orphan.Digest), "file"), "unused deployment")
	write(st.Path("tmp", "leftover"), "unused temporary file")
	write(st.Path("data", "legacy", "file"), "legacy application files")
	cacheBytes := write(st.Path("cache", "downloads", "download"), "downloaded content")
	cacheBytes += write(st.Path("cache", "tools", "tool", "file"), "tool files")
	cacheBytes += write(st.Path("cache", "work", "checkout", "file"), "project files")
	if err := st.WriteBuildCache(strings.Repeat("b", 64), build.Manifest); err != nil {
		t.Fatal(err)
	}
	cacheBytes += uint64(len(build.Manifest))

	bind := t.TempDir()
	write(filepath.Join(bind, "outside"), "external data omitted from the report")
	create := func(name string, rec store.ContainerRecord) *store.ContainerRecord {
		t.Helper()
		c, err := st.CreateContainer(name, rec)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	paused := create("paused", store.ContainerRecord{Image: kept.Manifest, AppID: kept.ID,
		State: store.ContainerState{Status: "paused", Running: true, Paused: true, Launched: true}})
	hold(st.ContainerRunLockPath(paused.ID))
	restarting := create("restarting", store.ContainerRecord{Image: kept.Manifest, AppID: kept.ID,
		State: store.ContainerState{Status: "restarting", Restarting: true, Launched: true}})
	hold(st.ContainerRunLockPath(restarting.ID))
	running := create("running", store.ContainerRecord{Image: kept.Manifest, AppID: kept.ID,
		State: store.ContainerState{Status: "running", Running: true, Launched: true}})
	hold(st.ContainerRunLockPath(running.ID))
	created := create("created", store.ContainerRecord{Image: kept.Manifest, AppID: kept.ID})
	hold(st.ContainerOperationLockPath(created.ID))
	stale := create("stale", store.ContainerRecord{
		Image: kept.Manifest, AppID: kept.ID, State: store.ContainerState{Status: "running", Running: true, Launched: true},
		HostConfig: store.ContainerHostConfig{AutoRemove: true, Mounts: []mounts.Spec{
			{Type: "bind", Source: bind, Target: "${data}/bind"},
			{Type: "volume", Source: "used", Target: "${data}/used"},
			{Type: "volume", Source: "used", Target: "${data}/used-again"},
		}},
	})
	for _, mount := range stale.Mounts[:2] {
		if err := mounts.Attach(stale.DataDir, mount); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = mounts.Detach(stale.DataDir, mount) })
	}
	masked := stale.Mounts[2].Destination
	maskedBytes := write(filepath.Join(masked, "detached"), "reserved mount target omitted even if it is a directory")
	private := write(stale.LogPath, "captured output") + write(filepath.Join(stale.DataDir, "private"), "private bytes")
	write(filepath.Join(stale.Mounts[1].Source, "volume"), "persistent volume files")
	idle, err := st.CreateVolume("idle")
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(idle.Mountpoint, "volume"), "idle volume files")
	guarded, err := st.CreateVolume("guarded")
	if err != nil {
		t.Fatal(err)
	}
	session, err := grants.BeginMounts(st, running, oci.SandboxSoft, []grants.MountRequest{{Path: guarded.Mountpoint}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if err := st.TrackNetwork("887dc070-532a-4937-9a26-136928291107"); err != nil {
		t.Fatal(err)
	}

	paths := []string{st.Path("images", "index.json"), st.Path("events.json"), stale.LogPath}
	for _, c := range []*store.ContainerRecord{paused, restarting, running, created, stale} {
		paths = append(paths, st.ContainerStatePath(c.ID))
	}
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := make(map[string][]byte)
	for _, path := range paths {
		before[path] = read(path)
	}

	report, err := loadSystemDF(st)
	if err != nil {
		t.Fatal(err)
	}
	sharedBytes := uint64(shared.Size) + sharedDeploy
	wantImages := store.DiskUsageTotals{
		Total: 6, Active: 2,
		Size: keptMeta + sharedMeta + lazyMeta + borrowedMeta + layerLeaseMeta + imageLeaseMeta +
			sharedBytes + lazyBytes + uint64(borrowed.Size+layerLeased.Size) + leasedDeploy,
		Reclaimable: sharedMeta + lazyMeta + borrowedMeta + layerLeaseMeta + lazyBytes,
	}
	if report.ImageTotals != wantImages {
		t.Fatalf("image totals %+v, want %+v", report.ImageTotals, wantImages)
	}
	for _, row := range report.Images {
		if row.ID == kept.Manifest && (row.SharedSize != sharedBytes || row.UniqueSize != keptMeta || row.Containers != 5 ||
			!reflect.DeepEqual(row.Names, []string{"example.test/system:alias", kept.Source})) {
			t.Fatalf("pinned image detail: %+v", row)
		}
		if row.ID == sharedImage.Manifest && (row.SharedSize != sharedBytes || row.UniqueSize != sharedMeta) {
			t.Fatalf("shared image detail: %+v", row)
		}
	}
	wantCache := cacheBytes + borrowedBuildMeta + buildMeta + uint64(buildOnly.Size) + buildDeploy
	if report.CacheTotals != (store.DiskUsageTotals{Total: 6, Size: wantCache + buildLazy, Reclaimable: wantCache}) {
		t.Fatalf("cache totals %+v, want 6 entries, %d bytes and %d reclaimable", report.CacheTotals, wantCache+buildLazy, wantCache)
	}
	staleSize := private + uint64(len(before[st.ContainerStatePath(stale.ID)]))
	var containerSize = staleSize
	for _, c := range []*store.ContainerRecord{paused, restarting, running, created} {
		containerSize += uint64(len(before[st.ContainerStatePath(c.ID)]))
	}
	if report.ContainerTotals != (store.DiskUsageTotals{Total: 5, Active: 3, Size: containerSize, Reclaimable: staleSize}) {
		t.Fatalf("container totals %+v, want %d bytes and %d reclaimable", report.ContainerTotals, containerSize, staleSize)
	}
	wantVolumeSize := mounts.TreeSize(st.Path("volumes"))
	wantVolumeReclaimable := mounts.TreeSize(filepath.Dir(idle.Mountpoint))
	if report.VolumeTotals != (store.DiskUsageTotals{Total: 3, Active: 2, Size: wantVolumeSize, Reclaimable: wantVolumeReclaimable}) {
		t.Fatalf("volume totals %+v, want %d bytes and %d reclaimable", report.VolumeTotals, wantVolumeSize, wantVolumeReclaimable)
	}
	if report.Volumes[2].Name != "used" || report.Volumes[2].References != 1 {
		t.Fatalf("volume references: %+v", report.Volumes)
	}

	if code, err := Dispatch(ctx, []string{"system", "info", "-f", `{{.Images}} {{.Volumes}} {{.TrackedNetworks}} {{.Containers}} {{.ContainersCreated}} {{.ContainersRunning}} {{.ContainersPaused}} {{.ContainersRestarting}} {{.ContainersExited}}`}); code != 0 || err != nil || out.String() != "6 3 1 5 1 1 1 1 1\n" {
		t.Fatalf("populated info: %d, %v, %q", code, err, out)
	}
	out.Reset()
	if code, err := Dispatch(ctx, []string{"system", "df", "-v"}); code != 0 || err != nil || !strings.Contains(out.String(), "exited") {
		t.Fatalf("verbose df: %d, %v, %q", code, err, out)
	}
	first := out.String()
	out.Reset()
	if code, err := Dispatch(ctx, []string{"system", "df", "--verbose"}); code != 0 || err != nil || out.String() != first {
		t.Fatalf("unstable details: %d, %v\n%q\n%q", code, err, first, out)
	}
	for path, data := range before {
		if !bytes.Equal(data, read(path)) {
			t.Errorf("report changed %s", path)
		}
	}
	if st.HasBlob(lazy.Digest) {
		t.Error("df fetched the missing layer blob")
	}
	pruned, pruneBytes, _, err := st.PruneContainers(true)
	if err != nil || len(pruned) != 1 || pruned[0].ID != stale.ID || pruneBytes != report.ContainerTotals.Reclaimable+maskedBytes {
		t.Fatalf("container prune preview: %v, %d bytes, %v", pruned, pruneBytes, err)
	}
	for _, check := range []struct {
		args []string
		want string
	}{
		{[]string{"image", "prune", "--all", "--dry-run"}, "Would reclaim: " + lifecycle.FormatBytes(wantImages.Reclaimable)},
		{[]string{"volume", "prune", "--dry-run"}, "Would reclaim " + lifecycle.FormatBytes(wantVolumeReclaimable) + " in 1 volume(s)"},
	} {
		out.Reset()
		if code, err := Dispatch(ctx, check.args); code != 0 || err != nil || !strings.Contains(out.String(), check.want) {
			t.Fatalf("%v returned %d, %v, %q, want %q", check.args, code, err, out, check.want)
		}
	}
}
