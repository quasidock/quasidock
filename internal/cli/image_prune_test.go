package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/lazy"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
)

func TestParseImagePruneOptions(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 30, 0, 0, time.FixedZone("Bangkok", 7*3600))
	for _, args := range [][]string{
		{"-af", "--dry-run", "--filter", "until=1h30m", "--filter=label=team=a=b", "--filter", "label!=keep", "--filter=label=empty=", "--"},
		{"--all=true", "--force=false", "--dry-run=true", "--filter=until=1h30m", "--filter=label=team=a=b", "--filter=label!=keep", "--filter=label=empty="},
	} {
		opts, err := parseImagePruneOptions(args, now)
		if err != nil || !opts.selection.All || !opts.dry || len(opts.selection.Until) != 1 || !opts.selection.Until[0].Equal(now.Add(-90*time.Minute)) {
			t.Fatalf("%v: %+v %v", args, opts, err)
		}
		if !reflect.DeepEqual(opts.selection.Labels, []store.ImageLabelFilter{{Key: "team", Value: "a=b", HasValue: true}, {Key: "empty", HasValue: true}}) ||
			!reflect.DeepEqual(opts.selection.NotLabels, []store.ImageLabelFilter{{Key: "keep"}}) {
			t.Fatalf("labels: %+v", opts.selection)
		}
	}
	for _, raw := range []string{"0", "123.123456789", "2026-11-26", "2026-11-26T12:30", "2026-11-26T12:30:00+07:00"} {
		if _, err := parseImagePruneOptions([]string{"--filter", "until=" + raw}, now); err != nil {
			t.Errorf("valid time %q: %v", raw, err)
		}
	}
	if opts, err := parseImagePruneOptions([]string{"-a", "--all=false", "--dry-run=false", "-f=false"}, now); err != nil || opts.selection.All || opts.dry {
		t.Fatalf("false flags: %+v %v", opts, err)
	}
	for _, args := range [][]string{
		{"app"}, {"-"}, {"--", "app"}, {"--downloads"}, {"--data"}, {"-q"},
		{"--all=maybe"}, {"-f=maybe"}, {"--dry-run="}, {"--force=maybe"},
		{"--filter"}, {"--filter="}, {"--filter", "until="}, {"--filter=until=invalid"},
		{"--filter=until=1.1234567890"}, {"--filter=until=1."}, {"--filter=until=2026-02-30"},
		{"--filter=label"}, {"--filter=label="}, {"--filter=label==value"}, {"--filter=label!="},
		{"--filter=unknown=value"}, {"--filter=label=   "},
	} {
		if _, err := parseImagePruneOptions(args, now); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

// Snapshots intentionally exclude lock files: reads may create coordination
// locks, but must not change catalog, container, journal, or image data.
func imagePruneSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(path, ".lock") || path == filepath.Join(root, "lock") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err == nil {
			rel, _ := filepath.Rel(root, path)
			files[rel] = string(data)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestImagePruneCLI(t *testing.T) {
	ctx, out := fixtureCLI(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	name := func(v string) string { return reg.Host() + "/app:" + v }
	a, shared := fixtureImage(t, reg, "1", []byte("shared"))
	b, _ := fixtureImage(t, reg, "2", []byte("shared"))
	c, unique := fixtureImage(t, reg, "3", []byte("unique payload"))
	d, buildLayer := fixtureImage(t, reg, "4", []byte("cached build dependency"))
	for _, v := range []string{"1", "2", "3", "4"} {
		if _, err := imageCommand(ctx, []string{"pull", name(v)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"1", "3"} {
		if err := ctx.Store.TagImage("", b, name(v)); err != nil {
			t.Fatal(err)
		}
	}
	if err := ctx.Store.MarkBuildImage(d); err != nil {
		t.Fatal(err)
	}
	pinned, err := ctx.Store.CreateContainer("keep", store.ContainerRecord{Image: b, AppID: "test.cli.fixture", State: store.ContainerState{Status: "exited"}})
	if err != nil {
		t.Fatal(err)
	}
	// Ordinary container lookups would reconcile and delete this crashed --rm
	// record. Both prune modes must retain the raw record and its image instead.
	pinned.State = store.ContainerState{Status: "running", Running: true, Launched: true}
	pinned.HostConfig.AutoRemove = true
	state, err := json.Marshal(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctx.Store.ContainerStatePath(pinned.ID), state, 0o644); err != nil {
		t.Fatal(err)
	}
	// Seed unrelated files in every broad-prune area, plus an already orphaned blob.
	orphan, err := ctx.Store.PutBlob([]byte("earlier orphan"), oci.MediaTypeSource)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"data/unused", "cache/downloads", "cache/tools/tool", "cache/work/project", "tmp/kept", "volumes/unused/data"} {
		path := ctx.Store.Path(dir, "saved")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("preserved"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	requests := reg.Count("")
	reg.Fail(true)
	before := imagePruneSnapshot(t, ctx.Store.Root)
	out.Reset()
	if code, err := imageCommand(ctx, []string{"prune", "--dry-run"}); err != nil || code != 0 {
		t.Fatalf("dry run: %d %v", code, err)
	}
	preview := out.String()
	if !strings.Contains(preview, a) || !strings.Contains(preview, c) || strings.Contains(preview, b) || strings.Contains(preview, d) || !strings.Contains(preview, "Would reclaim:") {
		t.Fatalf("preview selected wrong images: %s", preview)
	}
	if after := imagePruneSnapshot(t, ctx.Store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("dry run changed store data")
	}
	var expected uint64
	for _, digest := range []string{a, c} {
		img, err := ctx.Store.ReadImage(digest, digest)
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range []string{digest, img.Manifest.Config.Digest} {
			info, err := os.Stat(ctx.Store.BlobPath(oci.DigestHex(object)))
			if err != nil {
				t.Fatal(err)
			}
			expected += uint64(info.Size())
		}
	}
	info, _ := os.Stat(ctx.Store.BlobPath(oci.DigestHex(unique.Digest)))
	expected += uint64(info.Size())
	uniqueDeploy, _ := ctx.Store.DeployPath(unique.Digest)
	if err := filepath.WalkDir(uniqueDeploy, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			expected += uint64(info.Size())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code, err := imageCommand(ctx, []string{"prune", "-f"}); err != nil || code != 0 {
		t.Fatalf("prune: %d %v", code, err)
	}
	if !strings.Contains(preview, "Would reclaim: "+lifecycle.FormatBytes(expected)) || !strings.Contains(out.String(), "Total reclaimed space: "+lifecycle.FormatBytes(expected)) {
		t.Fatalf("incorrect accounting: expected %d; preview %s; actual %s", expected, preview, out.String())
	}
	ids := []string{a, c}
	sort.Strings(ids)
	if strings.Index(out.String(), ids[0]) > strings.Index(out.String(), ids[1]) {
		t.Fatal("image output is not deterministic")
	}
	for _, digest := range []string{a, c} {
		if _, err := ctx.Store.ResolveLocalImage("", digest); !errors.Is(err, store.ErrImageNotFound) || ctx.Store.HasBlob(digest) {
			t.Fatalf("pruned image remains: %s %v", digest, err)
		}
	}
	sharedDeploy, _ := ctx.Store.DeployPath(shared.Digest)
	if !ctx.Store.HasBlob(shared.Digest) || !store.Exists(sharedDeploy) || store.Exists(uniqueDeploy) || ctx.Store.HasBlob(unique.Digest) {
		t.Fatal("shared layer was lost or unique layer survived")
	}
	out.Reset()
	if code, err := imageCommand(ctx, []string{"prune", "--all"}); err != nil || code != 0 || !strings.Contains(out.String(), d) || !strings.Contains(out.String(), "Total reclaimed space: 0 B") {
		t.Fatalf("build-cache prune: %d %v %s", code, err, out.String())
	}
	idx, _ := ctx.Store.ImageCatalog()
	if len(idx.Images) != 1 || idx.Images[b].Manifest != b || len(idx.Defaults) != 0 {
		t.Fatalf("wrong retained images/defaults: %+v", idx)
	}
	buildDeploy, _ := ctx.Store.DeployPath(buildLayer.Digest)
	if !ctx.Store.HasBlob(d) || !ctx.Store.HasBlob(buildLayer.Digest) || !store.Exists(buildDeploy) || !ctx.Store.HasBlob(orphan.Digest) {
		t.Fatal("build cache or unrelated orphan was removed")
	}
	after := imagePruneSnapshot(t, ctx.Store.Root)
	for path, data := range before {
		if strings.HasPrefix(path, "containers"+string(filepath.Separator)) || strings.HasPrefix(path, "cache"+string(filepath.Separator)) ||
			strings.HasPrefix(path, "data"+string(filepath.Separator)) || strings.HasPrefix(path, "volumes"+string(filepath.Separator)) || strings.HasPrefix(path, "tmp"+string(filepath.Separator)) || path == "events.json" {
			if after[path] != data {
				t.Fatalf("unrelated file changed: %s (container %s)", path, pinned.ID)
			}
		}
	}
	out.Reset()
	if code, err := imageCommand(ctx, []string{"inspect", "-f", "{{.CachedAt}}", b}); err != nil || code != 0 {
		t.Fatal("inspect CachedAt:", code, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(out.String())); err != nil {
		t.Fatal("invalid inspect CachedAt:", out.String(), err)
	}
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	for _, args := range [][]string{{"image", "prune", "--help"}, {"image", "prune", "--all", "--dry-run"}} {
		out.Reset()
		if code, err := Dispatch(ctx, args); err != nil || code != 0 {
			t.Fatalf("dispatch %v: %d %v", args, code, err)
		}
	}
	if reg.Count("") != requests {
		t.Fatal("image prune contacted the registry")
	}
}

func TestImagePruneMetadataErrorsAreAtomic(t *testing.T) {
	for _, kind := range []string{"container", "candidate config", "retained manifest", "build config", "invalid layer", "negative size"} {
		t.Run(kind, func(t *testing.T) {
			ctx, out := fixtureCLI(t)
			img := historyFixture()
			if kind == "invalid layer" {
				img.Manifest.Layers[len(img.Manifest.Layers)-1].Digest = "invalid"
			}
			if kind == "negative size" {
				img.Manifest.Layers[len(img.Manifest.Layers)-1].Size = -1
			}
			candidate := registerHistoryImage(t, ctx, img, "example.test/app:1")
			args := []string{"prune", "--all"}
			switch kind {
			case "container":
				c, err := ctx.Store.CreateContainer("broken", store.ContainerRecord{Image: candidate, AppID: "history-fixture"})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(ctx.Store.ContainerStatePath(c.ID), []byte("{"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "candidate config", "build config":
				m, _ := ctx.Store.ReadManifest(candidate)
				if err := os.Remove(ctx.Store.BlobPath(oci.DigestHex(m.Config.Digest))); err != nil {
					t.Fatal(err)
				}
				if kind == "build config" {
					if err := ctx.Store.MarkBuildImage(candidate); err != nil {
						t.Fatal(err)
					}
				}
			case "retained manifest":
				args = []string{"prune"}
				if err := os.Remove(ctx.Store.BlobPath(oci.DigestHex(candidate))); err != nil {
					t.Fatal(err)
				}
			}
			before := imagePruneSnapshot(t, ctx.Store.Root)
			for _, dry := range []bool{true, false} {
				opts := append([]string(nil), args...)
				if dry {
					opts = append(opts, "--dry-run")
				}
				if code, err := imageCommand(ctx, opts); err == nil || code != 1 {
					t.Fatalf("accepted unreadable %s: %d %v", kind, code, err)
				}
				if !reflect.DeepEqual(before, imagePruneSnapshot(t, ctx.Store.Root)) || out.Len() != 0 {
					t.Fatal("failed preflight changed data or reported removals")
				}
			}
		})
	}
}

func TestImagePruneLazyAndCleanupFailure(t *testing.T) {
	ctx, out := fixtureCLI(t)
	img := historyFixture()
	digest := registerHistoryImage(t, ctx, img, "example.test/app:1")
	first := img.Manifest.Layers[0].Digest
	lazyDir := lazy.DirPath(ctx.Store, oci.DigestHex(first))
	deploy, _ := ctx.Store.DeployPath(first)
	unrelated := lazy.DirPath(ctx.Store, strings.Repeat("e", 64))
	for _, dir := range []string{lazyDir, deploy, unrelated} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "partial"), []byte("partial bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := imagePruneSnapshot(t, ctx.Store.Root)
	if code, err := imageCommand(ctx, []string{"prune", "--all", "--dry-run"}); err != nil || code != 0 || !reflect.DeepEqual(before, imagePruneSnapshot(t, ctx.Store.Root)) {
		t.Fatal("incomplete lazy dry run:", code, err)
	}
	// A blob path that cannot be removed as a blob must produce a partial failure,
	// while successfully removing catalog metadata and independent lazy content.
	bad := ctx.Store.BlobPath(oci.DigestHex(img.Manifest.Layers[1].Digest))
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code, err := imageCommand(ctx, []string{"prune", "--all"}); err == nil || code != 1 || !strings.Contains(err.Error(), "regular blob") || !strings.Contains(out.String(), digest) {
		t.Fatalf("cleanup failure: %d %v %s", code, err, out.String())
	}
	if store.Exists(lazyDir) || store.Exists(deploy) || !store.Exists(unrelated) || !store.Exists(bad) {
		t.Fatal("scoped lazy cleanup deleted wrong content")
	}
	if _, err := ctx.Store.ResolveLocalImage("", digest); !errors.Is(err, store.ErrImageNotFound) {
		t.Fatal("cleanup failure rolled back successful catalog removal", err)
	}
	if bytes.Contains(out.Bytes(), []byte("Would")) {
		t.Fatal("actual cleanup reported a dry run")
	}
}

func TestImagePruneFilterSelection(t *testing.T) {
	ctx, out := fixtureCLI(t)
	cutoff := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour).Format(time.RFC3339Nano)
	var ids []string
	for i, labels := range []map[string]string{
		{"temporary": "yes", "empty": ""}, {"temporary": "yes", "keep": "yes"},
		{"temporary": "yes"}, {"temporary": "yes"}, nil,
	} {
		img := historyFixture()
		img.Config.Version = string(rune('a' + i))
		img.Config.FormatVersion = 4
		img.Config.Labels = labels
		// Creation metadata is deliberately unrelated to local cache age.
		img.Manifest.Annotations = map[string]string{"org.opencontainers.image.created": "1900-01-01T00:00:00Z"}
		ids = append(ids, registerHistoryImage(t, ctx, img, "example.test/app:"+img.Config.Version))
	}
	idx, err := ctx.Store.ImageCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for i, digest := range ids {
		ref := idx.Images[digest]
		ref.CachedAt = old
		if i == 2 {
			ref.CachedAt = cutoff.Format(time.RFC3339Nano)
		}
		if i == 3 {
			ref.CachedAt = ""
		}
		idx.Images[digest] = ref
	}
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctx.Store.Path("images", "index.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"prune", "--all", "--filter=until=" + cutoff.Format(time.RFC3339Nano), "--filter=label=temporary=yes", "--filter=label!=keep"}
	out.Reset()
	before := imagePruneSnapshot(t, ctx.Store.Root)
	if code, err := imageCommand(ctx, append(append([]string(nil), args...), "--dry-run")); err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if !strings.Contains(out.String(), ids[0]) {
		t.Fatal("matching image was not selected:", out.String())
	}
	for _, digest := range ids[1:] {
		if strings.Contains(out.String(), digest) {
			t.Fatalf("nonmatching image selected: %s %s", digest, out.String())
		}
	}
	if !reflect.DeepEqual(before, imagePruneSnapshot(t, ctx.Store.Root)) {
		t.Fatal("filtered preview changed store")
	}
	out.Reset()
	if code, err := imageCommand(ctx, args); err != nil || code != 0 {
		t.Fatal(code, err)
	}
	for i, digest := range ids {
		_, err := ctx.Store.ResolveLocalImage("", digest)
		if i == 0 && !errors.Is(err, store.ErrImageNotFound) || i != 0 && err != nil {
			t.Fatalf("filtered removal %s: %v", digest, err)
		}
	}
	// Invalid options never execute an otherwise valid prune selection.
	before = imagePruneSnapshot(t, ctx.Store.Root)
	for _, args := range [][]string{{"prune", "--all", "--filter=bad=value"}, {"prune", "--all", "--force=invalid"}, {"prune", "app"}} {
		out.Reset()
		if code, err := imageCommand(ctx, args); err == nil || code != 1 || out.Len() != 0 || !reflect.DeepEqual(before, imagePruneSnapshot(t, ctx.Store.Root)) {
			t.Fatalf("invalid invocation changed data: %v %d %v", args, code, err)
		}
	}
}
