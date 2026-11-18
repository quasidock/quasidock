package builder

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/buildcontext"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/tree"
)

func unitFixture(t *testing.T) (*context.Context, *bytes.Buffer) {
	t.Helper()
	ctx := context.New()
	var err error
	ctx.Store, err = store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		entries, err := os.ReadDir(ctx.Store.Path("deploy"))
		if err != nil {
			t.Error(err)
			return
		}
		for _, entry := range entries {
			if entry.IsDir() && oci.IsSha256Hex(entry.Name()) {
				if deleted, err := ctx.Store.DeleteDeployment("sha256:" + entry.Name()); err != nil || deleted == store.DeletionInUse {
					t.Errorf("releasing test deployment: %v (in use: %t)", err, deleted == store.DeletionInUse)
				}
			}
		}
	})
	var log bytes.Buffer
	ctx.Stdout, ctx.Stderr = &log, &log
	return ctx, &log
}

func unitFile(t *testing.T, root, name, value string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeUnitRecipe(t *testing.T, file string, r recipe.Recipe) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recipe.Parse(file, data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func unitSource(name, dest string, paths ...string) recipe.Source {
	return recipe.Source{Build: &recipe.BuildSource{Name: name, Paths: paths}, Dest: &dest}
}

func contextSource(dest string, paths ...string) recipe.Source {
	return recipe.Source{Context: &recipe.ContextSource{Root: ".", Dockerfile: "Dockerfile", Paths: paths}, Dest: &dest}
}

func unitLayerFiles(t *testing.T, ctx *context.Context, image *oci.Image) map[string]string {
	t.Helper()
	blob, err := ctx.Store.ReadBlob(oci.OwnLayer(&image.Manifest).Digest)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	result := map[string]string{}
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		result[header.Name] = string(data)
	}
	return result
}

func copyGraph() recipe.Recipe {
	return recipe.Recipe{
		ID: "unit-app", Version: "1", Entrypoint: recipe.Entrypoint{"app.exe"},
		Builds: []recipe.BuildUnit{
			{Name: "child", Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{unitSource("BASE", ".", "."), contextSource("out/app.exe", "replacement.txt")}}}},
			{Name: "base", Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{contextSource("out/", "dist"), contextSource("secret.txt", "secret.txt")}}}},
			{Name: "unused", SDK: map[string]string{"unused": "localhost:1/unused@sha256:" + strings.Repeat("a", 64)}, Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{{Context: &recipe.ContextSource{Root: "missing", Dockerfile: "Dockerfile", Paths: []string{"."}}, Dest: newString(".")}}}}},
		},
		Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{
			unitSource("child", "app.exe", "out/app.exe"),
			unitSource("base", "original.exe", "out/app.exe"),
			unitSource("base", "assets/", "out/assets"),
		}}},
	}
}

func newString(value string) *string { return &value }

