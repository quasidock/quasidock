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
)

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
