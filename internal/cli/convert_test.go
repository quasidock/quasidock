package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/builder"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestConvertInspectsOnlyCopyMetadata(t *testing.T) {
	ctx, out := fixtureCLI(t)
	root, unrelated := t.TempDir(), t.TempDir()
	payload := filepath.Join(root, "payload.exe")
	if err := os.WriteFile(payload, []byte("conversion does not execute or read this"), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := win32.CreateFile(payload, win32.GENERIC_READ,
		win32.FILE_SHARE_WRITE|win32.FILE_SHARE_DELETE, nil, win32.OPEN_EXISTING, win32.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(h)
	// An unrelated output subtree must never be traversed for these COPYs.
	if err := os.Mkdir(filepath.Join(root, "out"), 0755); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(root, "out", "link")
	if data, err := exec.Command("cmd", "/c", "mklink", "/J", junction, unrelated).CombinedOutput(); err != nil {
		t.Fatalf("creating junction: %v: %s", err, data)
	}
	t.Cleanup(func() { _ = os.Remove(junction) })
	for _, tc := range []struct {
		name, dockerfile, wantError string
	}{
		{"no-copy", "FROM scratch\nCMD [\"/payload.exe\"]", ""},
		{"literal", "FROM scratch\nCOPY payload.exe /\nCMD [\"/payload.exe\"]", ""},
		{"glob", "FROM scratch\nCOPY *.exe /\nCMD [\"/payload.exe\"]", ""},
		{"build-unit", "FROM scratch AS build\nCOPY payload.exe /app.exe\nFROM scratch\nCOPY --from=build /app.exe /app.exe\nCMD [\"/app.exe\"]", ""},
		{"missing", "FROM scratch\nCOPY missing.exe /\nCMD [\"/missing.exe\"]", "Dockerfile:2: COPY:"},
		{"whole-context", "FROM scratch\nCOPY . /\nCMD [\"/payload.exe\"]", "link or junction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(tc.dockerfile), 0600); err != nil {
				t.Fatal(err)
			}
			code, err := convertCommand(ctx, []string{root, "--id", "metadata-only", "-o", "-"})
			if tc.wantError != "" {
				if err == nil || code == 0 || !strings.Contains(err.Error(), tc.wantError) || out.Len() != 0 {
					t.Fatalf("expected %q without output: code %d, error %v, output %s", tc.wantError, code, err, out)
				}
			} else if err != nil || code != 0 || !json.Valid(out.Bytes()) {
				t.Fatalf("conversion: code %d, error %v, output %s", code, err, out)
			}
		})
	}
}

