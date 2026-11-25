package imagearchive

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/store"
)

// Save writes the selected local images and all their aliases to dst. It never
// contacts a registry. An error writing a stream may leave a partial archive.
func Save(st *store.Store, envRegistry string, images []string, dst io.Writer) error {
	if len(images) == 0 {
		return errors.New("save requires at least one image")
	}
	lock, err := st.Lock(false)
	if err != nil {
		return err
	}
	defer lock.Release()
	catalog, err := st.ImageCatalog()
	if err != nil {
		return err
	}
	selected := map[string]store.Ref{}
	for _, name := range images {
		ref, err := catalog.Resolve(envRegistry, name)
		if err != nil {
			return err
		}
		selected[ref.Manifest] = *ref
	}
	digests := make([]string, 0, len(selected))
	for digest := range selected {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	index := imageIndex{SchemaVersion: 2, MediaType: indexMediaType, Manifests: []indexDescriptor{}}
	meta := metadata{Version: 1, Images: map[string]provenance{}, Defaults: map[string]string{}}
	blobs := map[string]oci.Descriptor{}
	for _, digest := range digests {
		ref := selected[digest]
		manifestBytes, err := readStoreJSON(st, digest)
		if errors.Is(err, os.ErrNotExist) {
			return missingBlob(ref, digest)
		}
		if err != nil {
			return err
		}
		image, err := parseImage(digest, manifestBytes, func(desc oci.Descriptor) ([]byte, error) {
			if desc.Size > maxJSONSize {
				return nil, errors.New("image config exceeds 16 MiB")
			}
			data, err := readStoreJSON(st, desc.Digest)
			if errors.Is(err, os.ErrNotExist) {
				return nil, missingBlob(ref, desc.Digest)
			}
			if err == nil && int64(len(data)) != desc.Size {
				return nil, fmt.Errorf("blob %s: descriptor size mismatch", desc.Digest)
			}
			return data, err
		})
		if err != nil {
			return err
		}
		if !strings.EqualFold(ref.ID, image.Config.ID) || ref.Version != image.Config.Version {
			return fmt.Errorf("image catalog metadata disagrees with manifest %s", digest)
		}
		manifest := oci.Descriptor{MediaType: oci.MediaTypeManifest, Digest: digest, Size: int64(len(manifestBytes))}
		all := append([]oci.Descriptor{manifest, image.Manifest.Config}, image.Manifest.Layers...)
		for _, desc := range all {
			if previous, ok := blobs[desc.Digest]; ok && previous.Size != desc.Size {
				return fmt.Errorf("blob %s: inconsistent descriptor sizes", desc.Digest)
			}
			info, err := os.Stat(st.BlobPath(oci.DigestHex(desc.Digest)))
			if errors.Is(err, os.ErrNotExist) {
				return missingBlob(ref, desc.Digest)
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() != desc.Size {
				return fmt.Errorf("%w: %s (size or file type mismatch)", store.ErrCorruptBlob, desc.Digest)
			}
			blobs[desc.Digest] = desc
		}
		names := []string{}
		for name, target := range catalog.Names {
			if target == digest {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		if len(names) == 0 {
			names = append(names, "")
		}
		for _, name := range names {
			desc := indexDescriptor{Descriptor: manifest, Platform: &platform{OS: "windows", Architecture: "amd64"}}
			if name != "" {
				desc.Annotations = map[string]string{refName: name}
			}
			index.Manifests = append(index.Manifests, desc)
		}
		repositories := slices.Clone(ref.Repositories)
		sort.Strings(repositories)
		meta.Images[digest] = provenance{Source: ref.Source, Repositories: slices.Compact(repositories)}
		if catalog.Defaults[strings.ToLower(ref.ID)] == digest {
			meta.Defaults[strings.ToLower(ref.ID)] = digest
		}
	}
	// Prepare all JSON before writing any bytes to the output.
	jsonFiles := map[string][]byte{}
	for name, value := range map[string]any{"oci-layout": layout{Version: "1.0.0"}, "index.json": index, "quasidock.json": meta} {
		data, err := jsonBytes(value)
		if err != nil {
			return err
		}
		jsonFiles[name] = data
	}
	tw := tar.NewWriter(dst)
	for _, name := range []string{"oci-layout", "index.json", "quasidock.json"} {
		data := jsonFiles[name]
		if err := writeMember(tw, name, int64(len(data)), bytes.NewReader(data)); err != nil {
			return err
		}
	}
	for _, name := range []string{"blobs/", "blobs/sha256/"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatPAX}); err != nil {
			return err
		}
	}
	digests = digests[:0]
	for digest := range blobs {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	for _, digest := range digests {
		if err := writeBlob(st, tw, blobs[digest]); err != nil {
			return err
		}
	}
	return tw.Close()
}

func readStoreJSON(st *store.Store, digest string) ([]byte, error) {
	info, err := os.Stat(st.BlobPath(oci.DigestHex(digest)))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxJSONSize {
		return nil, fmt.Errorf("JSON blob %s must be a regular file no larger than 16 MiB", digest)
	}
	return st.ReadBlob(digest)
}

func writeMember(tw *tar.Writer, name string, size int64, r io.Reader) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: size, Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err := io.Copy(tw, r)
	return err
}

func writeBlob(st *store.Store, tw *tar.Writer, desc oci.Descriptor) error {
	f, err := os.Open(st.BlobPath(oci.DigestHex(desc.Digest)))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if err := writeMember(tw, "blobs/sha256/"+oci.DigestHex(desc.Digest), desc.Size, io.TeeReader(f, h)); err != nil {
		return err
	}
	if fmt.Sprintf("sha256:%x", h.Sum(nil)) != desc.Digest {
		return fmt.Errorf("%w: %s", store.ErrCorruptBlob, desc.Digest)
	}
	return nil
}

func missingBlob(ref store.Ref, digest string) error {
	repository := ""
	if origin, err := registry.ParseReference(ref.Source); err == nil {
		repository = origin.Host + "/" + origin.Repository
	} else if len(ref.Repositories) > 0 {
		repository = ref.Repositories[0]
	}
	guidance := "fully pull the exact repository@digest before saving"
	if repository != "" {
		guidance = fmt.Sprintf("fully pull the exact image with `quasidock pull %s@%s` before saving", repository, ref.Manifest)
	}
	return fmt.Errorf("%w: %s (image %s may still be lazy); %s", store.ErrMissingBlob, digest, ref.Manifest, guidance)
}
