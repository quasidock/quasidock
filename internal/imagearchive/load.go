package imagearchive

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/store"
)

type stagedArchive struct {
	json  map[string][]byte
	blobs map[string]store.ImportBlob
}

// Load validates the complete archive before importing any images. Both plain
// tar and gzip-compressed tar are accepted, without contacting a registry.
func Load(st *store.Store, src io.Reader, opts LoadOptions) ([]LoadedImage, error) {
	lock, err := st.Lock(false)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	dir, err := st.MakeTmpDir("image-load")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	archive, err := stageArchive(dir, src)
	if err != nil {
		return nil, err
	}
	in, result, err := archive.validate()
	if err != nil {
		return nil, err
	}
	if err := st.ImportImages(in, opts.Force); err != nil {
		return nil, err
	}
	return result, nil
}

func stageArchive(dir string, src io.Reader) (*stagedArchive, error) {
	br := bufio.NewReader(src)
	header, err := br.Peek(2)
	if err != nil {
		return nil, fmt.Errorf("reading image archive: %w", err)
	}
	var reader io.Reader = br
	if header[0] == 0x1f && header[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("reading gzip archive: %w", err)
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	archive := &stagedArchive{json: map[string][]byte{}, blobs: map[string]store.ImportBlob{}}
	seen := map[string]bool{}
	tr := tar.NewReader(reader)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading image tar: %w", err)
		}
		name, err := memberName(hdr.Name)
		if err != nil {
			return nil, err
		}
		isJSON := name == "oci-layout" || name == "index.json" || name == "quasidock.json"
		isBlob := strings.HasPrefix(name, "blobs/sha256/")
		if hdr.Typeflag == tar.TypeDir {
			if isJSON || (isBlob && oci.IsSha256Hex(strings.TrimPrefix(name, "blobs/sha256/"))) {
				return nil, fmt.Errorf("archive member %q must be a regular file", name)
			}
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("archive member %q must be a regular file or directory", hdr.Name)
		}
		if name == "" {
			return nil, errors.New("archive file has an empty path")
		}
		if !isJSON && !isBlob {
			// OCI layouts allow auxiliary files. Never extract their paths.
			continue
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate archive member %q", name)
		}
		seen[name] = true
		if isJSON {
			if hdr.Size > maxJSONSize {
				return nil, fmt.Errorf("%s exceeds 16 MiB", name)
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", name, err)
			}
			archive.json[name] = data
			continue
		}
		hex := strings.TrimPrefix(name, "blobs/sha256/")
		if !oci.IsSha256Hex(hex) {
			return nil, fmt.Errorf("invalid blob path %q", name)
		}
		blob, err := stageBlob(dir, tr, "sha256:"+hex)
		if err != nil {
			return nil, err
		}
		archive.blobs[blob.Digest] = blob
	}
	// Read through the gzip footer as well as any tar padding. Otherwise a
	// truncated or corrupt gzip stream could pass after tar's end marker.
	if _, err := io.Copy(zeroPadding{}, reader); err != nil {
		return nil, fmt.Errorf("finishing image archive: %w", err)
	}
	return archive, nil
}

func stageBlob(dir string, r io.Reader, digest string) (store.ImportBlob, error) {
	f, err := os.CreateTemp(dir, "blob-")
	if err != nil {
		return store.ImportBlob{}, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		return store.ImportBlob{}, fmt.Errorf("reading blob %s: %w", digest, err)
	}
	if fmt.Sprintf("sha256:%x", h.Sum(nil)) != digest {
		return store.ImportBlob{}, fmt.Errorf("%w: %s", store.ErrCorruptBlob, digest)
	}
	if err := f.Close(); err != nil {
		return store.ImportBlob{}, err
	}
	return store.ImportBlob{Path: f.Name(), Digest: digest, Size: n}, nil
}

func memberName(name string) (string, error) {
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	name = strings.TrimSuffix(name, "/")
	if name == "." || name == "" {
		return "", nil
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || part == "" {
			return "", fmt.Errorf("unsafe archive path %q", name)
		}
	}
	return name, nil
}

type zeroPadding struct{}

func (zeroPadding) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != 0 {
			return 0, errors.New("unexpected data after tar end marker")
		}
	}
	return len(p), nil
}

