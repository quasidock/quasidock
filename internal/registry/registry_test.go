package registry

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
)

func TestReferenceParse(t *testing.T) {
	hex := "07bb1e5b095b00d68a695481f9240879f33c5724b40aa2308f999d54ed78f075"
	r1, err := ParseReference("ghcr.io/owner/tools/node:24.21.0")
	if err != nil {
		t.Fatalf("ParseReference error: %v", err)
	}
	if r1.Host != "ghcr.io" || r1.Repository != "owner/tools/node" || r1.Tag != "24.21.0" || r1.ManifestRef() != "24.21.0" {
		t.Errorf("unexpected r1: %+v", r1)
	}

	r2, err := ParseReference("localhost:5000/busybox@sha256:" + hex)
	if err != nil {
		t.Fatalf("ParseReference error: %v", err)
	}
	if r2.Host != "localhost:5000" || r2.Tag != "" || r2.ManifestRef() != "sha256:"+hex {
		t.Errorf("unexpected r2: %+v", r2)
	}

	r3, err := ParseReference("docker.io/library/alpine")
	if err != nil {
		t.Fatalf("ParseReference error: %v", err)
	}
	if r3.ManifestRef() != "latest" {
		t.Errorf("expected latest, got %s", r3.ManifestRef())
	}

	bad := []string{"node", "owner/node:1", "ghcr.io/Owner/node", "ghcr.io/owner/node:", "ghcr.io/", "ghcr.io/a//b", "ghcr.io/a@sha256:xyz", "gh%cr.io/a"}
	for _, b := range bad {
		if _, err := ParseReference(b); err == nil {
			t.Errorf("expected error for %q", b)
		}
	}
}

func TestReferenceBaseURL(t *testing.T) {
	r1, _ := ParseReference("ghcr.io/a/b")
	if r1.BaseURL() != "https://ghcr.io" {
		t.Errorf("got %s, want https://ghcr.io", r1.BaseURL())
	}
	r2, _ := ParseReference("localhost:5000/b")
	if r2.BaseURL() != "http://localhost:5000" {
		t.Errorf("got %s, want http://localhost:5000", r2.BaseURL())
	}
	r3, _ := ParseReference("docker.io/library/alpine")
	if r3.BaseURL() != "https://registry-1.docker.io" {
		t.Errorf("got %s, want https://registry-1.docker.io", r3.BaseURL())
	}
}

func TestMayReceiveCredentials(t *testing.T) {
	if !MayReceiveCredentials("https://ghcr.io/token", false) {
		t.Error("expected true")
	}
	if !MayReceiveCredentials("HTTPS://auth.docker.io/token", false) {
		t.Error("expected true")
	}
	if MayReceiveCredentials("http://auth.example/token", false) {
		t.Error("expected false for http and non-local")
	}
	if !MayReceiveCredentials("http://localhost:5002/token", true) {
		t.Error("expected true for local http")
	}
}

func TestChallengeParse(t *testing.T) {
	c := ParseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:a/b:pull,push"`)
	if c == nil || c.Scheme != SchemeBearer || c.Realm != "https://ghcr.io/token" || c.Service != "ghcr.io" {
		t.Errorf("unexpected challenge: %+v", c)
	}

	c2 := ParseChallenge(`Basic realm="zot"`)
	if c2 == nil || c2.Scheme != SchemeBasic {
		t.Errorf("unexpected challenge: %+v", c2)
	}

	if c3 := ParseChallenge("Negotiate"); c3 != nil {
		t.Errorf("expected nil for Negotiate")
	}
}

func TestResolveURL(t *testing.T) {
	base := "https://ghcr.io/v2/a/b/blobs/sha256:1?x=1"
	cases := []struct {
		loc  string
		want string
	}{
		{"https://cdn.example/x", "https://cdn.example/x"},
		{"//cdn.example/x", "https://cdn.example/x"},
		{"/v2/other", "https://ghcr.io/v2/other"},
		{"next", "https://ghcr.io/v2/a/b/blobs/next"},
	}

	for _, c := range cases {
		got, err := resolveURL(base, c.loc)
		if err != nil {
			t.Fatalf("resolveURL(%q) err: %v", c.loc, err)
		}
		if got != c.want {
			t.Errorf("resolveURL(%q) = %q, want %q", c.loc, got, c.want)
		}
	}

	if a := authority(base); a != "ghcr.io" {
		t.Errorf("authority(%q) = %q, want ghcr.io", base, a)
	}
	if a := authority("http://localhost:5000"); a != "localhost:5000" {
		t.Errorf("authority = %q, want localhost:5000", a)
	}
}

