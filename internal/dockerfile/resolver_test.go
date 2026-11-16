package dockerfile

import (
	"encoding/json"
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
	cfg, _ := json.Marshal(oci.AppConfig{ID: image, Version: version, Entrypoint: []string{"app.exe"}})
	layer := []byte("fixture layer")
	m, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: reg.PutBlob(cfg), Size: int64(len(cfg))}, Layers: []oci.Descriptor{{MediaType: oci.MediaTypeLayerTar, Digest: reg.PutBlob(layer), Size: int64(len(layer))}}})
	return reg.PutManifest("images/"+image, version, m)
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
