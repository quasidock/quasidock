package imagearchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func fixtureLayer(t *testing.T, st *store.Store, kind format.Format) oci.Descriptor {
	t.Helper()
	dir := t.TempDir()
	blob, err := format.WriteLayer(filepath.Join(dir, "layer"), filepath.Join(dir, "tar"), kind, func(w io.Writer) error {
		lw := format.NewLayerWriter(w)
		if err := lw.AddFile("tool.exe", 4, strings.NewReader("tool")); err != nil {
			return err
		}
		return lw.Finish()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := st.PutBlobFile(blob.Path, store.FileHash{Hex: oci.DigestHex(blob.Digest), Size: blob.Size}, blob.MediaType)
	if err != nil {
		t.Fatal(err)
	}
	return blob.Descriptor(desc)
}

func fixtureImage(t *testing.T, st *store.Store, version string, layers []oci.Descriptor, names []string, makeDefault bool, customize ...func(*oci.AppConfig)) store.Ref {
	t.Helper()
	cfg := oci.AppConfig{ID: "archive-fixture", Version: version, Entrypoint: []string{"tool.exe"}}
	for _, f := range customize {
		f(&cfg)
	}
	mediaType := oci.MediaTypeConfig
	if cfg.FormatVersion == 4 {
		mediaType = oci.MediaTypeConfigV4
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	config, err := st.PutBlob(data, mediaType)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: config, Layers: layers})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := st.PutBlob(data, oci.MediaTypeManifest)
	if err != nil {
		t.Fatal(err)
	}
	ref := store.Ref{ID: cfg.ID, Version: cfg.Version, Manifest: manifest.Digest, Source: `C:\recipes\fixture.json`}
	if len(names) == 0 {
		names = []string{""}
	}
	for _, name := range names {
		if err := st.RegisterImage(ref, name, makeDefault); err != nil {
			t.Fatal(err)
		}
	}
	return ref
}

func saved(t *testing.T, st *store.Store, images ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := Save(st, "", images, &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func catalog(t *testing.T, st *store.Store) *store.ImageIndex {
	t.Helper()
	idx, err := st.ImageCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func assertCleanStaging(t *testing.T, st *store.Store) {
	t.Helper()
	entries, err := os.ReadDir(st.Path("tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging not cleaned: %v %v", entries, err)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, kind := range []format.Format{format.FormatTar, format.FormatGzip, format.FormatEstargz} {
		t.Run(kind.String(), func(t *testing.T) {
			source, dest := newStore(t), newStore(t)
			layer := fixtureLayer(t, source, kind)
			a := fixtureImage(t, source, "1", []oci.Descriptor{layer}, []string{"example.test/app:one", "example.test/alias:one"}, true)
			b := fixtureImage(t, source, "2", []oci.Descriptor{layer}, nil, true)
			if err := source.RegisterImage(a, "example.test/app@"+a.Manifest, false); err != nil {
				t.Fatal(err)
			}
			if _, err := source.PutBlob([]byte("unused"), ""); err != nil {
				t.Fatal(err)
			}
			data := saved(t, source, a.Manifest, b.Manifest, a.Manifest)
			if !bytes.Equal(data, saved(t, source, b.Manifest, a.Manifest)) {
				t.Fatal("archive depends on argument order or duplicate selections")
			}
			members := readMembers(t, data)
			counts := map[string]int{}
			for _, m := range members {
				counts[m.header.Name]++
				if strings.HasPrefix(m.header.Name, "blobs/sha256/") && m.header.Typeflag == tar.TypeReg {
					original, err := os.ReadFile(source.BlobPath(strings.TrimPrefix(m.header.Name, "blobs/sha256/")))
					if err != nil || !bytes.Equal(original, m.data) {
						t.Fatal("saved blob bytes changed", err)
					}
				}
			}
			if counts["blobs/sha256/"+oci.DigestHex(layer.Digest)] != 1 {
				t.Fatal("shared layer was not deduplicated")
			}
			loaded, err := Load(dest, bytes.NewReader(data), LoadOptions{})
			if err != nil || len(loaded) != 2 {
				t.Fatalf("load: %v %v", loaded, err)
			}
			if !reflect.DeepEqual(catalog(t, source), catalog(t, dest)) {
				t.Fatalf("catalog changed: source=%+v dest=%+v", catalog(t, source), catalog(t, dest))
			}
			if !bytes.Equal(data, saved(t, dest, a.Manifest, b.Manifest)) {
				t.Fatal("re-saving changed archive")
			}
			before := catalog(t, dest)
			if _, err := Load(dest, bytes.NewReader(data), LoadOptions{}); err != nil || !reflect.DeepEqual(before, catalog(t, dest)) {
				t.Fatal("repeated load changed catalog", err)
			}
			entries, err := os.ReadDir(dest.Path("deploy"))
			if err != nil || len(entries) != 0 {
				t.Fatal("load deployed layers", err)
			}
			assertCleanStaging(t, dest)
		})
	}
}

func TestLoadConflictsAndDefaults(t *testing.T) {
	source, dest := newStore(t), newStore(t)
	layer := fixtureLayer(t, source, format.FormatTar)
	a := fixtureImage(t, source, "1", []oci.Descriptor{layer}, []string{"example.test/app:one"}, false)
	b := fixtureImage(t, source, "2", []oci.Descriptor{layer}, []string{"example.test/app:two"}, true)
	old := fixtureImage(t, dest, "old", []oci.Descriptor{fixtureLayer(t, dest, format.FormatTar)}, []string{"example.test/app:one"}, true)
	pinned, err := dest.CreateContainer("pinned", store.ContainerRecord{Image: old.Manifest, AppID: old.ID, AppVersion: old.Version})
	if err != nil {
		t.Fatal(err)
	}
	data := saved(t, source, a.Manifest, b.Manifest)
	before := catalog(t, dest)
	if _, err := Load(dest, bytes.NewReader(data), LoadOptions{}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("conflict not reported: %v", err)
	}
	if !reflect.DeepEqual(before, catalog(t, dest)) {
		t.Fatal("conflict partially registered images")
	}
	if dest.HasBlob(a.Manifest) || dest.HasBlob(b.Manifest) {
		t.Fatal("conflict promoted blobs")
	}
	assertCleanStaging(t, dest)
	if _, err := Load(dest, bytes.NewReader(data), LoadOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	after := catalog(t, dest)
	if after.Names["example.test/app:one"] != a.Manifest || after.Defaults[old.ID] != old.Manifest || len(after.Images) != 3 {
		t.Fatalf("wrong force behavior: %+v", after)
	}
	if rec, err := dest.ReadContainer(pinned.ID); err != nil || rec.Image != old.Manifest {
		t.Fatal("existing container changed", err)
	}
	// An existing default must survive even when its own digest is imported.
	fresh := newStore(t)
	if _, err := Load(fresh, bytes.NewReader(saved(t, source, a.Manifest)), LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fresh, bytes.NewReader(data), LoadOptions{}); err != nil || catalog(t, fresh).Defaults[a.ID] != a.Manifest {
		t.Fatal("existing imported default was replaced", err)
	}
	// With no existing default, prefer the archive's original default.
	fresh = newStore(t)
	if _, err := Load(fresh, bytes.NewReader(data), LoadOptions{}); err != nil || catalog(t, fresh).Defaults[a.ID] != b.Manifest {
		t.Fatal("archive default was not restored", err)
	}
	// Without Quasidock metadata, choose a stable fallback and keep aliases.
	members := readMembers(t, data)
	members = slices.DeleteFunc(members, func(m testMember) bool { return m.header.Name == "quasidock.json" })
	fresh = newStore(t)
	if _, err := Load(fresh, bytes.NewReader(writeMembers(t, members)), LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if idx := catalog(t, fresh); idx.Defaults[a.ID] != min(a.Manifest, b.Manifest) || len(idx.Names) != 2 {
		t.Fatalf("metadata-free load: %+v", idx)
	}
}

func TestLoadPreservesExistingProvenance(t *testing.T) {
	source, dest := newStore(t), newStore(t)
	ref := fixtureImage(t, source, "1", []oci.Descriptor{fixtureLayer(t, source, format.FormatTar)}, []string{"example.test/app:one"}, true)
	data := saved(t, source, ref.Manifest)
	if _, err := Load(dest, bytes.NewReader(data), LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	ref.Source = "different local source"
	if err := dest.RegisterImage(ref, "other.test/app:local", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dest, bytes.NewReader(data), LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	got := catalog(t, dest).Images[ref.Manifest]
	if got.Source != ref.Source || !reflect.DeepEqual(got.Repositories, []string{"example.test/app", "other.test/app"}) {
		t.Fatalf("provenance not merged: %+v", got)
	}
}

func TestCommittedDataRoundTrip(t *testing.T) {
	source, dest := newStore(t), newStore(t)
	private := t.TempDir()
	if err := os.Mkdir(filepath.Join(private, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "saved.txt"), []byte("private state"), 0o600); err != nil {
		t.Fatal(err)
	}
	seed, err := source.WriteDataLayer(context.Background(), private)
	if err != nil {
		t.Fatal(err)
	}
	ref := fixtureImage(t, source, "saved", []oci.Descriptor{seed, fixtureLayer(t, source, format.FormatTar)}, nil, false, func(c *oci.AppConfig) {
		c.FormatVersion, c.DataLayer = 4, seed.Digest
		c.Labels = map[string]string{"purpose": "offline"}
		c.UnsetEnv = []string{"REMOVE_ME"}
	})
	if _, err := Load(dest, bytes.NewReader(saved(t, source, ref.Manifest)), LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest.Path("deploy", oci.DigestHex(seed.Digest))); !os.IsNotExist(err) {
		t.Fatal("seed deployed during load", err)
	}
	dataDir, err := dest.DataSeed(ref.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = dest.DeleteDeployment(seed.Digest) })
	if data, err := os.ReadFile(filepath.Join(dataDir, "saved.txt")); err != nil || string(data) != "private state" {
		t.Fatalf("seed not preserved: %q %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(dataDir, "empty")); err != nil || !info.IsDir() {
		t.Fatal("empty seed directory missing", err)
	}
	image, err := dest.ReadImage("saved", ref.Manifest)
	if err != nil || image.Config.Labels["purpose"] != "offline" || !reflect.DeepEqual(image.Config.UnsetEnv, []string{"REMOVE_ME"}) {
		t.Fatal("v4 config changed", err)
	}
}

func TestRuntimeLayersRoundTrip(t *testing.T) {
	source, dest := newStore(t), newStore(t)
	base := fixtureLayer(t, source, format.FormatGzip)
	app := fixtureLayer(t, source, format.FormatTar)
	ref := fixtureImage(t, source, "1", []oci.Descriptor{base, app}, nil, true, func(c *oci.AppConfig) {
		c.From = map[string]oci.Runtime{"base": {
			ID: "base", Version: "1", Image: "sha256:" + strings.Repeat("a", 64), Layer: base.Digest,
		}}
	})
	if _, err := Load(dest, bytes.NewReader(saved(t, source, ref.Manifest)), LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if !dest.HasBlob(base.Digest) || !dest.HasBlob(app.Digest) || len(catalog(t, dest).Images) != 1 {
		t.Fatal("runtime layer or app missing, or extra base manifest registered")
	}
}

type testMember struct {
	header tar.Header
	data   []byte
}

func readMembers(t *testing.T, data []byte) []testMember {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var members []testMember
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return members
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, testMember{header: *h, data: data})
	}
}

func writeMembers(t *testing.T, members []testMember) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for _, m := range members {
		h := m.header
		h.Size = int64(len(m.data))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(m.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func changeJSON(t *testing.T, members []testMember, name string, mutate func(map[string]any)) {
	t.Helper()
	for i := range members {
		if members[i].header.Name == name {
			var v map[string]any
			if err := json.Unmarshal(members[i].data, &v); err != nil {
				t.Fatal(err)
			}
			mutate(v)
			data, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(name, "blobs/sha256/") {
				replaceBlob(members, "sha256:"+strings.TrimPrefix(name, "blobs/sha256/"), data)
			} else {
				members[i].data = data
			}
			return
		}
	}
	t.Fatal("missing member", name)
}

// Rehash a changed JSON blob and its parent manifests, maintaining a valid
// content-addressed graph so validation tests reach semantic checks.
func replaceBlob(members []testMember, oldDigest string, data []byte) {
	newDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	for i := range members {
		if members[i].header.Name == "blobs/sha256/"+oci.DigestHex(oldDigest) {
			members[i].header.Name = "blobs/sha256/" + oci.DigestHex(newDigest)
			members[i].data = data
		}
	}
	for i := range members {
		if !json.Valid(members[i].data) || !bytes.Contains(members[i].data, []byte(oldDigest)) {
			continue
		}
		updated := bytes.ReplaceAll(members[i].data, []byte(oldDigest), []byte(newDigest))
		var value any
		if err := json.Unmarshal(updated, &value); err != nil {
			panic(err)
		}
		updateDescriptorSizes(value, newDigest, len(data))
		updated, _ = json.Marshal(value)
		if strings.HasPrefix(members[i].header.Name, "blobs/sha256/") {
			replaceBlob(members, "sha256:"+strings.TrimPrefix(members[i].header.Name, "blobs/sha256/"), updated)
		} else {
			members[i].data = updated
		}
	}
}

func updateDescriptorSizes(value any, digest string, size int) {
	switch value := value.(type) {
	case map[string]any:
		if value["digest"] == digest {
			value["size"] = size
		}
		for _, child := range value {
			updateDescriptorSizes(child, digest, size)
		}
	case []any:
		for _, child := range value {
			updateDescriptorSizes(child, digest, size)
		}
	}
}

func gzipArchive(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestGzipAndAuxiliaryFiles(t *testing.T) {
	source := newStore(t)
	ref := fixtureImage(t, source, "1", []oci.Descriptor{fixtureLayer(t, source, format.FormatTar)}, nil, true)
	members := readMembers(t, saved(t, source, ref.Manifest))
	members = append(members, testMember{header: tar.Header{Name: "notes.txt", Typeflag: tar.TypeReg, Mode: 0o644}, data: []byte("auxiliary")})
	unused := []byte("unreferenced blob")
	members = append(members, testMember{header: tar.Header{Name: fmt.Sprintf("blobs/sha256/%x", sha256.Sum256(unused)), Typeflag: tar.TypeReg}, data: unused})
	changeJSON(t, members, "index.json", func(v map[string]any) {
		v["manifests"] = append(v["manifests"].([]any), map[string]any{"mediaType": "application/example", "digest": "unavailable"})
	})
	for i := range members {
		members[i].header.Name = "./" + members[i].header.Name
	}
	compressed := gzipArchive(t, writeMembers(t, members))
	dest := newStore(t)
	if _, err := Load(dest, bytes.NewReader(compressed), LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if dest.HasBlob(fmt.Sprintf("sha256:%x", sha256.Sum256(unused))) || store.Exists(dest.Path("notes.txt")) {
		t.Fatal("auxiliary content was persisted")
	}
	for _, broken := range [][]byte{compressed[:len(compressed)-4], slices.Clone(compressed)} {
		if len(broken) == len(compressed) {
			broken[len(broken)-8] ^= 1
		}
		if _, err := Load(newStore(t), bytes.NewReader(broken), LoadOptions{}); err == nil {
			t.Fatal("accepted truncated or corrupt gzip footer")
		}
	}
}

func TestRejectInvalidArchives(t *testing.T) {
	source := newStore(t)
	layer := fixtureLayer(t, source, format.FormatTar)
	ref := fixtureImage(t, source, "1", []oci.Descriptor{layer}, []string{"example.test/app:one"}, true)
	data := saved(t, source, ref.Manifest)
	image, err := source.ReadImage("test", ref.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := "blobs/sha256/" + oci.DigestHex(ref.Manifest)
	configPath := "blobs/sha256/" + oci.DigestHex(image.Manifest.Config.Digest)
	tests := []struct {
		name   string
		mutate func([]testMember) []testMember
		want   string
	}{
		{"missing layout", func(m []testMember) []testMember {
			return slices.DeleteFunc(m, func(m testMember) bool { return m.header.Name == "oci-layout" })
		}, "missing oci-layout"},
		{"missing index", func(m []testMember) []testMember {
			return slices.DeleteFunc(m, func(m testMember) bool { return m.header.Name == "index.json" })
		}, "missing index.json"},
		{"layout version", func(m []testMember) []testMember {
			changeJSON(t, m, "oci-layout", func(v map[string]any) { v["imageLayoutVersion"] = "9" })
			return m
		}, "layout version"},
		{"index schema", func(m []testMember) []testMember {
			changeJSON(t, m, "index.json", func(v map[string]any) { v["schemaVersion"] = 1 })
			return m
		}, "image index"},
		{"empty index", func(m []testMember) []testMember {
			changeJSON(t, m, "index.json", func(v map[string]any) { v["manifests"] = []any{} })
			return m
		}, "no supported"},
		{"nested index", func(m []testMember) []testMember {
			changeJSON(t, m, "index.json", func(v map[string]any) { v["manifests"].([]any)[0].(map[string]any)["mediaType"] = indexMediaType })
			return m
		}, "nested OCI"},
		{"platform", func(m []testMember) []testMember {
			changeJSON(t, m, "index.json", func(v map[string]any) {
				v["manifests"].([]any)[0].(map[string]any)["platform"] = map[string]any{"os": "linux", "architecture": "amd64"}
			})
			return m
		}, "unsupported platform"},
		{"descriptor digest", func(m []testMember) []testMember {
			changeJSON(t, m, "index.json", func(v map[string]any) { v["manifests"].([]any)[0].(map[string]any)["digest"] = "sha256:bad" })
			return m
		}, "invalid descriptor"},
		{"descriptor size", func(m []testMember) []testMember {
			changeJSON(t, m, "index.json", func(v map[string]any) { v["manifests"].([]any)[0].(map[string]any)["size"] = 1 })
			return m
		}, "size mismatch"},
		{"manifest schema", func(m []testMember) []testMember {
			changeJSON(t, m, manifestPath, func(v map[string]any) { v["schemaVersion"] = 1 })
			return m
		}, "unsupported image manifest"},
		{"config type", func(m []testMember) []testMember {
			changeJSON(t, m, manifestPath, func(v map[string]any) {
				v["config"].(map[string]any)["mediaType"] = "application/vnd.oci.image.config.v1+json"
			})
			return m
		}, "not a quasidock app"},
		{"layer type", func(m []testMember) []testMember {
			changeJSON(t, m, manifestPath, func(v map[string]any) {
				v["layers"].([]any)[0].(map[string]any)["mediaType"] = "application/unsupported"
			})
			return m
		}, "layer of type"},
		{"layer relationship", func(m []testMember) []testMember {
			changeJSON(t, m, manifestPath, func(v map[string]any) { v["layers"] = []any{} })
			return m
		}, "config accounts for"},
		{"config validation", func(m []testMember) []testMember {
			changeJSON(t, m, configPath, func(v map[string]any) { v["id"] = "" })
			return m
		}, "id"},
		{"metadata version", func(m []testMember) []testMember {
			changeJSON(t, m, "quasidock.json", func(v map[string]any) { v["version"] = 9 })
			return m
		}, "quasidock.json"},
		{"absent default", func(m []testMember) []testMember {
			changeJSON(t, m, "quasidock.json", func(v map[string]any) { v["defaults"] = map[string]string{ref.ID: "sha256:" + strings.Repeat("a", 64)} })
			return m
		}, "invalid imported default"},
		{"invalid repository", func(m []testMember) []testMember {
			changeJSON(t, m, "quasidock.json", func(v map[string]any) {
				v["images"].(map[string]any)[ref.Manifest].(map[string]any)["repositories"] = []string{"bad path"}
			})
			return m
		}, "repository provenance"},
		{"missing layer", func(m []testMember) []testMember {
			return slices.DeleteFunc(m, func(m testMember) bool { return m.header.Name == "blobs/sha256/"+oci.DigestHex(layer.Digest) })
		}, "missing blob"},
		{"corrupt blob", func(m []testMember) []testMember {
			for i := range m {
				if m[i].header.Name == manifestPath {
					m[i].data = []byte("corrupt")
				}
			}
			return m
		}, "blob is corrupt"},
		{"duplicate member", func(m []testMember) []testMember { return append(m, m[1]) }, "duplicate archive member"},
		{"duplicate dot prefix", func(m []testMember) []testMember {
			extra := m[1]
			extra.header.Name = "./index.json"
			return append(m, extra)
		}, "duplicate archive member"},
		{"directory instead of metadata", func(m []testMember) []testMember {
			return append(m, testMember{header: tar.Header{Name: "index.json/", Typeflag: tar.TypeDir}})
		}, "must be a regular file"},
		{"symlink", func(m []testMember) []testMember {
			return append(m, testMember{header: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "index.json"}})
		}, "regular file or directory"},
		{"hardlink", func(m []testMember) []testMember {
			return append(m, testMember{header: tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "index.json"}})
		}, "regular file or directory"},
	}
	for _, name := range []string{"../escape", "/absolute", "/", "C:/drive", `dir\escape`, "dir/file:stream", "dir/../escape"} {
		tests = append(tests, struct {
			name   string
			mutate func([]testMember) []testMember
			want   string
		}{
			name, func(m []testMember) []testMember {
				kind := byte(tar.TypeReg)
				if name == "/" {
					kind = tar.TypeDir
				}
				return append(m, testMember{header: tar.Header{Name: name, Typeflag: kind}})
			}, "unsafe archive path",
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest := newStore(t)
			// A destination blob cannot fill a hole in an incomplete archive.
			original, err := source.ReadBlob(layer.Digest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := dest.PutBlob(original, layer.MediaType); err != nil {
				t.Fatal(err)
			}
			before := catalog(t, dest)
			broken := writeMembers(t, tt.mutate(readMembers(t, data)))
			if _, err := Load(dest, bytes.NewReader(broken), LoadOptions{Force: true}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want %q, got %v", tt.want, err)
			}
			if !reflect.DeepEqual(before, catalog(t, dest)) || dest.HasBlob(ref.Manifest) {
				t.Fatal("invalid archive partially registered")
			}
			assertCleanStaging(t, dest)
		})
	}
	// Cut inside a blob body, rather than only removing optional tar padding.
	truncated := data[:len(data)/2]
	if _, err := Load(newStore(t), bytes.NewReader(truncated), LoadOptions{}); err == nil {
		t.Fatal("accepted truncated tar")
	}
}

func TestRejectOversizedMetadata(t *testing.T) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	if err := tw.WriteHeader(&tar.Header{Name: "index.json", Size: maxJSONSize + 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	// The header alone must trigger the limit before reading or allocating its
	// advertised body. There is deliberately no body or tar end marker.
	dest := newStore(t)
	if _, err := Load(dest, bytes.NewReader(out.Bytes()), LoadOptions{}); err == nil || !strings.Contains(err.Error(), "exceeds 16 MiB") {
		t.Fatal("oversized metadata not rejected at header", err)
	}
	assertCleanStaging(t, dest)
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestSaveFailuresAndCorruptExistingBlob(t *testing.T) {
	source, dest := newStore(t), newStore(t)
	layer := fixtureLayer(t, source, format.FormatTar)
	ref := fixtureImage(t, source, "1", []oci.Descriptor{layer}, []string{"example.test/app:one"}, true)
	data := saved(t, source, ref.Manifest)
	writeErr := errors.New("closed output")
	if err := Save(source, "", []string{ref.Manifest}, errorWriter{writeErr}); !errors.Is(err, writeErr) {
		t.Fatal("write error not propagated", err)
	}
	if err := os.WriteFile(dest.BlobPath(oci.DigestHex(layer.Digest)), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dest, bytes.NewReader(data), LoadOptions{}); !errors.Is(err, store.ErrCorruptBlob) {
		t.Fatal("corrupt existing blob was reused", err)
	}
	if len(catalog(t, dest).Images) != 0 || dest.HasBlob(ref.Manifest) {
		t.Fatal("corrupt existing blob caused a partial import")
	}
	assertCleanStaging(t, dest)
	if err := os.Remove(source.BlobPath(oci.DigestHex(layer.Digest))); err != nil {
		t.Fatal(err)
	}
	// A projected lazy deployment is not a complete local blob.
	if err := os.MkdirAll(source.Path("lazy", oci.DigestHex(layer.Digest)), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Save(source, "", []string{ref.Manifest}, &out); !errors.Is(err, store.ErrMissingBlob) || !strings.Contains(err.Error(), "example.test/app@"+ref.Manifest) || out.Len() != 0 {
		t.Fatal("incomplete lazy image not rejected with guidance", err)
	}
}