func conversionBase(t *testing.T, reg *testregistry.Registry, id string) {
	t.Helper()
	b, _ := json.Marshal(oci.AppConfig{ID: id, Version: "24.1.0", Entrypoint: []string{"app.exe"}})
	m, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: reg.PutBlob(b), Size: int64(len(b))}, Layers: []oci.Descriptor{{MediaType: oci.MediaTypeLayerTar, Digest: reg.PutBlob([]byte("layer")), Size: 5}}})
	reg.PutManifest("images/"+id, "24.1.0", m)
	reg.PutManifest("images/"+id, "latest", m)
}
func TestConvertCommandOutputAndFailures(t *testing.T) {
	ctx, out := fixtureCLI(t)
	var diagnostics bytes.Buffer
	ctx.Stderr = &diagnostics
	reg := testregistry.New()
	defer reg.Close()
	conversionBase(t, reg, "node")
	conversionBase(t, reg, "busybox")
	ctx.Env[remote.RegistryVar] = reg.Host() + "/images"
	root := t.TempDir()
	df := filepath.Join(root, "Dockerfile")
	target := filepath.Join(root, "quasidock.json")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(df, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("FROM node:24\nWORKDIR /app\nCOPY app.js .\nCMD [\"node\",\"app.js\"]")
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("console.log('ok')"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, err := convertCommand(ctx, []string{root, "--id", "converted"}); err != nil || code != 0 {
		t.Fatalf("%d %v %s", code, err, &diagnostics)
	}
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var r recipe.Recipe
	if err := json.Unmarshal(original, &r); err != nil {
		t.Fatal(err)
	}
	if r.Workdir != "app" || r.Cmd[0] != "app.js" || !strings.Contains(r.From["node"], "@sha256:") {
		t.Fatalf("%+v", r)
	}
	if _, err := convertCommand(ctx, []string{root}); err == nil {
		t.Fatal("overwrote output without --force")
	}
	write("FROM node:24\nRUN apt-get update\nCMD [\"node\"]")
	if _, err := convertCommand(ctx, []string{root, "--force"}); err == nil {
		t.Fatal("unsupported conversion succeeded")
	}
	now, _ := os.ReadFile(target)
	if !bytes.Equal(now, original) {
		t.Fatal("failed conversion changed existing recipe")
	}
	if !strings.Contains(diagnostics.String(), "Dockerfile:2: RUN") {
		t.Fatal(diagnostics.String())
	}
	write("FROM node:24 AS build\nCOPY missing.js /app/app.js\nFROM node:24 AS runtime\nCOPY --from=build /app /app\nCMD [\"node\"]")
	if _, err := convertCommand(ctx, []string{root, "--force"}); err == nil {
		t.Fatal("missing context source in a build unit was not checked")
	} else if !strings.Contains(err.Error(), "Dockerfile:2: COPY: stage build (0)") {
		t.Fatal(err)
	}
	now, _ = os.ReadFile(target)
	if !bytes.Equal(now, original) {
		t.Fatal("multi-stage rejection changed existing recipe")
	}
	write("FROM node:24\nWORKDIR /app\nCOPY app.js .\nCMD [\"node\",\"app.js\"]")
	if _, err := convertCommand(ctx, []string{root, "-o", "-", "--id", "stdout-app"}); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out.Bytes()) {
		t.Fatal(out.String())
	}
	if _, err := convertCommand(ctx, []string{root, "--id", "new-id", "--force"}); err != nil {
		t.Fatal(err)
	}
	now, _ = os.ReadFile(target)
	if bytes.Equal(now, original) {
		t.Fatal("--force did not replace recipe")
	}
}

func TestConvertMultiStageCopyBuildAndRun(t *testing.T) {
	ctx, out := fixtureCLI(t)
	binary, payload, _ := buildContainerTestBinaries(t)
	root := t.TempDir()
	for name, data := range map[string][]byte{
		"fixture.exe": payload,
		"message.txt": []byte("copy-ok"),
		"Dockerfile":  []byte("FROM scratch AS source\nCOPY fixture.exe /out/app.exe\nCOPY message.txt /out/message.txt\nFROM scratch AS middle\nCOPY --from=source /out/ /package/\nCMD [\"/package/app.exe\",\"read-path\",\"package/message.txt\"]\nFROM scratch\nCOPY --from=1 /package/ /\nCMD [\"/app.exe\",\"read-path\",\"message.txt\"]"),
	} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if code, err := convertCommand(ctx, []string{root, "--id", "copy-graph"}); err != nil || code != 0 {
		t.Fatalf("conversion: %d %v", code, err)
	}
	file := filepath.Join(root, "quasidock.json")
	image, err := builder.Build(ctx, file, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(image.Manifest.Layers) != 1 || len(image.Config.From) != 0 {
		t.Fatal("builder dependencies leaked into runtime")
	}
	if err := lifecycle.Install(ctx, image); err != nil {
		t.Fatal(err)
	}
	run := func(want string) {
		t.Helper()
		command := exec.Command(binary, "run", "--rm", "--sandbox=soft", "copy-graph")
		command.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		data, err := command.CombinedOutput()
		if err != nil || string(data) != want {
			t.Fatalf("converted graph run: %q %v", data, err)
		}
	}
	run("copy-ok")
	if err := os.WriteFile(filepath.Join(root, "message.txt"), []byte("updated"), 0600); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := builder.Build(ctx, file, builder.Options{})
	if err != nil || rebuilt.ManifestDigest == image.ManifestDigest {
		t.Fatalf("live source did not invalidate a converted graph: %v", err)
	}
	if err := lifecycle.Install(ctx, rebuilt); err != nil {
		t.Fatal(err)
	}
	run("updated")
	out.Reset()
	if _, err := convertCommand(ctx, []string{root, "-o", "-", "--id", "copy-stdout"}); err != nil || !json.Valid(out.Bytes()) {
		t.Fatalf("multi-stage stdout is not JSON: %s %v", out, err)
	}
	if _, err := convertCommand(ctx, []string{root, "--target=MIDDLE", "--id", "copy-graph", "--force"}); err != nil {
		t.Fatal(err)
	}
	selected, err := builder.Build(ctx, file, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Config.Entrypoint[0] != "${app}/package/app.exe" {
		t.Fatal("selected target startup was not used:", selected.Config.Entrypoint)
	}
	if err := lifecycle.Install(ctx, selected); err != nil {
		t.Fatal(err)
	}
	run("updated")
}

func TestConvertTargetAndStageBaseFlags(t *testing.T) {
	ctx, out := fixtureCLI(t)
	var diagnostics bytes.Buffer
	ctx.Stderr = &diagnostics
	reg := testregistry.New()
	defer reg.Close()
	conversionBase(t, reg, "node")
	ctx.Env[remote.RegistryVar] = reg.Host() + "/images"
	root := t.TempDir()
	for name, value := range map[string]string{
		"Dockerfile": "FROM node:custom AS build\nCOPY app.js /app.js\nFROM build AS preview\nENV MODE=preview\nCMD [\"node\",\"app.js\"]\nFROM alpine AS production\nRUN apt-get update\nCOPY missing.txt /missing.txt",
		"app.js":     "console.log('preview')",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, flags := range [][]string{
		{"--target", "PREVIEW", "--stage-base", "BUILD=node:24.1.0"},
		{"--target=preview", "--stage-base=0=node:24.1.0", "--stage-base=production=unavailable:1"},
		{"--target=build", "--base=node:24.1.0"},
	} {
		out.Reset()
		diagnostics.Reset()
		args := append([]string{root, "-o", "-", "--id", "selected"}, flags...)
		if code, err := convertCommand(ctx, args); err != nil || code != 0 {
			t.Fatalf("%v: %d %v %s", flags, code, err, &diagnostics)
		}
		var r recipe.Recipe
		if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.From["node"] == "" || len(r.Builds) > 1 {
			t.Fatalf("invalid selected recipe: %s %v", out, err)
		}
		if !strings.Contains(diagnostics.String(), "selected conversion target") || !strings.Contains(diagnostics.String(), "stage production (2): unused stage") {
			t.Fatal(diagnostics.String())
		}
	}
	if reg.Count("unavailable") != 0 || reg.Count("/blobs/") != 1 {
		t.Fatal("unused overrides or native layers were downloaded")
	}
	out.Reset()
	if _, err := convertCommand(ctx, []string{"--help"}); err != nil || !strings.Contains(out.String(), "--target NAME") || !strings.Contains(out.String(), "--stage-base NAME_OR_INDEX=IMAGE") {
		t.Fatalf("help omits target flags: %s %v", out, err)
	}
}

func TestConvertInvalidTargetFlagsPreserveOutput(t *testing.T) {
	ctx, out := fixtureCLI(t)
	var diagnostics bytes.Buffer
	ctx.Stderr = &diagnostics
	root := t.TempDir()
	df := filepath.Join(root, "Dockerfile")
	file := filepath.Join(root, "quasidock.json")
	if err := os.WriteFile(df, []byte("FROM node AS app\nFROM app AS inherited\nFROM scratch AS empty\nCMD [\"/app.exe\"]"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("keep existing recipe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{
		{"--target"}, {"--target="}, {"--target", "--force"},
		{"--target", "app", "--target=app"}, {"--target=missing"}, {"--target=0"},
		{"--stage-base"}, {"--stage-base=app"}, {"--stage-base=app="}, {"--stage-base==node"},
		{"--stage-base=APP=node", "--stage-base=app=node"},
		{"--stage-base=0=node", "--stage-base=app=node"},
		{"--target=app", "--base=node", "--stage-base=app=node"},
		{"--stage-base=unknown=node"}, {"--stage-base=inherited=node"}, {"--stage-base=empty=node"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			out.Reset()
			diagnostics.Reset()
			args := append([]string{root, "--force", "--id", "invalid"}, flags...)
			if code, err := convertCommand(ctx, args); err == nil || code == 0 {
				t.Fatal("invalid flags succeeded")
			}
			current, err := os.ReadFile(file)
			if err != nil || string(current) != "keep existing recipe" || out.Len() != 0 {
				t.Fatalf("failure changed output: %s %s %v", current, out, err)
			}
		})
	}
}

func TestConvertRejectsLiteralExecutableCollisionsBeforeWrite(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	reg := testregistry.New()
	defer reg.Close()
	conversionBase(t, reg, "go")
	conversionBase(t, reg, "busybox")
	ctx.Env[remote.RegistryVar] = reg.Host() + "/images"
	root := t.TempDir()
	data := "FROM golang AS build\nCOPY . /out/\nRUN go build -o /out/server .\nFROM scratch\nCOPY --from=build /out/server /server\nCMD [\"/server\"]"
	for name, value := range map[string]string{"Dockerfile": data, "server.exe": "literal collision", "quasidock.json": "unchanged"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := convertCommand(ctx, []string{root, "--id", "collision", "--force"}); err == nil || !strings.Contains(err.Error(), "Dockerfile:2: COPY: stage build (0)") || !strings.Contains(err.Error(), "collides with mapped executable") {
		t.Fatalf("missing collision diagnostic: %v", err)
	}
	current, err := os.ReadFile(filepath.Join(root, "quasidock.json"))
	if err != nil || string(current) != "unchanged" {
		t.Fatal("failed collision check changed output")
	}
}

func TestContextSourceBuildWorkingDirectoryAndRestart(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	binary, payload, _ := buildContainerTestBinaries(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.exe"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("workdir-ok"), 0600); err != nil {
		t.Fatal(err)
	}
	dest := "app/"
	r := recipe.Recipe{ID: "context-fixture", Version: "1", Workdir: "app", Entrypoint: recipe.Entrypoint{"app/fixture.exe", "read-path", "payload.txt"}, Permissions: oci.Permissions{Sandbox: oci.SandboxSoft}, Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{{Context: &recipe.ContextSource{Root: ".", Dockerfile: "Dockerfile", Paths: []string{"fixture.exe", "payload.txt"}}, Dest: &dest}}}}}
	data, _ := json.Marshal(r)
	file := filepath.Join(root, "quasidock.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	img, err := builder.Build(ctx, file, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if img.Manifest.Config.MediaType != oci.MediaTypeConfigV3 {
		t.Fatal(img.Manifest.Config.MediaType)
	}
	if err := lifecycle.Install(ctx, img); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) string {
		t.Helper()
		c := exec.Command(binary, args...)
		c.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s %v", args, out, err)
		}
		return string(out)
	}
	if out := call("run", "--rm", "context-fixture"); out != "workdir-ok" {
		t.Fatalf("foreground: %q", out)
	}
	if out := call("run", "--rm", "context-fixture", "fixture.exe", "read-path", "payload.txt"); out != "workdir-ok" {
		t.Fatalf("workdir command override: %q", out)
	}
	call("run", "-d", "--name", "wd-context", "context-fixture")
	wait := func() {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			rec, err := ctx.Store.ResolveContainer("wd-context")
			if err == nil && !rec.State.Running && rec.State.Status == "exited" {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("container did not exit")
	}
	wait()
	call("start", "wd-context")
	wait()
	if out := call("logs", "wd-context"); strings.Count(out, "workdir-ok") != 2 {
		t.Fatalf("restart logs: %q", out)
	}
	rec, err := ctx.Store.ResolveContainer("wd-context")
	if err != nil || filepath.Base(rec.Config.WorkingDir) != "app" {
		t.Fatalf("%+v %v", rec, err)
	}
	call("rm", "wd-context")
	if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := builder.Build(ctx, file, builder.Options{})
	if err != nil || rebuilt.ManifestDigest == img.ManifestDigest {
		t.Fatalf("live source did not invalidate cache: %v", err)
	}
}