func prepareCopyGraph(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	unitFile(t, root, "dist/app.exe", "original")
	unitFile(t, root, "dist/assets/data.txt", "data")
	unitFile(t, root, "replacement.txt", "child")
	unitFile(t, root, "secret.txt", "build-only")
	if err := os.MkdirAll(filepath.Join(root, "dist", "assets", "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "quasidock.json")
}

func TestBuildUnitsCopyGraphIsolationAndCache(t *testing.T) {
	ctx, _ := unitFixture(t)
	file := prepareCopyGraph(t)
	r := copyGraph()
	writeUnitRecipe(t, file, r)
	image, err := Build(ctx, file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	files := unitLayerFiles(t, ctx, image)
	if files["app.exe"] != "child" || files["original.exe"] != "original" || files["assets/data.txt"] != "data" {
		t.Fatal(files)
	}
	if _, exists := files["assets/empty/"]; !exists {
		t.Fatal("empty output directory was lost:", files)
	}
	if len(image.Manifest.Layers) != 1 || len(image.Config.From) != 0 || image.Config.Build == nil {
		t.Fatalf("builder files or runtime dependencies leaked: %+v", image)
	}
	for name := range files {
		if strings.Contains(name, "secret") || strings.Contains(name, "replacement") {
			t.Fatal("builder input leaked into final layer:", name)
		}
	}
	order, err := r.ReachableBuilds()
	if err != nil || len(order) != 2 || order[0].Name != "base" || order[1].Name != "child" {
		t.Fatalf("units were not ordered once each: %v %v", order, err)
	}
	entries, _ := os.ReadDir(ctx.Store.Path("tmp"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "build-unit-outputs") {
			t.Fatal("unit snapshots were not released:", entry.Name())
		}
	}
	cached, err := Build(ctx, file, Options{})
	if err != nil || cached.ManifestDigest != image.ManifestDigest {
		t.Fatalf("whole-build cache was not reused: %v", err)
	}
	unitFile(t, filepath.Dir(file), "dist/app.exe", "new-parent")
	rebuilt, err := Build(ctx, file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ManifestDigest == image.ManifestDigest || unitLayerFiles(t, ctx, rebuilt)["original.exe"] != "new-parent" {
		t.Fatal("builder context edit returned stale output")
	}
	data, _ := os.ReadFile(file)
	inputs, err := TakeSnapshots(ctx, &r, file)
	if err != nil {
		t.Fatal(err)
	}
	key, err := inputHash(ctx, &r, data, file, inputs, true)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a different complete image under this input key to distinguish cache
	// lookup from execution, even though the real build is deterministic.
	if err := ctx.Store.WriteBuildCache(key, image.ManifestDigest); err != nil {
		t.Fatal(err)
	}
	fromCache, err := Build(ctx, file, Options{})
	if err != nil || fromCache.ManifestDigest != image.ManifestDigest {
		t.Fatalf("whole-build cache lookup failed: %v", err)
	}
	for _, options := range []Options{{Rebuild: true}, {Clean: true}, {KeepBuildDir: true}} {
		if err := ctx.Store.WriteBuildCache(key, image.ManifestDigest); err != nil {
			t.Fatal(err)
		}
		if result, err := Build(ctx, file, options); err != nil || result.ManifestDigest != rebuilt.ManifestDigest {
			t.Fatalf("build flags skipped graph execution: %+v %v", options, err)
		}
	}
}

func TestReachableBuildOrderRejectsInvalidDirectInputs(t *testing.T) {
	r := copyGraph()
	r.Builds[1].Stages[0].Sources = []recipe.Source{unitSource("child", ".", ".")}
	if _, err := r.ReachableBuilds(); err == nil || !strings.Contains(err.Error(), "circular") {
		t.Fatalf("direct cyclic graph was accepted: %v", err)
	}
	r = copyGraph()
	r.Stages[0].Sources[0].Build.Name = "missing"
	if _, err := r.ReachableBuilds(); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("direct missing reference was accepted: %v", err)
	}
	r = copyGraph()
	order, _ := r.ReachableBuilds()
	var names []string
	for _, unit := range order {
		names = append(names, unit.Name)
	}
	if !reflect.DeepEqual(names, []string{"base", "child"}) {
		t.Fatal(names)
	}
}

func TestBuildUnitContextCacheInvalidation(t *testing.T) {
	for _, change := range []string{"addition", "deletion", "ignore edit"} {
		t.Run(change, func(t *testing.T) {
			ctx, _ := unitFixture(t)
			file := prepareCopyGraph(t)
			writeUnitRecipe(t, file, copyGraph())
			before, err := Build(ctx, file, Options{})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "addition":
				unitFile(t, filepath.Dir(file), "dist/added.txt", "added")
			case "deletion":
				if err := os.Remove(filepath.Join(filepath.Dir(file), "dist", "assets", "data.txt")); err != nil {
					t.Fatal(err)
				}
			case "ignore edit":
				unitFile(t, filepath.Dir(file), ".dockerignore", "*.tmp\n")
			}
			after, err := Build(ctx, file, Options{})
			if err != nil || after.ManifestDigest == before.ManifestDigest {
				t.Fatalf("builder %s returned stale cached output: %v", change, err)
			}
		})
	}
}

func TestUnitOutputSnapshotsOwnTheirBytes(t *testing.T) {
	original := t.TempDir()
	unitFile(t, original, "app.exe", "parent")
	parentTree, err := buildcontext.ReadOutput(original)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := captureUnitOutput(parentTree, filepath.Join(t.TempDir(), "parent"))
	if err != nil {
		t.Fatal(err)
	}
	unitFile(t, original, "app.exe", "mutated producer")
	selected, err := parent.Copy([]string{"."}, ".", true)
	if err != nil {
		t.Fatal(err)
	}
	child := t.TempDir()
	if err := selected.WriteFiles(child); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(child, "app.exe")); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := parent.WriteTar(&archive); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&archive)
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(reader)
	if string(data) != "parent" {
		t.Fatalf("producer mutation or child deletion changed parent: %q", data)
	}
}

