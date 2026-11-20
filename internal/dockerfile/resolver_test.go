package dockerfile

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
)

func nativeImage(t *testing.T, reg *testregistry.Registry, image, version string) string {
	t.Helper()
	return nativeConfigImage(t, reg, image, version, oci.AppConfig{ID: image, Version: version, Entrypoint: []string{"app.exe"}})
}
func nativeConfigImage(t *testing.T, reg *testregistry.Registry, image, tag string, config oci.AppConfig) string {
	t.Helper()
	cfg, _ := json.Marshal(config)
	layer := []byte("fixture layer")
	var layers []oci.Descriptor
	for _, runtime := range config.From {
		layers = append(layers, oci.Descriptor{MediaType: oci.MediaTypeLayerTar, Digest: runtime.Layer, Size: int64(len(layer))})
	}
	layers = append(layers, oci.Descriptor{MediaType: oci.MediaTypeLayerTar, Digest: reg.PutBlob(layer), Size: int64(len(layer))})
	m, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: reg.PutBlob(cfg), Size: int64(len(cfg))}, Layers: layers})
	return reg.PutManifest("images/"+image, tag, m)
}

func TestStageOverridesResolvePinnedNativeFamilies(t *testing.T) {
	reg := testregistry.New()
	defer reg.Close()
	buildPin := nativeConfigImage(t, reg, "node", "24.1.0", oci.AppConfig{ID: "node", Version: "24.1.0", Entrypoint: []string{"node.exe"}, Permissions: oci.Permissions{Sandbox: oci.SandboxSoft}})
	runtimePin := nativeConfigImage(t, reg, "node", "26.1.0", oci.AppConfig{ID: "node", Version: "26.1.0", Entrypoint: []string{"node.exe"}, Permissions: oci.Permissions{Sandbox: oci.SandboxLow}})
	nativeImage(t, reg, "go", "1.26.0")
	nativeConfigImage(t, reg, "node", "24.9.0", oci.AppConfig{ID: "node", Version: "24.9.0", Entrypoint: []string{"node.exe"}, From: map[string]oci.Runtime{"node": {ID: "node", Version: "24.1.0", Image: buildPin, Layer: testregistry.Digest([]byte("fixture layer"))}}})
	ctx := context.New()
	var err error
	ctx.Store, err = store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx.Env[remote.RegistryVar] = reg.Host() + "/images"
	opts := options(t)
	opts.StageBases = map[string]string{"build": "node:24.1.0", "runtime": "node:26.1.0"}
	data := []byte("FROM node:custom AS build\nCOPY app.js /app.js\nFROM node:22 AS runtime\nCOPY --from=build /app.js /app.js\nCMD [\"node\",\"app.js\"]")
	result, err := Convert(data, opts, ImageResolver{Context: ctx})
	if err != nil {
		t.Fatalf("%v %v", err, result.Diagnostics)
	}
	if !strings.HasSuffix(result.Recipe.From["node"], runtimePin) || len(result.Recipe.Builds) != 1 || !strings.HasSuffix(result.Recipe.Builds[0].From["node"], buildPin) || result.Recipe.Permissions.Sandbox != oci.SandboxLow {
		t.Fatalf("per-stage native pins or runtime sandbox were lost: %+v", result.Recipe)
	}
	if reg.Count("/blobs/") != 2 {
		t.Fatal("conversion fetched more than the two native configs")
	}
	for _, image := range []string{"go:1.26.0", "node:24.9.0"} {
		opts.StageBases["build"] = image
		result, err := Convert(data, opts, ImageResolver{Context: ctx})
		if err == nil || result.Recipe != nil || !strings.Contains(fmt.Sprint(result.Diagnostics), "stage build (0):") || !strings.Contains(fmt.Sprint(result.Diagnostics), "standalone node image") {
			t.Fatalf("invalid native replacement %s accepted: %v %v", image, err, result.Diagnostics)
		}
	}
}

func TestDistroToolOverridesResolvePinnedNativeFamilies(t *testing.T) {
	reg := testregistry.New()
	defer reg.Close()
	pythonPin := nativeConfigImage(t, reg, "python-sdk", "latest", oci.AppConfig{ID: "python-sdk", Version: "3.14.10", Entrypoint: []string{"python.exe"}, Permissions: oci.Permissions{Sandbox: oci.SandboxLow}})
	gitPin := nativeConfigImage(t, reg, "mingit", "2.56.0", oci.AppConfig{ID: "mingit", Version: "2.56.0", Entrypoint: []string{"cmd/git.exe"}, Permissions: oci.Permissions{Sandbox: oci.SandboxSoft}})
	nativeImage(t, reg, "busybox", "latest")
	nativeImage(t, reg, "node", "latest")
	nativeConfigImage(t, reg, "python-sdk", "composite", oci.AppConfig{ID: "python-sdk", Version: "3.14.10", Entrypoint: []string{"python.exe"}, From: map[string]oci.Runtime{"node": {ID: "node", Version: "24", Image: gitPin, Layer: testregistry.Digest([]byte("fixture layer"))}}})
	ctx := context.New()
	var err error
	ctx.Store, err = store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx.Env[remote.RegistryVar] = reg.Host() + "/images"
	opts := options(t)
	opts.Tools = map[string]string{"git": "mingit:2.56.0"}
	data := []byte("FROM ubuntu:24.04 AS app\nRUN apt install python3 python3-pip git\nCMD [\"python3\",\"main.py\"]")
	result, err := Convert(data, opts, ImageResolver{Context: ctx})
	if err != nil || !strings.HasSuffix(result.Recipe.From["python"], pythonPin) || !strings.HasSuffix(result.Recipe.From["git"], gitPin) || result.Recipe.Permissions.Sandbox != oci.SandboxSoft {
		t.Fatalf("%v %+v", err, result)
	}
	if reg.Count("/blobs/") != 3 {
		t.Fatal("conversion must read only the three native config blobs")
	}
	for _, image := range []string{"node", "python-sdk:composite"} {
		opts.StageTools = map[string]map[string]string{"APP": {"python": image}}
		result, err := Convert(data, opts, ImageResolver{Context: ctx})
		if err == nil || result.Recipe != nil || !strings.Contains(fmt.Sprint(result.Diagnostics), "standalone python-sdk image") || !strings.Contains(fmt.Sprint(result.Diagnostics), "Dockerfile:2: RUN") {
			t.Fatalf("invalid tool %s: %v %v", image, err, result.Diagnostics)
		}
	}
}
func TestResolverVersionsAndOverride(t *testing.T) {
	reg := testregistry.New()
	defer reg.Close()
	nativeImage(t, reg, "node", "22.1.0")
	nativeImage(t, reg, "node", "24.1.0")
	wanted := nativeImage(t, reg, "node", "24.2.0")
	ctx := context.New()
	var err error
	ctx.Store, err = store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx.Env[remote.RegistryVar] = reg.Host() + "/images"
	r := ImageResolver{Context: ctx}
	dep, err := r.Resolve("node", "24", "")
	if err != nil || dep.Config.Version != "24.2.0" || !strings.HasSuffix(dep.Reference, wanted) {
		t.Fatalf("%+v %v", dep, err)
	}
	if _, err := r.Resolve("node", "23", ""); err == nil {
		t.Fatal("accepted nonexistent series")
	}
	dep, err = r.Resolve("node", "22", "node:24.1.0")
	if err != nil || dep.Config.Version != "24.1.0" {
		t.Fatalf("override: %+v %v", dep, err)
	}
	if reg.Count("/blobs/") != 2 {
		t.Fatalf("expected metadata only, got %d blob requests", reg.Count("/blobs/"))
	}
}
