package cli

import (
	"archive/tar"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/signature"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/trust"
)

func fixtureImage(t *testing.T, reg *testregistry.Registry, version string, payload []byte, customize ...func(*oci.AppConfig)) (string, oci.Descriptor) {
	t.Helper()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	files := map[string][]byte{"fixture.exe": payload, "args.cmd": []byte("@echo off\r\n\"%~dp0fixture.exe\" %*\r\n")}
	for _, name := range []string{"fixture.exe", "args.cmd"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer := oci.Descriptor{MediaType: oci.MediaTypeLayerTar, Digest: reg.PutBlob(archive.Bytes()), Size: int64(archive.Len())}
	cfg := oci.AppConfig{ID: "test.cli.fixture", Version: version, Entrypoint: []string{"fixture.exe"}, Cmd: []string{"default", version}, Path: []string{"."}, Env: map[string]string{"REMOVE_ME": "image"}, Permissions: oci.Permissions{Sandbox: oci.SandboxSoft}}
	for _, f := range customize {
		f(&cfg)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mediaType := oci.MediaTypeConfig
	if cfg.FormatVersion == 4 {
		mediaType = oci.MediaTypeConfigV4
	}
	config := oci.Descriptor{MediaType: mediaType, Digest: reg.PutBlob(b), Size: int64(len(b))}
	b, err = json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: config, Layers: []oci.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	d := reg.PutManifest("app", version, b)
	reg.PutManifest("app", "latest", b)
	return d, layer
}

func fixtureCLI(t *testing.T) (*context.Context, *bytes.Buffer) {
	t.Helper()
	ctx := context.New()
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx.Store = st
	t.Cleanup(func() {
		entries, err := os.ReadDir(st.Path("deploy"))
		if err != nil {
			t.Error(err)
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() || !oci.IsSha256Hex(entry.Name()) {
				continue
			}
			deleted, err := st.DeleteDeployment("sha256:" + entry.Name())
			if err != nil || deleted == store.DeletionInUse {
				t.Errorf("cleaning test deployment %s: %v (in use: %t)", entry.Name(), err, deleted == store.DeletionInUse)
			}
		}
	})
	var out bytes.Buffer
	ctx.Stdout = &out
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	return ctx, &out
}

func TestImageList(t *testing.T) {
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	layer := pruneFixtureBlob(t, ctx.Store, "image list fixture layer")
	image := func(id string) store.Ref {
		t.Helper()
		digest, _ := pruneImageFixture(t, ctx.Store, id, "", false, layer)
		return store.Ref{ID: id, Version: "1", Manifest: digest}
	}
	register := func(ref store.Ref, name string, makeDefault bool) {
		t.Helper()
		if err := ctx.Store.RegisterImage(ref, name, makeDefault); err != nil {
			t.Fatal(err)
		}
	}
	retired, named, pinned, defaultImage := image("retired"), image("named"), image("pinned"), image("default")
	register(retired, "example.test/app:latest", false)
	register(named, "example.test/app:latest", false)
	if err := ctx.Store.TagImage("", named.Manifest, "example.test/app:alias"); err != nil {
		t.Fatal(err)
	}
	register(pinned, "example.test/pinned@"+pinned.Manifest, false)
	register(defaultImage, "", true)
	if err := ctx.Store.MarkBuildImage(image("build-only").Manifest); err != nil {
		t.Fatal(err)
	}

	call := func(t *testing.T, args ...string) string {
		t.Helper()
		out.Reset()
		if code, err := Dispatch(ctx, args); code != 0 || err != nil {
			t.Fatalf("%v: exit %d, %v", args, code, err)
		}
		return out.String()
	}
	type row struct {
		repository, tag, digest string
	}
	visibleRows := []row{
		{"<none>", "<none>", defaultImage.Manifest},
		{"example.test/app", "alias", named.Manifest},
		{"example.test/app", "latest", named.Manifest},
		{"example.test/pinned", "<none>", pinned.Manifest},
	}
	unnamed := []string{defaultImage.Manifest, retired.Manifest}
	slices.Sort(unnamed)
	allRows := []row{{"<none>", "<none>", unnamed[0]}, {"<none>", "<none>", unnamed[1]}}
	allRows = append(allRows, visibleRows[1:]...)
	for _, tt := range []struct {
		name            string
		args            []string
		rows            []row
		digests, fullID bool
	}{
		{"default", []string{"images"}, visibleRows, false, false},
		{"all", []string{"image", "ls", "-a"}, allRows, false, false},
		{"digests", []string{"image", "list", "--digests"}, visibleRows, true, false},
		{"full IDs", []string{"images", "--no-trunc"}, visibleRows, false, true},
		{"all display flags", []string{"images", "--all", "--digests", "--no-trunc"}, allRows, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			header := "REPOSITORY TAG IMAGE ID SIZE"
			if tt.digests {
				header = "REPOSITORY TAG DIGEST IMAGE ID SIZE"
			}
			want := []string{header}
			for _, row := range tt.rows {
				fields := []string{row.repository, row.tag}
				if tt.digests {
					fields = append(fields, row.digest)
				}
				id := row.digest
				if !tt.fullID {
					id = strings.TrimPrefix(id, "sha256:")[:12]
				}
				manifest, err := ctx.Store.ReadManifest(row.digest)
				if err != nil {
					t.Fatal(err)
				}
				want = append(want, strings.Join(fields, " ")+fmt.Sprintf(" %s %d B", id, manifest.Config.Size+layer.Size))
			}
			got := strings.Split(strings.TrimSpace(call(t, tt.args...)), "\n")
			for i, line := range got {
				got[i] = strings.Join(strings.Fields(line), " ")
			}
			if !slices.Equal(got, want) {
				t.Fatalf("got rows %q, want %q", got, want)
			}
		})
	}

	visibleIDs := []string{defaultImage.Manifest, named.Manifest, pinned.Manifest}
	allIDs := append(slices.Clone(visibleIDs), retired.Manifest)
	slices.Sort(visibleIDs)
	slices.Sort(allIDs)
	if got := call(t, "images", "-q"); got != strings.Join(visibleIDs, "\n")+"\n" {
		t.Fatalf("quiet list: %q", got)
	}
	for _, args := range [][]string{
		{"images", "-aq"},
		{"image", "list", "-qa"},
		{"image", "ls", "--all", "--quiet", "--digests", "--no-trunc"},
	} {
		if got := call(t, args...); got != strings.Join(allIDs, "\n")+"\n" {
			t.Fatalf("%v: %q", args, got)
		}
	}
	ctx.Env["QUASIDOCK_REGISTRY"] = "example.test"
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"exact alias", []string{"image", "ls", "-f", "reference=app:alias", "--format", "{{.Tag}}"}, "alias\n"},
		{"all repository aliases", []string{"image", "list", "--filter=reference=example.test/app", "--format={{.Tag}}"}, "alias\nlatest\n"},
		{"digest pin", []string{"images", "-f=reference=pinned@" + pinned.Manifest, "--format", "{{.Tag}} {{.Digest}}"}, "<none> " + pinned.Manifest + "\n"},
		{"quiet alias union", []string{"images", "-q", "-f", "reference=app:ali*", "-f", "reference=app:latest", "--format", "{{"}, named.Manifest + "\n"},
		{"dangling includes retired", []string{"images", "-q", "--filter=dangling=true"}, retired.Manifest + "\n"},
		{"named or default", []string{"images", "-aq", "-f", "dangling=false"}, strings.Join(visibleIDs, "\n") + "\n"},
		{"both dangling values", []string{"images", "-q", "-f", "dangling=false", "-f", "dangling=true"}, strings.Join(allIDs, "\n") + "\n"},
		{"no historical alias", []string{"images", "-a", "-f", "dangling=true", "-f", "reference=app", "--format", "json"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := call(t, tt.args...); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
	defaultTable := call(t, "images", "--digests", "--no-trunc")
	if got := call(t, "image", "ls", "--format=table", "--digests", "--no-trunc"); got != defaultTable {
		t.Fatalf("explicit table differs from default: %q", got)
	}
	if got := call(t, "images", "--help"); !strings.Contains(got, "--filter") || !strings.Contains(got, "--format") {
		t.Fatalf("incomplete image listing help: %q", got)
	}
	for _, args := range [][]string{
		{"images", "--unknown"},
		{"image", "ls", "-ax"},
		{"image", "list", "example.test/app"},
	} {
		out.Reset()
		if _, err := Dispatch(ctx, args); err == nil || !strings.Contains(err.Error(), "unknown images option") || out.Len() != 0 {
			t.Fatalf("%v: output %q, error %v", args, out.String(), err)
		}
	}
	missing := historyDigest(42)
	if err := ctx.Store.RegisterImage(store.Ref{ID: "missing", Version: "1", Manifest: missing}, "example.test/missing:latest", false); err != nil {
		t.Fatal(err)
	}
	if got := call(t, "images", "-q", "-f", "reference=missing"); got != missing+"\n" {
		t.Fatalf("catalog-only filter needed image metadata: %q", got)
	}
	if got := call(t, "images", "-f", "reference=app:alias", "--format", "{{.Tag}}"); got != "alias\n" {
		t.Fatalf("nonmatching image affected filtered listing: %q", got)
	}
}

func TestImageListMetadataAndFormat(t *testing.T) {
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	ctx.Env["QUASIDOCK_REGISTRY"] = "example.test"
	fixtures := []struct {
		tag, createdAt, cachedAt string
		labels                   map[string]string
		digest                   string
	}{
		{tag: "old", createdAt: "2020-01-01T07:00:00+07:00", cachedAt: "2035-01-01T00:00:00Z", labels: map[string]string{"stage": "test", "empty": ""}},
		{tag: "middle", createdAt: "invalid", cachedAt: "2021-01-01T00:00:00Z", labels: map[string]string{"stage": "beta", "empty": "present"}},
		{tag: "new", createdAt: "2022-01-01T00:00:00Z", labels: map[string]string{"stage": "prod"}},
		{tag: "unknown"},
	}
	digestsByTag := make(map[string]string)
	for i := range fixtures {
		f := &fixtures[i]
		img := historyFixture()
		img.Config.Version = f.tag
		img.Config.FormatVersion = 4
		img.Config.Labels = f.labels
		img.Manifest.Annotations["org.opencontainers.image.created"] = f.createdAt
		f.digest = registerHistoryImage(t, ctx, img, "example.test/app:"+f.tag)
		digestsByTag[f.tag] = f.digest
	}
	idx, err := ctx.Store.ImageCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		ref := idx.Images[f.digest]
		ref.CachedAt = f.cachedAt
		idx.Images[f.digest] = ref
	}
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctx.Store.Path("images", "index.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	call := func(t *testing.T, args ...string) string {
		t.Helper()
		out.Reset()
		if code, err := Dispatch(ctx, append([]string{"images"}, args...)); code != 0 || err != nil {
			t.Fatalf("%v: exit %d, %v", args, code, err)
		}
		return out.String()
	}
	old, middle, newest := fixtures[0].digest, fixtures[1].digest, fixtures[2].digest
	for _, tt := range []struct {
		name    string
		filters []string
		want    string
	}{
		{"label present", []string{"label=empty"}, "middle\nold\n"},
		{"label empty", []string{"label=empty="}, "old\n"},
		{"filter union and intersection", []string{"label=stage=prod", "label=stage=test", "reference=app:o*"}, "old\n"},
		{"strict before", []string{"before=app:middle"}, "old\n"},
		{"strict since with short ID", []string{"since=" + strings.TrimPrefix(middle, "sha256:")[:12]}, "new\n"},
		{"window", []string{"since=" + old, "before=" + newest}, "middle\n"},
		{"repeated cutoff", []string{"before=app:old", "before=app:new"}, "middle\nold\n"},
		{"label and date", []string{"label=stage=beta", "since=" + old}, "middle\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var args []string
			for _, f := range tt.filters {
				args = append(args, "--filter", f)
			}
			if got := call(t, append(slices.Clone(args), "--format", "{{.Tag}}")...); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			var digests []string
			for _, tag := range strings.Fields(tt.want) {
				digests = append(digests, digestsByTag[tag]+"\n")
			}
			slices.Sort(digests)
			if got := call(t, append(args, "-q")...); got != strings.Join(digests, "") {
				t.Fatalf("unexpected quiet output %q", got)
			}
		})
	}
	wantDates := map[string]string{"old": "2020-01-01T00:00:00Z", "middle": "2021-01-01T00:00:00Z", "new": "2022-01-01T00:00:00Z", "unknown": "--"}
	jsonLines := call(t, "--format", "json")
	lines := strings.Split(strings.TrimSuffix(jsonLines, "\n"), "\n")
	if len(lines) != len(fixtures) {
		t.Fatalf("unexpected JSON Lines: %q", jsonLines)
	}
	if got := call(t, "--filter=reference=app:unknown", "--format", "{{json .}}"); got != lines[len(lines)-1]+"\n" {
		t.Fatalf("json helper differs from JSON Lines: %q", got)
	}
	for i, tag := range []string{"middle", "new", "old", "unknown"} {
		var row imageListRow
		if err := json.Unmarshal([]byte(lines[i]), &row); err != nil {
			t.Fatal(err)
		}
		digest := digestsByTag[tag]
		manifest, err := ctx.Store.ReadManifest(digest)
		if err != nil {
			t.Fatal(err)
		}
		if row.Repository != "example.test/app" || row.Tag != tag || row.Digest != digest ||
			row.ID != strings.TrimPrefix(digest, "sha256:")[:12] || row.Size != fmt.Sprintf("%d B", imageManifestSize(manifest)) ||
			row.CreatedAt != wantDates[tag] || (tag == "unknown" && row.CreatedSince != "--") ||
			(tag != "unknown" && !strings.HasSuffix(row.CreatedSince, " ago")) {
			t.Fatalf("unexpected JSON row for %s: %+v", tag, row)
		}
	}
	if got := call(t, "--filter=reference=app:old", "--no-trunc", "--format", "{{.ID}}"); got != old+"\n" {
		t.Fatalf("unexpected full template ID: %q", got)
	}
	for _, tt := range []struct{ filter, want string }{
		{"reference=app:old", "TAG CREATED AT\nold 2020-01-01T00:00:00Z"},
		{"reference=no-match", "TAG CREATED AT"},
	} {
		table := call(t, "--filter", tt.filter, "--format", "table {{.Tag}}\\t{{.CreatedAt}}")
		lines := strings.Split(strings.TrimSpace(table), "\n")
		for i, line := range lines {
			lines[i] = strings.Join(strings.Fields(line), " ")
		}
		if got := strings.Join(lines, "\n"); got != tt.want {
			t.Fatalf("unexpected custom table: %q", table)
		}
	}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--filter"}, "needs a nonempty value"},
		{[]string{"--filter=label"}, "expected KEY=VALUE"},
		{[]string{"-f=label==yes"}, "label key is empty"},
		{[]string{"-f=dangling=maybe"}, "expected true or false"},
		{[]string{"-f=reference=["}, "invalid images reference filter"},
		{[]string{"-f=unknown=value"}, "unsupported images filter"},
		{[]string{"-f=before=app:unknown"}, "image has no creation or local cache timestamp"},
		{[]string{"-f=since=app:missing"}, "image is not cached"},
		{[]string{"--format="}, "needs a nonempty value"},
		{[]string{"--format=table "}, "needs a template"},
		{[]string{"--format={{", "-f=reference=no-match"}, "template:"},
		{[]string{"--format={{if eq .Tag \"new\"}}{{.Missing}}{{end}}{{.Tag}}"}, "can't evaluate field Missing"},
	} {
		out.Reset()
		code, err := Dispatch(ctx, append([]string{"images"}, tt.args...))
		if code != 1 || err == nil || !strings.Contains(err.Error(), tt.want) || out.Len() != 0 {
			t.Errorf("%v: exit %d, error %v, output %q", tt.args, code, err, out.String())
		}
	}
}