func TestBuildUnitOutputCopyFailures(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		change        func(*recipe.Recipe)
	}{
		{"missing", "COPY source", func(r *recipe.Recipe) { r.Stages[0].Sources[0].Build.Paths = []string{"missing"} }},
		{"glob to file", "multiple sources", func(r *recipe.Recipe) { r.Stages[0].Sources[0].Build.Paths = []string{"out/*"} }},
		{"file directory collision", "path conflict", func(r *recipe.Recipe) { r.Stages[0].Sources[1].Dest = newString("app.exe/data") }},
		{"invalid output name", "valid Windows filename", func(r *recipe.Recipe) { r.Builds[1].Stages[0].Sources[1].Dest = newString("CON") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := unitFixture(t)
			file := prepareCopyGraph(t)
			r := copyGraph()
			tc.change(&r)
			writeUnitRecipe(t, file, r)
			if _, err := Build(ctx, file, Options{}); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want %q, got %v", tc.message, err)
			} else if tc.name == "missing" && strings.Contains(err.Error(), ".dockerignore") {
				t.Fatal("build output lookup used a context diagnostic:", err)
			}
		})
	}
}

func TestBuildUnitsShareContextSnapshotAndRecheck(t *testing.T) {
	ctx, _ := unitFixture(t)
	file := prepareCopyGraph(t)
	r := copyGraph()
	r.Stages[0].Sources = append(r.Stages[0].Sources, contextSource("live.txt", "replacement.txt"))
	r.Stages[0].Sources[3].Context.Dockerfile = "./Dockerfile"
	writeUnitRecipe(t, file, r)
	snapshots, err := TakeSnapshots(ctx, &r, file)
	if err != nil {
		t.Fatal(err)
	}
	if snapshots.Get("copy", 3).Context != snapshots.units["base"].Get("copy", 0).Context || snapshots.units["child"].Get("copy", 1).Context != snapshots.Get("copy", 3).Context {
		t.Fatal("scopes captured different versions of the same context")
	}
	unitFile(t, filepath.Dir(file), "added.txt", "added")
	if err := snapshots.recheckGraph(ctx); err == nil {
		t.Fatal("source additions were not detected across the graph")
	}
}

