package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/quasidock/quasidock/internal/builder"
	"github.com/quasidock/quasidock/internal/catalog"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/testregistry"
)

func TestBuildUnitSourcePublicationRoundTrip(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	reg := testregistry.New()
	defer reg.Close()
	ctx.Env[remote.RegistryVar] = reg.Host() + "/published"
	var offline atomic.Bool
	var unusedRequests atomic.Int32
	payload := []byte("native graph executable fixture")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/unused.exe" {
			unusedRequests.Add(1)
			http.Error(w, "unused", 404)
			return
		}
		if offline.Load() {
			http.Error(w, "gone", 404)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()
	url, unusedURL := upstream.URL+"/app.exe", upstream.URL+"/unused.exe"
	hash, unusedHash := testregistry.Digest(payload)[7:], strings.Repeat("b", 64)
	app, dot := "app.exe", "."
	r := recipe.Recipe{ID: "unit-source-fixture", Version: "1", Entrypoint: recipe.Entrypoint{app}, Builds: []recipe.BuildUnit{
		{Name: "download", Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{{URL: &url, Sha256: &hash, Dest: &app}}}}},
		{Name: "copy", Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{{Build: &recipe.BuildSource{Name: "download", Paths: []string{app}}, Dest: &app}}}}},
		{Name: "unused", Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{{URL: &unusedURL, Sha256: &unusedHash, Dest: &app}}}}},
	}, Stages: []recipe.Stage{{Name: "package", Sources: []recipe.Source{{Build: &recipe.BuildSource{Name: "copy", Paths: []string{dot}}, Dest: &dot}}}}}
	file := filepath.Join(t.TempDir(), "quasidock.json")
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	image, err := builder.Build(ctx, file, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Install(ctx, image); err != nil {
		t.Fatal(err)
	}
	if _, err := imageCommand(ctx, []string{"push", "--sources", r.ID}); err != nil {
		t.Fatal(err)
	}
	if unusedRequests.Load() != 0 {
		t.Fatal("unused build inputs were downloaded")
	}
	if err := os.Remove(ctx.Store.Path("cache", "downloads", hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := imageCommand(ctx, []string{"push", "--sources", r.ID}); err == nil {
		t.Fatal("publication silently omitted a required unit URL source")
	}
	offline.Store(true)
	fresh, _ := fixtureCLI(t)
	fresh.Env[remote.RegistryVar] = ctx.Env[remote.RegistryVar]
	reproduced, err := builder.Build(fresh, file, builder.Options{})
	if err != nil || reproduced.ManifestDigest != image.ManifestDigest {
		t.Fatalf("graph source artifact did not reproduce: %v", err)
	}
}

func TestVendorSourceFallbackRestoresStageInputs(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	reg := testregistry.New()
	defer reg.Close()
	ctx.Env[remote.RegistryVar] = reg.Host() + "/published"
	base := []byte("original stage input\r\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(base) }))
	defer upstream.Close()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "dep.txt", Mode: 0644, Size: 3}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("dep"))
	tw.Close()
	vendorHash := testregistry.Digest(archive.Bytes())[7:]
	if err := os.WriteFile(ctx.Store.Path("cache", "downloads", vendorHash), archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	recipeFile := filepath.Join(t.TempDir(), "vendor.json")
	data, _ := json.Marshal(map[string]any{"id": "vendor-fixture", "version": "1", "entrypoint": "app.exe", "stages": []any{map[string]any{
		"name": "main", "shell": "cmd", "sources": []any{map[string]any{"url": upstream.URL + "/base.txt", "sha256": testregistry.Digest(base)[7:]}},
		"vendor": map[string]any{"commands": []string{"echo damaged>base.txt & exit /b 3"}, "dir": "deps", "sha256": vendorHash},
		"build":  []string{`copy /Y base.txt "%PREFIX%\app.exe"`},
	}}})
	if err := os.WriteFile(recipeFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	img, err := builder.Build(ctx, recipeFile, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Install(ctx, img); err != nil {
		t.Fatal(err)
	}
	if _, err := imageCommand(ctx, []string{"push", "--sources", "vendor-fixture"}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := fixtureCLI(t)
	fresh.Env[remote.RegistryVar] = ctx.Env[remote.RegistryVar]
	rebuilt, err := builder.Build(fresh, recipeFile, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ManifestDigest != img.ManifestDigest {
		t.Fatal("failed vendor command contaminated the recovered build")
	}
}

func TestSourcePublicationFallbackAndCandidateCatalog(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	reg := testregistry.New()
	defer reg.Close()
	ctx.Env[remote.RegistryVar] = reg.Host() + "/published"
	var fail atomic.Bool
	payload := []byte("fixture executable bytes")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "gone", 404)
			return
		}
		w.Write(payload)
	}))
	defer upstream.Close()
	hash := testregistry.Digest(payload)[7:]
	recipeFile := filepath.Join(t.TempDir(), "app.json")
	data, _ := json.Marshal(map[string]any{"id": "source-fixture", "version": "1", "entrypoint": "source.exe", "stages": []any{map[string]any{"name": "main", "sources": []any{map[string]any{"url": upstream.URL + "/source.exe", "sha256": hash}}}}})
	if err := os.WriteFile(recipeFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	img, err := builder.Build(ctx, recipeFile, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Install(ctx, img); err != nil {
		t.Fatal(err)
	}
	ref := ctx.Env[remote.RegistryVar] + "/source-fixture:ci-1"
	if _, err := imageCommand(ctx, []string{"tag", img.ManifestDigest, ref}); err != nil {
		t.Fatal(err)
	}
	if _, err := imageCommand(ctx, []string{"push", "--sources", ref}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.PushRefs(ctx.Store, ctx.Env[remote.RegistryVar], []string{ref}, "ci-1", false); err != nil {
		t.Fatal(err)
	}
	if reg.Count("PUT /v2/published/source-fixture/manifests/latest") != 0 || reg.Count("PUT /v2/published/catalog/manifests/latest") != 0 {
		t.Fatal("staging moved a release tag")
	}
	if err := catalog.PushRefs(ctx.Store, ctx.Env[remote.RegistryVar], []string{ref}, "latest", false); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Load(ctx.Store, ctx.Env[remote.RegistryVar], false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Catalog.Apps) != 1 || cat.Catalog.Apps[0].Manifest != img.ManifestDigest {
		t.Fatal("catalog did not preserve staged digest")
	}
	fail.Store(true)
	fresh, _ := fixtureCLI(t)
	fresh.Env[remote.RegistryVar] = ctx.Env[remote.RegistryVar]
	// The image exists only under its candidate tag; use that exact reference for
	// the low-level fallback before testing the recipe-version path.
	if _, err := remote.FetchSource(fresh.Store, fresh.Env[remote.RegistryVar], ref, hash, false); err != nil {
		t.Fatal(err)
	}
	if _, err := imageCommand(ctx, []string{"tag", img.ManifestDigest, "source-fixture:1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := imageCommand(ctx, []string{"push", "source-fixture:1"}); err != nil {
		t.Fatal(err)
	}
	freshAgain, _ := fixtureCLI(t)
	freshAgain.Env[remote.RegistryVar] = ctx.Env[remote.RegistryVar]
	rebuilt, err := builder.Build(freshAgain, recipeFile, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ManifestDigest != img.ManifestDigest {
		t.Fatal("source fallback changed the rebuilt image")
	}
	if err := os.WriteFile(ctx.Store.Path("cache", "downloads", hash), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := imageCommand(ctx, []string{"push", "--sources", ref}); err == nil {
		t.Fatal("published corrupt cached source")
	}
}