func TestImageListAmbiguousShortID(t *testing.T) {
	first, second := historyDigest(1), historyDigest(2)
	idx := &store.ImageIndex{Images: map[string]store.Ref{first: {Manifest: first}, second: {Manifest: second}}}
	if _, err := resolveImageListTarget(idx, "", "000000000000"); err == nil || !strings.Contains(err.Error(), "ambiguous image ID") {
		t.Fatalf("unexpected ambiguous ID error: %v", err)
	}
}

func TestImageCommandsAndCachePolicies(t *testing.T) {
	ctx, out := fixtureCLI(t)
	reg := testregistry.New()
	defer reg.Close()
	a, _ := fixtureImage(t, reg, "1", []byte("fixture"))
	b, layer := fixtureImage(t, reg, "2", []byte("fixture"))
	name := func(tag string) string { return reg.Host() + "/app:" + tag }
	for _, tag := range []string{"1", "2"} {
		if _, err := imageCommand(ctx, []string{"pull", name(tag)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{a, b} {
		if _, err := ctx.Store.ReadImage("test", d); err != nil {
			t.Fatal("pull did not persist manifest/config:", err)
		}
	}
	before := reg.Count("/manifests/")
	// never must not contact the registry, including a missing explicit tag.
	if _, err := runImage(ctx, runOptionsForTest(t, []string{"--pull=never", name("missing")})); !errors.Is(err, store.ErrImageNotFound) {
		t.Fatalf("never missing: %v", err)
	}
	if reg.Count("/manifests/") != before {
		t.Fatal("never contacted registry")
	}
	if _, err := imageCommand(ctx, []string{"tag", a, name("copy")}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"tag", a, b}, {"push", a}, {"pull", a}} {
		if _, err := imageCommand(ctx, args); err == nil {
			t.Fatalf("used a local manifest ID as a remote name: %v", args)
		}
	}
	def, _ := ctx.Store.ReadRef("test.cli.fixture")
	if def.Manifest != b {
		t.Fatal("tag changed app default")
	}
	out.Reset()
	if _, err := imageCommand(ctx, []string{"inspect", "-f", "{{.Id}} {{.Quasidock.Version}}", name("copy")}); err != nil || strings.TrimSpace(out.String()) != a+" 1" {
		t.Fatalf("formatted inspect: %q %v", out.String(), err)
	}
	out.Reset()
	if _, err := imageCommand(ctx, []string{"inspect", name("1"), name("2")}); err != nil {
		t.Fatal(err)
	}
	var inspected []inspectedImage
	if err := json.Unmarshal(out.Bytes(), &inspected); err != nil || len(inspected) != 2 || inspected[0].Id != a || inspected[1].Id != b {
		t.Fatalf("inspect: %s %v", out.String(), err)
	}
	out.Reset()
	if _, err := imageCommand(ctx, []string{"ls", "-q"}); err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(out.String())) != 2 {
		t.Fatalf("quiet list duplicated aliases: %s", out.String())
	}
	// Publish an alias, then fetch it into a completely separate store.
	if _, err := imageCommand(ctx, []string{"push", name("copy")}); err != nil {
		t.Fatal(err)
	}
	other, _ := fixtureCLI(t)
	if _, err := pullImage(other, name("copy"), false); err != nil {
		t.Fatal(err)
	}
	got, err := other.Store.ResolveLocalImage("", name("copy"))
	if err != nil || got.Manifest != a {
		t.Fatalf("push selected wrong image: %v %v", got, err)
	}
	// An interrupted refresh never publishes the candidate's name/default.
	c, broken := fixtureImage(t, reg, "3", []byte("different layer"))
	_ = c
	reg.BreakBlob(broken.Digest)
	if _, err := pullImage(ctx, name("3"), false); err == nil {
		t.Fatal("broken layer pull succeeded")
	}
	if _, err := ctx.Store.ResolveLocalImage("", name("3")); !errors.Is(err, store.ErrImageNotFound) {
		t.Fatalf("failed pull published reference: %v", err)
	}
	def, _ = ctx.Store.ReadRef("test.cli.fixture")
	if def.Manifest != b {
		t.Fatal("failed pull changed default")
	}
	if !ctx.Store.HasBlob(layer.Digest) {
		t.Fatal("failed pull damaged old layer")
	}
	// Removing one version must preserve another version's shared deployment and
	// data, including after collection of the removed manifest/config blobs.
	dataDir := ctx.Store.Path("data", "test.cli.fixture")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "saved"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := imageCommand(ctx, []string{"rm", a}); err != nil {
		t.Fatal(err)
	}
	lock, err := ctx.Store.Lock(true)
	if err != nil {
		t.Fatal(err)
	}
	err = lifecycle.Prune(ctx, lifecycle.PruneOptions{Data: true})
	lock.Release()
	if err != nil {
		t.Fatal(err)
	}
	deploy, err := ctx.Store.DeployPath(layer.Digest)
	if err != nil || !store.Exists(deploy) {
		t.Fatal("removing one version lost shared deployment", err)
	}
	if !ctx.Store.HasBlob(layer.Digest) || !store.Exists(filepath.Join(dataDir, "saved")) {
		t.Fatal("prune lost another version's layer or app data")
	}
	reg.Fail(true)
	if _, err := runImage(ctx, runOptionsForTest(t, []string{"--pull=always", name("2")})); err == nil {
		t.Fatal("failed always pull ran cached image")
	}
}

func runOptionsForTest(t *testing.T, args []string) run.Options {
	t.Helper()
	opts, err := parseRunOptions(context.New(), args)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

func TestForegroundImages(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	buildDir := t.TempDir()
	fixture := filepath.Join(buildDir, "fixture.exe")
	cmd := exec.Command("go", "build", "-o", fixture, "../../tests/cmd/clifixture")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %s %v", out, err)
	}
	payload, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	reg := testregistry.New()
	defer reg.Close()
	a, _ := fixtureImage(t, reg, "1", payload)
	fixtureImage(t, reg, "2", payload)
	// Run actual CLI children so stdout, exit status and abrupt client death are
	// tested through the same entrypoint as the shipped program.
	binary := filepath.Join(buildDir, "quasidock.exe")
	cmd = exec.Command("go", "build", "-o", binary, "../../cmd/quasidock")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %s %v", out, err)
	}
	call := func(args ...string) (string, int) {
		t.Helper()
		c := exec.Command(binary, args...)
		c.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		var out, errout bytes.Buffer
		c.Stdout = &out
		c.Stderr = &errout
		err := c.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if code != 0 {
			return out.String() + errout.String(), code
		}
		return out.String(), code
	}
	name := reg.Host() + "/app:1"
	assertArgs := func(args []string, want []string) {
		t.Helper()
		out, code := call(args...)
		var got []string
		if code != 0 || json.Unmarshal([]byte(out), &got) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%v -> %s (exit %d), want %v", args, out, code, want)
		}
	}
	assertArgs([]string{"run", name}, []string{"default", "1"})
	requests := reg.Count("/manifests/")
	reg.Fail(true)
	assertArgs([]string{"run", name, "fixture.exe", "replacement", "--pull=always"}, []string{"replacement", "--pull=always"})
	assertArgs([]string{"run", "--pull=never", name, "fixture.exe", "offline"}, []string{"offline"})
	assertArgs([]string{"run", "--entrypoint=fixture.exe", a, "entrypoint"}, []string{"entrypoint"})
	assertArgs([]string{"run", "--entrypoint=args.cmd", name, "a b", "", `say "hello"`, `C:\a\`}, []string{"a b", "", `say "hello"`, `C:\a\`})
	if reg.Count("/manifests/") != requests {
		t.Fatal("cache hits contacted registry")
	}
	t.Setenv("CLI_INHERIT", "host")
	out, code := call("run", "-e", "CLI_INHERIT", "--env=REMOVE_ME", "-e", "EXPLICIT=one", "-eEXPLICIT=two", name, "fixture.exe", "env", "CLI_INHERIT", "REMOVE_ME", "EXPLICIT")
	if code != 0 || !strings.Contains(out, "CLI_INHERIT=host:true") || !strings.Contains(out, "REMOVE_ME=:false") || !strings.Contains(out, "EXPLICIT=two:true") {
		t.Fatalf("environment: %s %d", out, code)
	}
	if out, code := call("run", name, "fixture.exe", "exit", "37"); code != 37 {
		t.Fatalf("child exit: %s %d", out, code)
	}
	if out, code := call("run", "--name", "writer1", name, "fixture.exe", "write", "isolated"); code != 0 {
		t.Fatalf("write data: %s", out)
	}
	reg.Fail(false)
	if out, code := call("pull", reg.Host()+"/app:2"); code != 0 {
		t.Fatal(out)
	}
	// Containers isolate per-container data: a new run does not see writer1's file.
	if out, code := call("run", reg.Host()+"/app:2", "fixture.exe", "read"); code != 3 {
		t.Fatalf("expected isolated data miss (exit 3), got %q (%d)", out, code)
	}
	dataDirOut, code := call("inspect", "-f", "{{.DataDir}}", "writer1")
	if code != 0 {
		t.Fatalf("inspect writer1: %s %d", dataDirOut, code)
	}
	savedBytes, err := os.ReadFile(filepath.Join(strings.TrimSpace(dataDirOut), "home", "saved.txt"))
	if err != nil || string(savedBytes) != "isolated" {
		t.Fatalf("writer1 DataDir missing saved.txt: %q %v", savedBytes, err)
	}
	if out, code := call("run", "--ephemeral", name, "fixture.exe", "read"); code == 0 || !strings.Contains(out, "--rm") {
		t.Fatalf("expected --ephemeral rejection pointing to --rm, got %q (%d)", out, code)
	}
	if _, code := call("run", name, "missing.exe"); code == 0 {
		t.Fatal("missing executable succeeded")
	}
	// A failed launch must leave concurrent runs unblocked.
	assertArgs([]string{"run", reg.Host() + "/app:2"}, []string{"default", "2"})
	ready := filepath.Join(buildDir, "ready")
	hold := exec.Command(binary, "run", "--name", "held", name, "fixture.exe", "hold", ready)
	hold.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Process.Kill(); _ = hold.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for !store.Exists(ready) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !store.Exists(ready) {
		t.Fatal("held child did not become ready")
	}
	// Different versions of the same app can run concurrently in separate containers.
	assertArgs([]string{"run", reg.Host() + "/app:2"}, []string{"default", "2"})
	assertArgs([]string{"run", name}, []string{"default", "1"})
	if out, code := call("rmi", a); code == 0 || !strings.Contains(out, "in use") {
		t.Fatalf("running image removal: %s %d", out, code)
	}
	if err := hold.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = hold.Wait()
	// Losing the foreground viewer leaves its detached supervisor running.
	if rec, err := ctx.Store.ResolveContainer("held"); err != nil || !rec.State.Running {
		t.Fatalf("client exit killed container: %+v %v", rec, err)
	}
	if out, code := call("stop", "-t", "0", "held"); code != 0 {
		t.Fatalf("stop after client exit: %s %d", out, code)
	}
	// Exited containers continue pinning their image until removed or pruned.
	if out, code := call("rmi", a); code == 0 || !strings.Contains(out, "in use") {
		t.Fatalf("stopped container should pin image: %s %d", out, code)
	}
	if out, code := call("container", "prune"); code != 0 {
		t.Fatalf("container prune: %s %d", out, code)
	}
	if out, code := call("rmi", a); code != 0 {
		t.Fatalf("rmi after container prune: %s %d", out, code)
	}
}

func TestRunParserCoreFlagsAndRetiredCommands(t *testing.T) {
	for _, args := range [][]string{{"--pull=bad", "app"}, {"--command=x", "app"}, {"-v", "app"}, {"--ephemeral", "app"}, {"-p", "app"}, {"-e", "=bad", "app"}} {
		if _, err := parseRunOptions(context.New(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	opts := runOptionsForTest(t, []string{"--pull", "never", "-e", "NAME", "app", "--entrypoint=x"})
	if opts.Pull != "never" || opts.ID != "app" || !reflect.DeepEqual(opts.Args, []string{"--entrypoint=x"}) || !reflect.DeepEqual(opts.Settings.Env, []string{"NAME"}) {
		t.Fatalf("options: %+v", opts)
	}
	for old, replacement := range map[string]string{"list": "images", "info": "image info", "ls": "image files", "cat": "image cat", "-v": "--verbose"} {
		_, err := Dispatch(context.New(), []string{old})
		if err == nil || !strings.Contains(err.Error(), replacement) {
			t.Fatalf("retired %s: %v", old, err)
		}
	}
}

func TestPullSignatureGatePreservesReferences(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	reg := testregistry.New()
	defer reg.Close()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	public := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})
	trustBytes, err := json.Marshal(trust.File{Keys: []trust.Entry{{Prefix: reg.Host() + "/app", Key: string(public)}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctx.Store.Path("trust.json"), trustBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	name := reg.Host() + "/app:latest"
	a, _ := fixtureImage(t, reg, "1", []byte("first"))
	// Existing policy permits a missing signature, with a warning.
	if _, err := pullImage(ctx, name, false); err != nil {
		t.Fatal("missing signature should warn and install:", err)
	}
	sign := func(digest string, valid bool) {
		t.Helper()
		payload := []byte(signature.MakePayload(reg.Host()+"/app", digest))
		sum := sha256.Sum256(payload)
		sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		if !valid {
			sig[len(sig)-1] ^= 1
		}
		p := oci.Descriptor{MediaType: signature.PayloadMediaType, Digest: reg.PutBlob(payload), Size: int64(len(payload)), Annotations: map[string]string{signature.SignatureAnnotation: base64.StdEncoding.EncodeToString(sig)}}
		b, err := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Layers: []oci.Descriptor{p}})
		if err != nil {
			t.Fatal(err)
		}
		tag, err := signature.TagFor(digest)
		if err != nil {
			t.Fatal(err)
		}
		reg.PutManifest("app", tag, b)
	}
	b, layer := fixtureImage(t, reg, "2", []byte("second"))
	sign(b, false)
	before := reg.Count("/blobs/" + layer.Digest)
	if _, err := pullImage(ctx, name, false); err == nil || !strings.Contains(err.Error(), "Nothing was installed") {
		t.Fatalf("invalid signature: %v", err)
	}
	if _, err := createCommand(ctx, []string{"--pull=always", name}); err == nil || !strings.Contains(err.Error(), "Nothing was installed") {
		t.Fatalf("create accepted invalid signature: %v", err)
	}
	if containers, err := ctx.Store.ListContainers(true); err != nil || len(containers) != 0 {
		t.Fatalf("rejected signature created a container: %v %v", containers, err)
	}
	if reg.Count("/blobs/"+layer.Digest) != before {
		t.Fatal("rejected image downloaded a layer")
	}
	got, err := ctx.Store.ResolveLocalImage("", name)
	if err != nil || got.Manifest != a {
		t.Fatalf("rejected refresh moved reference: %v %v", got, err)
	}
	sign(b, true)
	if _, err := pullImage(ctx, name, false); err != nil {
		t.Fatal("valid signature refused:", err)
	}
	got, err = ctx.Store.ResolveLocalImage("", name)
	if err != nil || got.Manifest != b {
		t.Fatalf("signed refresh: %v %v", got, err)
	}
}
