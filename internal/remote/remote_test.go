package remote

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/store"
)

func TestRemoteBlobReadAcrossCachedTail(t *testing.T) {
	data := []byte("0123456789")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "blob", time.Time{}, bytes.NewReader(data))
	}))
	defer server.Close()
	ref, err := registry.ParseReference(strings.TrimPrefix(server.URL, "http://") + "/fixture:1")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(ref, "pull", false)
	if err != nil {
		t.Fatal(err)
	}
	blob := &RemoteBlob{Reg: reg, Desc: oci.Descriptor{Digest: "sha256:" + strings.Repeat("a", 64), Size: int64(len(data))}, TailStart: 5, Tail: data[5:]}
	buf := make([]byte, 8)
	if n, err := blob.ReadAt(buf, 2); n != 8 || err != nil || string(buf) != "23456789" {
		t.Fatalf("crossing tail: n=%d err=%v data=%q", n, err, buf)
	}
	if blob.Fetched != 3 {
		t.Fatalf("fetched %d bytes, want only the 3 uncached bytes", blob.Fetched)
	}
	var tail bytes.Buffer
	if err := blob.fetchTo(7, 10, &tail); err != nil || tail.String() != "789" {
		t.Fatalf("cached tail: %q %v", tail.String(), err)
	}
	if n, err := blob.ReadAt(buf, 8); n != 2 || !errors.Is(err, io.EOF) || string(buf[:n]) != "89" {
		t.Fatalf("partial EOF: %d %v %q", n, err, buf[:n])
	}
}

func repeatStr(s string, count int) string {
	return strings.Repeat(s, count)
}

func TestExpandShort(t *testing.T) {
	mingit, err := ExpandShort(DefaultRegistry, "org.git_scm.MinGit:2.56.0.windows.1")
	if err != nil {
		t.Fatalf("ExpandShort error: %v", err)
	}
	if mingit == nil {
		t.Fatal("expected non-nil mingit")
	}
	if mingit.Text != "ghcr.io/quasidock/quasidock/org.git_scm.mingit:2.56.0.windows.1" {
		t.Errorf("got %s", mingit.Text)
	}
	if mingit.AppID != "org.git_scm.MinGit" {
		t.Errorf("got appID %s", mingit.AppID)
	}

	local, err := ExpandShort("localhost:5000/test/", "org.nodejs.node")
	if err != nil || local.Text != "localhost:5000/test/org.nodejs.node" {
		t.Errorf("unexpected local: %+v, err: %v", local, err)
	}

	r, err := ExpandShort("r.io", "net.frippery.busybox@sha256:ab")
	if err != nil || r.Text != "r.io/net.frippery.busybox@sha256:ab" {
		t.Errorf("unexpected r: %+v, err: %v", r, err)
	}

	// Full reference gives nil
	full, err := ExpandShort(DefaultRegistry, "ghcr.io/owner/app:1")
	if err != nil || full != nil {
		t.Errorf("expected nil for full reference")
	}

	// Bad IDs
	if _, err := ExpandShort(DefaultRegistry, "has space:1"); err == nil {
		t.Errorf("expected error for invalid ID")
	}
	if _, err := ExpandShort(DefaultRegistry, ":latest"); err == nil {
		t.Errorf("expected error for empty ID")
	}
}

func TestResolveRepositoryRegistry(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, registry, image, want string
	}{
		{"bare", "", "node", "ghcr.io/quasidock/quasidock/node:latest"},
		{"tag", "", "node:24", "ghcr.io/quasidock/quasidock/node:24"},
		{"digest", "", "node@" + digest, "ghcr.io/quasidock/quasidock/node@" + digest},
		{"override", "localhost:5000/custom/", "node", "localhost:5000/custom/node:latest"},
		{"explicit", "", "ghcr.io/quasidock/node:24", "ghcr.io/quasidock/node:24"},
		{"explicit with override", "localhost:5000/custom", "ghcr.io/other/node:24", "ghcr.io/other/node:24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := Resolve(tc.registry, tc.image)
			if err != nil {
				t.Fatal(err)
			}
			ref := target.Ref
			if ref.Tag == "" && ref.Digest == "" {
				ref.Tag = ref.ManifestRef()
			}
			if ref.String() != tc.want {
				t.Fatalf("resolved %s, want %s", ref, tc.want)
			}
			cachedName, err := store.ImageName(tc.registry, tc.image)
			if err != nil || cachedName != tc.want {
				t.Fatalf("cache name %s differs from remote: %v", cachedName, err)
			}
		})
	}
}

