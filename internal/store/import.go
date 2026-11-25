package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
)

// ImportBlob is a staged, digest-verified blob. ImportImages consumes Path when
// the blob is not already stored, just as PutBlobFile does.
type ImportBlob struct {
	Path   string
	Digest string
	Size   int64
}

// ImageImport contains a fully validated, self-contained set of images.
type ImageImport struct {
	Images   map[string]Ref
	Names    map[string]string
	Defaults map[string]string
	Blobs    []ImportBlob
}

// ImportImages publishes an entire import with one catalog update. The caller
// holds a shared operation lock and has verified the staged image graph. Name
// conflicts are checked under the catalog lock, before any blobs are promoted.
// A failed catalog write can leave verified, unreferenced blobs for pruning.
func (s *Store) ImportImages(in ImageImport, force bool) error {
	if len(in.Images) == 0 {
		return errors.New("no images to import")
	}
	for digest, ref := range in.Images {
		if oci.DigestHex(digest) == "" || ref.Manifest != digest || !oci.IsValidID(ref.ID) {
			return errors.New("invalid imported image record")
		}
	}
	for name, digest := range in.Names {
		ref, err := registry.ParseReference(name)
		if err != nil || (ref.Tag == "" && ref.Digest == "") || (ref.Digest != "" && ref.Digest != digest) {
			return fmt.Errorf("invalid imported image name %q", name)
		}
		if _, ok := in.Images[digest]; !ok {
			return fmt.Errorf("imported name %q has no image", name)
		}
	}
	for id, digest := range in.Defaults {
		ref, ok := in.Images[digest]
		if !ok || id != strings.ToLower(ref.ID) {
			return fmt.Errorf("invalid imported default %q", id)
		}
	}
	for _, blob := range in.Blobs {
		if oci.DigestHex(blob.Digest) == "" || blob.Size < 0 {
			return errors.New("invalid imported blob")
		}
	}
	return s.changeImages(func(idx *ImageIndex) error {
		originalDefaults := maps.Clone(idx.Defaults)
		for digest, ref := range in.Images {
			if previous, ok := idx.Images[digest]; ok && (!strings.EqualFold(previous.ID, ref.ID) || previous.Version != ref.Version) {
				return fmt.Errorf("image catalog metadata disagrees with imported manifest %s", digest)
			}
		}
		names := make([]string, 0, len(in.Names))
		for name := range in.Names {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if previous, ok := idx.Names[name]; ok && previous != in.Names[name] && !force {
				return fmt.Errorf("image name %s already points to %s; use --force to replace it", name, previous)
			}
		}
		// Check every existing blob before promoting any staged files. Never
		// silently reuse a corrupt blob merely because its filename exists.
		for _, blob := range in.Blobs {
			err := verifyImportedBlob(s.BlobPath(oci.DigestHex(blob.Digest)), blob)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		for _, blob := range in.Blobs {
			if _, err := s.PutBlobFile(blob.Path, FileHash{Hex: oci.DigestHex(blob.Digest), Size: blob.Size}, ""); err != nil {
				return err
			}
		}
		digests := make([]string, 0, len(in.Images))
		for digest := range in.Images {
			digests = append(digests, digest)
		}
		sort.Strings(digests)
		for _, digest := range digests {
			ref := in.Images[digest]
			if previous, ok := idx.Images[digest]; ok {
				// The existing image's provenance remains authoritative locally.
				ref.Source = previous.Source
				ref.Repositories = append(slices.Clone(previous.Repositories), ref.Repositories...)
			}
			sort.Strings(ref.Repositories)
			ref.Repositories = slices.Compact(ref.Repositories)
			idx.Images[digest] = ref
			id := strings.ToLower(ref.ID)
			if idx.Defaults[id] == "" {
				// Sorted iteration supplies a stable fallback when the original
				// default was not included in the archive.
				idx.Defaults[id] = digest
			}
		}
		for _, name := range names {
			digest := in.Names[name]
			idx.Names[name] = digest
			rememberRepository(idx, digest, name)
		}
		for id, digest := range in.Defaults {
			if originalDefaults[id] == "" {
				idx.Defaults[id] = digest
			}
		}
		return nil
	})
}

func verifyImportedBlob(path string, blob ImportBlob) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != blob.Size {
		return fmt.Errorf("%w: %s (size or file type mismatch)", ErrCorruptBlob, blob.Digest)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != blob.Size || fmt.Sprintf("sha256:%x", h.Sum(nil)) != blob.Digest {
		return fmt.Errorf("%w: %s", ErrCorruptBlob, blob.Digest)
	}
	return nil
}
