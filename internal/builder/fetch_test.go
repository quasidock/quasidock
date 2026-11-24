package builder

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
)

func TestSourceMirrorPreservesPinnedBytes(t *testing.T) {
	reg := testregistry.New()
	defer reg.Close()
	payload := []byte("exact pinned source archive")
	digest := reg.PutBlob(payload)
	image, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest})
	imageDigest := reg.PutManifest("old/app", "1", image)
	sourceTag, err := oci.SourcesTag(imageDigest)
	if err != nil {
		t.Fatal(err)
	}
	sources, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest,
		Config: oci.Descriptor{MediaType: oci.MediaTypeSourcesConfig},
		Layers: []oci.Descriptor{{Digest: digest, Size: int64(len(payload)), MediaType: oci.MediaTypeSource}}})
	reg.PutManifest("old/app", sourceTag, sources)
	var corrupt atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/blobs/"+digest) && corrupt.Load() {
			_, _ = w.Write(bytes.Repeat([]byte("!"), len(payload)))
			return
		}
		reg.Server.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	mirror := strings.TrimPrefix(proxy.URL, "http://") + "/old/app@" + imageDigest
	// A 200 response containing HTML must never be accepted as the archive.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>Access denied</title>"))
	}))
	defer upstream.Close()
	url, hash := upstream.URL+"/source.tar.xz", strings.TrimPrefix(digest, "sha256:")
	src := recipe.Source{URL: &url, Sha256: &hash}
	for _, tamper := range []bool{false, true} {
		st, err := store.Open(t.TempDir(), false, "")
		if err != nil {
			t.Fatal(err)
		}
		ctx := &context.Context{Store: st, Env: map[string]string{remote.RegistryVar: reg.Host() + "/new"}}
		corrupt.Store(tamper)
		f := NewSources(ctx, "", "app:1", nil, mirror)
		defer f.Close()
		if err := f.fetcher.FetchAll([]recipe.Source{src}); err != nil {
			if !tamper {
				t.Fatal(err)
			}
			if !strings.Contains(err.Error(), "doesn't match its digest") {
				t.Fatalf("expected hash verification failure: %v", err)
			}
			if _, err := os.Stat(st.Path("cache", "downloads", hash)); !os.IsNotExist(err) {
				t.Fatal("cached corrupt mirrored source")
			}
			continue
		}
		if tamper {
			t.Fatal("accepted corrupt mirrored source")
		}
		got, err := os.ReadFile(st.Path("cache", "downloads", hash))
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("wrong recovered source: %q %v", got, err)
		}
	}
}
