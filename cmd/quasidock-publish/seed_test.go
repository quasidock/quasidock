package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/quasidock/quasidock/internal/builder"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/publishing"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
)

func TestBusyboxSeedBuildsWithoutUpstream(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "upstream unavailable", http.StatusForbidden)
	}))
	defer upstream.Close()
	reg := testregistry.New()
	defer reg.Close()
	payload := []byte("fixture executable bytes")
	hash := testregistry.Digest(payload)[7:]
	data, err := json.Marshal(map[string]any{
		"id": "busybox", "version": "1", "entrypoint": "busybox.exe",
		"stages": []any{map[string]any{"name": "busybox", "sources": []any{map[string]any{
			"url": upstream.URL + "/busybox.exe", "sha256": hash, "dest": "busybox.exe",
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "busybox.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := publishing.Parse(file, data)
	if err != nil {
		t.Fatal(err)
	}
	newContext := func() *context.Context {
		t.Helper()
		st, err := store.Open(t.TempDir(), false, "")
		if err != nil {
			t.Fatal(err)
		}
		return &context.Context{Store: st, Env: map[string]string{remote.RegistryVar: reg.Host() + "/seeded"}}
	}
	ctx := newContext()
	local := filepath.Join(t.TempDir(), "busybox.exe")
	if err := os.WriteFile(local, payload, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := seedBusybox(ctx, r, local); err != nil {
		t.Fatal(err)
	}
	img, err := builder.Build(ctx, file, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ctx.Store.RegisterImage(store.Ref{ID: img.Config.ID, Version: img.Config.Version, Manifest: img.ManifestDigest, Source: img.Source}, ctx.Env[remote.RegistryVar]+"/busybox:1", true); err != nil {
		t.Fatal(err)
	}
	tag := "busybox:seed-" + hash
	if err := ctx.Store.TagImage(ctx.Env[remote.RegistryVar], "busybox", tag); err != nil {
		t.Fatal(err)
	}
	if err := remote.Push(ctx, tag, remote.PushOptions{Sources: true}); err != nil {
		t.Fatal(err)
	}
	// A new namespace has no seed yet. Recover only the pinned source from
	// the recipe's immutable mirror, without downloading executable layers.
	var mirroredRecipe map[string]any
	if err := json.Unmarshal(r.Bytes, &mirroredRecipe); err != nil {
		t.Fatal(err)
	}
	mirroredRecipe["source_mirrors"] = []string{reg.Host() + "/seeded/busybox@" + img.ManifestDigest}
	mirroredBytes, err := json.Marshal(mirroredRecipe)
	if err != nil {
		t.Fatal(err)
	}
	mirrored, err := publishing.Parse(file, mirroredBytes)
	if err != nil {
		t.Fatal(err)
	}
	newNamespace := newContext()
	newNamespace.Env[remote.RegistryVar] = reg.Host() + "/new-namespace"
	layerRequests := reg.Count("GET /v2/seeded/busybox/blobs/" + img.Manifest.Layers[0].Digest)
	if _, err := seedBusybox(newNamespace, mirrored, ""); err != nil {
		t.Fatal("seeding new namespace from source mirror:", err)
	}
	if reg.Count("GET /v2/seeded/busybox/blobs/"+img.Manifest.Layers[0].Digest) != layerRequests {
		t.Fatal("source mirror downloaded an executable layer")
	}
	if got, err := store.Sha256File(newNamespace.Store.Path("cache", "downloads", hash)); err != nil || got.Hex != hash {
		t.Fatalf("mirrored seed hash: %v %v", got, err)
	}
	fresh := newContext()
	cached, err := seedBusybox(fresh, r, "")
	if err != nil {
		t.Fatal(err)
	}
	// A damaged cache is repaired from the registry, and a wrong local input
	// must never overwrite the verified seed.
	if err := os.WriteFile(cached, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := seedBusybox(fresh, r, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("wrong binary"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := seedBusybox(fresh, r, local); err == nil {
		t.Fatal("accepted a local binary with the wrong SHA-256")
	}
	rebuilt, err := builder.Build(fresh, file, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ManifestDigest != img.ManifestDigest {
		t.Fatal("registry seed changed the built image")
	}
	if requests.Load() != 0 {
		t.Fatalf("contacted upstream %d time(s)", requests.Load())
	}
}