func TestNextOffset(t *testing.T) {
	if o := nextOffset("0-1048575"); o == nil || *o != 1048576 {
		t.Errorf("unexpected offset: %v", o)
	}
	if o := nextOffset("bytes=0-65535"); o == nil || *o != 65536 {
		t.Errorf("unexpected offset: %v", o)
	}
	if o := nextOffset("0-0"); o == nil || *o != 0 {
		t.Errorf("unexpected offset: %v", o)
	}
	if o := nextOffset("garbage"); o != nil {
		t.Errorf("expected nil for garbage")
	}
}

func TestContentRangeStart(t *testing.T) {
	if s := contentRangeStart("bytes 6-10/35"); s == nil || *s != 6 {
		t.Errorf("unexpected start: %v", s)
	}
	if s := contentRangeStart("Bytes 0-4/*"); s == nil || *s != 0 {
		t.Errorf("unexpected start: %v", s)
	}
	if s := contentRangeStart("bytes */35"); s != nil {
		t.Errorf("expected nil")
	}
	if s := contentRangeStart("6-10/35"); s != nil {
		t.Errorf("expected nil")
	}
}

func TestTokenURL(t *testing.T) {
	got := TokenURL("https://ghcr.io/token", "ghcr.io", []string{"repository:zigsaw-io/app:pull,push", "repository:zigsaw-io/org.nodejs.node:pull"})
	want := "https://ghcr.io/token?service=ghcr.io&scope=repository%3Azigsaw-io%2Fapp%3Apull%2Cpush&scope=repository%3Azigsaw-io%2Forg.nodejs.node%3Apull"
	if got != want {
		t.Errorf("TokenURL = %q, want %q", got, want)
	}
}

func TestUploadsPath(t *testing.T) {
	got1 := uploadsPath("zigsaw-io/app", "", "")
	if got1 != "/v2/zigsaw-io/app/blobs/uploads/" {
		t.Errorf("uploadsPath = %s", got1)
	}
	got2 := uploadsPath("zigsaw-io/app", "sha256:ab", "zigsaw-io/org.nodejs.node")
	if got2 != "/v2/zigsaw-io/app/blobs/uploads/?mount=sha256%3Aab&from=zigsaw-io%2Forg.nodejs.node" {
		t.Errorf("uploadsPath = %s", got2)
	}
}

func TestMockRegistryManifestAndBlob(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/myrepo/manifests/latest":
			w.Header().Set("Content-Type", oci.MediaTypeManifest)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`))
		case "/v2/myrepo/blobs/sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855":
			if r.Method == "HEAD" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusOK)
			// empty body for empty sha256
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	ref := Reference{
		Host:       u.Host,
		Repository: "myrepo",
	}

	reg, err := New(ref, "pull", false)
	if err != nil {
		t.Fatal(err)
	}

	m, err := reg.FetchManifest()
	if err != nil {
		t.Fatalf("FetchManifest error: %v", err)
	}
	if len(m.Bytes) == 0 || !strings.HasPrefix(m.Digest, "sha256:") {
		t.Errorf("unexpected manifest: %+v", m)
	}

	has, err := reg.HasBlob("sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	if err != nil || !has {
		t.Errorf("HasBlob error: %v, has: %v", err, has)
	}

	tmpFile := filepath.Join(t.TempDir(), "blob.bin")
	h, err := reg.DownloadBlob(oci.Descriptor{
		Digest: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		Size:   0,
	}, tmpFile)
	if err != nil {
		t.Fatalf("DownloadBlob error: %v", err)
	}
	if h.Size != 0 {
		t.Errorf("expected 0 size, got %d", h.Size)
	}
}