func TestBuildUnitArchivesSurviveScopeRelease(t *testing.T) {
	ctx, _ := unitFixture(t)
	var payload bytes.Buffer
	archive := zip.NewWriter(&payload)
	entry, err := archive.Create("app.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("archived executable")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload.Bytes()) }))
	defer upstream.Close()
	url, hash, kind := upstream.URL+"/app.zip", testregistry.Digest(payload.Bytes())[7:], recipe.KindFile
	r := recipe.Recipe{ID: "unit-archives", Version: "1", Entrypoint: recipe.Entrypoint{"app.exe"}, Builds: []recipe.BuildUnit{{
		Name: "archive", Stages: []recipe.Stage{{Name: "unpack", Sources: []recipe.Source{
			{URL: &url, Sha256: &hash},
			{URL: &url, Sha256: &hash, Type: &kind, Dest: newString("payload.zip")},
		}}},
	}}, Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{unitSource("archive", "./", "app.exe", "payload.zip")}}}}
	file := filepath.Join(t.TempDir(), "quasidock.json")
	writeUnitRecipe(t, file, r)
	image, err := Build(ctx, file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	files := unitLayerFiles(t, ctx, image)
	if files["app.exe"] != "archived executable" || files["payload.zip"] != payload.String() {
		t.Fatal("archive handles closed before snapshots were copied, or a build source extracted an archive")
	}
	if !reflect.DeepEqual(image.Config.Build.Sources, []string{hash}) {
		t.Fatal("unit URL sources missing from build provenance:", image.Config.Build)
	}
}

func putUnitTool(t *testing.T, ctx *context.Context, id, version, command string, env map[string]string, from map[string]string) *oci.Image {
	t.Helper()
	root := t.TempDir()
	script := filepath.Join(root, "tool.cmd")
	unitFile(t, root, "tool.cmd", command)
	tool := recipe.Recipe{ID: id, Version: version, Entrypoint: recipe.Entrypoint{"tool.cmd"}, Env: env, From: from, Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{{Path: &script, Dest: newString("tool.cmd")}}}}}
	file := filepath.Join(root, "quasidock.json")
	writeUnitRecipe(t, file, tool)
	image, err := Build(ctx, file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func TestBuildUnitsSeparateNativeBasesWithoutRuntimeLayers(t *testing.T) {
	ctx, _ := unitFixture(t)
	one := putUnitTool(t, ctx, "unit-tool", "1", "@echo one\r\n", nil, nil)
	two := putUnitTool(t, ctx, "unit-tool", "2", "@echo two\r\n", nil, nil)
	file := prepareCopyGraph(t)
	r := copyGraph()
	r.Builds[1].From = map[string]string{"tool": "localhost/unit-tool@" + one.ManifestDigest}
	r.Builds[0].From = map[string]string{"tool": "localhost/unit-tool@" + two.ManifestDigest}
	writeUnitRecipe(t, file, r)
	image, err := Build(ctx, file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(image.Manifest.Layers) != 1 || len(image.Config.From) != 0 || image.Config.Build.SDK["base/from/tool"] != one.ManifestDigest || image.Config.Build.SDK["child/from/tool"] != two.ManifestDigest {
		t.Fatalf("unit base versions were merged or leaked into runtime: %+v", image)
	}
	// Distinct versions are supported across units, while one scope continues
	// to reject ambiguous native tool IDs.
	r.Builds[1].From["conflict"] = "localhost/unit-tool@" + two.ManifestDigest
	writeUnitRecipe(t, file, r)
	if _, err := Build(ctx, file, Options{}); err == nil || !strings.Contains(err.Error(), "two versions") {
		t.Fatalf("conflicting tools within one unit were accepted: %v", err)
	}
}

func requireUnitBuildDrive(t *testing.T) {
	t.Helper()
	target, err := queryDosDevice(DriveLetter)
	if err != nil {
		t.Fatal(err)
	}
	if target != "" && !IsBuildRoot(target) {
		t.Skipf("command execution requires %s; it is already mapped to %s", DriveLetter, target)
	}
}

func TestBuildUnitsCommandIsolationAndToolVersions(t *testing.T) {
	requireUnitBuildDrive(t)
	ctx, _ := unitFixture(t)
	helper := putUnitTool(t, ctx, "unit-helper", "1", "@echo runtime-helper\r\n", map[string]string{"UNIT_HELPER": "available"}, nil)
	one := putUnitTool(t, ctx, "unit-tool", "1", "@echo one\r\n", map[string]string{"UNIT_VERSION": "one", "UNIT_FIRST": "yes"}, map[string]string{"helper": "localhost/unit-helper@" + helper.ManifestDigest})
	two := putUnitTool(t, ctx, "unit-tool", "2", "@echo two\r\n", map[string]string{"UNIT_VERSION": "two"}, nil)
	local := filepath.Join(t.TempDir(), "quasidock.json")
	r := recipe.Recipe{ID: "unit-commands", Version: "1", Entrypoint: recipe.Entrypoint{"one.exe"}, Builds: []recipe.BuildUnit{
		{Name: "first", SDK: map[string]string{"tool": "localhost/unit-tool@" + one.ManifestDigest}, Stages: []recipe.Stage{{Name: "compile", Shell: recipe.ShellCmd, Build: []string{`if not "%UNIT_HELPER%"=="available" exit /b 21`, `call tool.cmd > "%PREFIX%\one.exe"`}}}},
		{Name: "second", From: map[string]string{"tool": "localhost/unit-tool@" + two.ManifestDigest}, Stages: []recipe.Stage{{Name: "compile", Shell: recipe.ShellCmd, Build: []string{`if defined UNIT_FIRST exit /b 22`, `if defined UNIT_HELPER exit /b 23`, `call tool.cmd > "%PREFIX%\two.exe"`}}}},
	}, Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{unitSource("first", "one.exe", "one.exe"), unitSource("second", "two.exe", "two.exe")}}}}
	writeUnitRecipe(t, local, r)
	image, err := Build(ctx, local, Options{})
	if err != nil {
		t.Fatal(err)
	}
	files := unitLayerFiles(t, ctx, image)
	if strings.TrimSpace(files["one.exe"]) != "one" || strings.TrimSpace(files["two.exe"]) != "two" || len(image.Manifest.Layers) != 1 || len(image.Config.From) != 0 {
		t.Fatalf("scope isolation failed: %v %+v", files, image.Config)
	}
	if image.Config.Build.SDK["first/sdk/tool"] != one.ManifestDigest || image.Config.Build.SDK["second/from/tool"] != two.ManifestDigest {
		t.Fatal("unit tool provenance is incomplete:", image.Config.Build)
	}
}

