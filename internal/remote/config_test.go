package remote

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
)

func configFixture(t *testing.T) (*testregistry.Registry, []byte, oci.Descriptor) {
	t.Helper()
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	config, err := json.Marshal(oci.AppConfig{ID: "config-fixture", Version: "1", Entrypoint: []string{"app.exe"}})
	if err != nil {
		t.Fatal(err)
	}
	desc := oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: reg.PutBlob(config), Size: int64(len(config))}
	layer := []byte("unused layer contents")
	manifest, err := json.Marshal(oci.Manifest{
		SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: desc,
		Layers: []oci.Descriptor{{MediaType: oci.MediaTypeLayerTar, Digest: reg.PutBlob(layer), Size: int64(len(layer))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg.PutManifest("app", "1", manifest)
	return reg, config, desc
}

func TestFetchInfoWithoutStore(t *testing.T) {
	reg, _, _ := configFixture(t)
	info, err := FetchInfo(nil, "", reg.Host()+"/app:1", false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.ID != "config-fixture" || info.Config.Version != "1" {
		t.Fatalf("unexpected config: %+v", info.Config)
	}
}

func TestFetchInfoIndependentOfSystemTemp(t *testing.T) {
	reg, config, desc := configFixture(t)
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	// Config caching must use the store's filesystem even when Windows TEMP
	// points elsewhere. An unusable system temp makes this reproducible with
	// one drive as well as on runners with C: TEMP and a D: store.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(key, blocked)
	}
	image := reg.Host() + "/app:1"
	for i := 0; i < 2; i++ {
		info, err := FetchInfo(st, "", image, false)
		if err != nil {
			t.Fatal(err)
		}
		if info.Config.ID != "config-fixture" {
			t.Fatalf("unexpected config: %+v", info.Config)
		}
	}
	got, err := st.ReadBlob(desc.Digest)
	if err != nil || !bytes.Equal(got, config) {
		t.Fatalf("cached config differs: %v", err)
	}
	if count := reg.Count("GET /v2/app/blobs/" + desc.Digest); count != 1 {
		t.Fatalf("config downloaded %d times, want once", count)
	}
	entries, err := os.ReadDir(st.Path("tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files remain: %v %v", entries, err)
	}
}

func TestFetchInfoFailedConfigLeavesNoTemporaryFile(t *testing.T) {
	reg, _, desc := configFixture(t)
	reg.BreakBlob(desc.Digest)
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FetchInfo(st, "", reg.Host()+"/app:1", false); err == nil {
		t.Fatal("accepted a failed config download")
	}
	if st.HasBlob(desc.Digest) {
		t.Fatal("cached a failed download")
	}
	entries, err := os.ReadDir(st.Path("tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files remain: %v %v", entries, err)
	}
}
