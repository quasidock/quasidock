// Package testregistry supplies a loopback-only OCI registry for integration
// tests. It has no dependency on a downloaded server or container runtime.
package testregistry

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

type Registry struct {
	Server     *httptest.Server
	mu         sync.Mutex
	blobs      map[string][]byte
	manifests  map[string]string
	requests   map[string]int
	fail       bool
	brokenBlob string
}

func New() *Registry {
	r := &Registry{blobs: map[string][]byte{}, manifests: map[string]string{}, requests: map[string]int{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	return r
}

func Digest(data []byte) string             { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }
func (r *Registry) Host() string            { return strings.TrimPrefix(r.Server.URL, "http://") }
func (r *Registry) Close()                  { r.Server.Close() }
func (r *Registry) Fail(on bool)            { r.mu.Lock(); defer r.mu.Unlock(); r.fail = on }
func (r *Registry) BreakBlob(digest string) { r.mu.Lock(); defer r.mu.Unlock(); r.brokenBlob = digest }
func (r *Registry) Count(fragment string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for key, n := range r.requests {
		if strings.Contains(key, fragment) {
			count += n
		}
	}
	return count
}
func (r *Registry) PutBlob(data []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := Digest(data)
	r.blobs[d] = append([]byte(nil), data...)
	return d
}
func (r *Registry) PutManifest(repo, tag string, data []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := Digest(data)
	r.blobs[d] = append([]byte(nil), data...)
	r.manifests[repo+"/"+tag] = d
	r.manifests[repo+"/"+d] = d
	return d
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests[req.Method+" "+req.URL.Path]++
	if r.fail {
		http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
		return
	}
	if req.URL.Path == "/v2/" {
		w.WriteHeader(http.StatusOK)
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/v2/")
	if i := strings.Index(path, "/manifests/"); i >= 0 {
		repo, tag := path[:i], path[i+len("/manifests/"):]
		if req.Method == http.MethodPut {
			data, err := io.ReadAll(req.Body)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			var manifest struct {
				SchemaVersion int    `json:"schemaVersion"`
				MediaType     string `json:"mediaType"`
			}
			if json.Unmarshal(data, &manifest) != nil || manifest.SchemaVersion != 2 || manifest.MediaType != "application/vnd.oci.image.manifest.v1+json" {
				http.Error(w, "invalid OCI manifest", http.StatusBadRequest)
				return
			}
			d := Digest(data)
			r.blobs[d] = data
			r.manifests[repo+"/"+tag] = d
			r.manifests[repo+"/"+d] = d
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusCreated)
			return
		}
		d, ok := r.manifests[repo+"/"+tag]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", d)
		w.Write(r.blobs[d])
		return
	}
	if i := strings.Index(path, "/blobs/uploads/"); i >= 0 {
		if req.Method == http.MethodPost {
			w.Header().Set("Location", "/v2/"+path+"fixture")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == http.MethodPut {
			data, _ := io.ReadAll(req.Body)
			d := Digest(data)
			if d != req.URL.Query().Get("digest") {
				http.Error(w, "bad digest", 400)
				return
			}
			r.blobs[d] = data
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusCreated)
			return
		}
	}
	if i := strings.Index(path, "/blobs/"); i >= 0 {
		d := path[i+len("/blobs/"):]
		data, ok := r.blobs[d]
		if !ok {
			http.NotFound(w, req)
			return
		}
		if d == r.brokenBlob {
			http.Error(w, "interrupted blob", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Header().Set("Docker-Content-Digest", d)
		if req.Method != http.MethodHead {
			w.Write(data)
		}
		return
	}
	http.NotFound(w, req)
}