func (a *stagedArchive) validate() (store.ImageImport, []LoadedImage, error) {
	in := store.ImageImport{Images: map[string]store.Ref{}, Names: map[string]string{}, Defaults: map[string]string{}}
	var marker layout
	if err := a.readJSON("oci-layout", &marker); err != nil {
		return in, nil, err
	}
	if marker.Version != "1.0.0" {
		return in, nil, fmt.Errorf("unsupported OCI image layout version %q", marker.Version)
	}
	var index imageIndex
	if err := a.readJSON("index.json", &index); err != nil {
		return in, nil, err
	}
	if index.SchemaVersion != 2 || (index.MediaType != "" && index.MediaType != indexMediaType) || index.Manifests == nil {
		return in, nil, errors.New("unsupported or incomplete OCI image index")
	}
	used := map[string]store.ImportBlob{}
	readBlob := func(desc oci.Descriptor) (store.ImportBlob, error) {
		if err := checkDescriptor(desc); err != nil {
			return store.ImportBlob{}, err
		}
		blob, ok := a.blobs[desc.Digest]
		if !ok {
			return blob, fmt.Errorf("archive is missing blob %s", desc.Digest)
		}
		if blob.Size != desc.Size {
			return blob, fmt.Errorf("blob %s: descriptor size mismatch (%d vs %d)", desc.Digest, desc.Size, blob.Size)
		}
		used[desc.Digest] = blob
		return blob, nil
	}
	readJSONBlob := func(desc oci.Descriptor) ([]byte, error) {
		blob, err := readBlob(desc)
		if err != nil {
			return nil, err
		}
		if blob.Size > maxJSONSize {
			return nil, fmt.Errorf("JSON blob %s exceeds 16 MiB", blob.Digest)
		}
		return os.ReadFile(blob.Path)
	}
	for _, desc := range index.Manifests {
		if desc.MediaType == indexMediaType {
			return in, nil, errors.New("nested OCI image indexes are not supported by load")
		}
		if desc.MediaType != oci.MediaTypeManifest {
			// Unknown auxiliary descriptor types are allowed by OCI layouts.
			continue
		}
		if desc.Platform != nil && (desc.Platform.OS != "windows" || desc.Platform.Architecture != "amd64") {
			return in, nil, fmt.Errorf("image %s has unsupported platform %s/%s; expected windows/amd64", desc.Digest, desc.Platform.OS, desc.Platform.Architecture)
		}
		data, err := readJSONBlob(desc.Descriptor)
		if err != nil {
			return in, nil, err
		}
		if _, ok := in.Images[desc.Digest]; !ok {
			image, err := parseImage(desc.Digest, data, readJSONBlob)
			if err != nil {
				return in, nil, err
			}
			for _, layer := range image.Manifest.Layers {
				if _, err := readBlob(layer); err != nil {
					return in, nil, err
				}
			}
			in.Images[desc.Digest] = store.Ref{ID: image.Config.ID, Version: image.Config.Version, Manifest: desc.Digest}
		}
		if name, ok := desc.Annotations[refName]; ok {
			ref, err := registry.ParseReference(name)
			if err != nil || (ref.Tag == "" && ref.Digest == "") || (ref.Digest != "" && ref.Digest != desc.Digest) {
				return in, nil, fmt.Errorf("invalid image reference annotation %q: expected a full repository:tag or repository@digest", name)
			}
			if previous, ok := in.Names[name]; ok && previous != desc.Digest {
				return in, nil, fmt.Errorf("archive assigns image name %s to multiple manifests", name)
			}
			in.Names[name] = desc.Digest
		}
	}
	if len(in.Images) == 0 {
		return in, nil, errors.New("archive contains no supported Quasidock images")
	}
	if _, ok := a.json["quasidock.json"]; ok {
		var meta metadata
		if err := a.readJSON("quasidock.json", &meta); err != nil {
			return in, nil, err
		}
		if meta.Version != 1 || meta.Images == nil || meta.Defaults == nil {
			return in, nil, errors.New("unsupported or incomplete quasidock.json metadata")
		}
		for digest, origin := range meta.Images {
			ref, ok := in.Images[digest]
			if !ok {
				return in, nil, fmt.Errorf("quasidock.json describes an absent image %s", digest)
			}
			for _, repository := range origin.Repositories {
				r, err := registry.ParseReference(repository)
				if err != nil || r.Tag != "" || r.Digest != "" {
					return in, nil, fmt.Errorf("invalid repository provenance %q", repository)
				}
			}
			ref.Source, ref.Repositories = origin.Source, origin.Repositories
			in.Images[digest] = ref
		}
		in.Defaults = meta.Defaults
	}
	// Only referenced blobs are promoted; auxiliary content remains temporary.
	digests := make([]string, 0, len(used))
	for digest := range used {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	for _, digest := range digests {
		in.Blobs = append(in.Blobs, used[digest])
	}
	digests = digests[:0]
	for digest := range in.Images {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	result := make([]LoadedImage, 0, len(digests))
	for _, digest := range digests {
		image := LoadedImage{Digest: digest}
		for name, target := range in.Names {
			if target == digest {
				image.Names = append(image.Names, name)
			}
		}
		sort.Strings(image.Names)
		result = append(result, image)
	}
	return in, result, nil
}

func (a *stagedArchive) readJSON(name string, dst any) error {
	data, ok := a.json[name]
	if !ok {
		return fmt.Errorf("archive is missing %s", name)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}
	return nil
}