func TestBuildUnitCommandChildDeletionPreservesParent(t *testing.T) {
	requireUnitBuildDrive(t)
	ctx, _ := unitFixture(t)
	file := filepath.Join(t.TempDir(), "quasidock.json")
	r := recipe.Recipe{ID: "unit-deletion", Version: "1", Entrypoint: recipe.Entrypoint{"app.exe"}, Builds: []recipe.BuildUnit{
		{Name: "parent", Stages: []recipe.Stage{{Name: "compile", Shell: recipe.ShellCmd, Build: []string{`echo parent> "%PREFIX%\app.exe"`}}}},
		{Name: "child", Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{unitSource("parent", ".", ".")}}, {Name: "delete", Shell: recipe.ShellCmd, Build: []string{`del "%PREFIX%\app.exe"`, `echo child> "%PREFIX%\child.txt"`}}}},
	}, Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{unitSource("child", "child.txt", "child.txt"), unitSource("parent", "app.exe", "app.exe")}}}}
	writeUnitRecipe(t, file, r)
	image, err := Build(ctx, file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	files := unitLayerFiles(t, ctx, image)
	if strings.TrimSpace(files["app.exe"]) != "parent" || strings.TrimSpace(files["child.txt"]) != "child" {
		t.Fatal(files)
	}
}

func TestCaptureUnitOutputRejectsUnsafeVirtualPaths(t *testing.T) {
	output := tree.New()
	if err := output.AddDir("../escape"); err != nil {
		t.Fatal(err)
	}
	if _, err := captureUnitOutput(output, filepath.Join(t.TempDir(), "output")); err == nil {
		t.Fatal("unsafe virtual output was materialized")
	}
}