func TestIndexClassificationAndPlatformPick(t *testing.T) {
	ref, _ := registry.ParseReference("ghcr.io/a/b:1")
	d := "sha256:" + repeatStr("a", 64)
	w := "sha256:" + repeatStr("b", 64)

	dockerList := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json","manifests":[` +
		`{"mediaType":"application/vnd.docker.distribution.manifest.v2+json","digest":"` + d + `","size":1,"platform":{"architecture":"arm64","os":"linux","variant":"v8"}},` +
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + d + `","size":1,"platform":{"architecture":"unknown","os":"unknown"}},` +
		`{"mediaType":"application/vnd.docker.distribution.manifest.v2+json","digest":"` + w + `","size":1,"platform":{"architecture":"amd64","os":"windows","os.version":"10.0.26100.1"}}]}`

	cls, err := Classify(ref, []byte(dockerList))
	if err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	if !cls.IsIndex {
		t.Fatalf("expected IsIndex=true")
	}
	picked := PickPlatform(cls.Index)
	if picked == nil || picked.Digest != w {
		t.Errorf("picked = %v, want digest %s", picked, w)
	}

	// OCI index without windows/amd64
	linuxIndex := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + d + `","size":1,"platform":{"architecture":"amd64","os":"linux"}},` +
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + d + `","size":1,"platform":{"architecture":"amd64","os":"linux"}},` +
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + d + `","size":1}]}`

	clsLinux, err := Classify(ref, []byte(linuxIndex))
	if err != nil {
		t.Fatal(err)
	}
	if PickPlatform(clsLinux.Index) != nil {
		t.Errorf("expected no windows/amd64 picked")
	}

	desc := DescribePlatforms(clsLinux.Index)
	if desc != "with images for linux/amd64" {
		t.Errorf("DescribePlatforms = %q, want %q", desc, "with images for linux/amd64")
	}

	// Single image manifest
	imgJSON := `{"schemaVersion":2,"config":{"mediaType":"x","digest":"` + d + `","size":1},"layers":[]}`
	clsImg, err := Classify(ref, []byte(imgJSON))
	if err != nil {
		t.Fatal(err)
	}
	if clsImg.IsIndex || clsImg.Image == nil || clsImg.Image.Config.Digest != d {
		t.Errorf("unexpected clsImg: %+v", clsImg)
	}
}

func TestMountSources(t *testing.T) {
	nodeLayer := "sha256:" + repeatStr("a", 64)
	ownLayer := "sha256:" + repeatStr("b", 64)
	configDigest := "sha256:" + repeatStr("c", 64)

	manifest := oci.Manifest{
		Config: oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: configDigest, Size: 1},
		Layers: []oci.Descriptor{
			{MediaType: oci.MediaTypeLayerTarGzip, Digest: nodeLayer, Size: 1},
			{MediaType: oci.MediaTypeLayerTarGzip, Digest: ownLayer, Size: 1},
		},
	}
	config := oci.AppConfig{
		ID:         "prettier",
		Version:    "3",
		Entrypoint: []string{"${node}\\node.exe"},
		From: map[string]oci.Runtime{
			"node": {
				ID:      "node",
				Version: "24",
				Image:   "sha256:" + repeatStr("d", 64),
				Layer:   nodeLayer,
			},
		},
	}

	target, _ := registry.ParseReference("ghcr.io/quasidock/prettier:3")

	// Built locally
	built := MountSources(target, "D:\\recipes\\prettier.json", manifest, config)
	if built.ByDigest[nodeLayer] != "quasidock/node" {
		t.Errorf("built mount for node = %q", built.ByDigest[nodeLayer])
	}
	if _, ok := built.ByDigest[ownLayer]; ok {
		t.Errorf("expected no mount for own layer")
	}
	repos := built.Repositories()
	if len(repos) != 1 || repos[0] != "quasidock/node" {
		t.Errorf("unexpected repos: %v", repos)
	}

	// Pulled from elsewhere on same registry
	pulled := MountSources(target, "ghcr.io/me/prettier:3", manifest, config)
	for _, dig := range []string{configDigest, nodeLayer, ownLayer} {
		if pulled.ByDigest[dig] != "me/prettier" {
			t.Errorf("pulled mount for %s = %q", dig, pulled.ByDigest[dig])
		}
	}

	// Pulled from different registry
	other := MountSources(target, "docker.io/me/prettier:3", manifest, config)
	if len(other.ByDigest) != 1 || other.ByDigest[nodeLayer] != "quasidock/node" {
		t.Errorf("unexpected other mounts: %+v", other.ByDigest)
	}
}
