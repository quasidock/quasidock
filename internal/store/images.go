package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
)

const DefaultImageRegistry = "ghcr.io/quasidock/quasidock"

var ErrImageNotFound = errors.New("image is not cached")

// ImageIndex is the local catalog. Names and Defaults are independent: moving a
// tag must not discard its previous manifest, or change an app's running image.
type ImageIndex struct {
	Version  int               `json:"version"`
	Images   map[string]Ref    `json:"images"`
	Names    map[string]string `json:"names"`
	Defaults map[string]string `json:"defaults"`
}

func newImageIndex() *ImageIndex {
	return &ImageIndex{Version: 1, Images: map[string]Ref{}, Names: map[string]string{}, Defaults: map[string]string{}}
}

// ImageName expands Quasidock's short names and makes the implicit latest tag
// explicit. It deliberately does not apply Docker Hub's namespace rules.
func ImageName(envRegistry, text string) (string, error) {
	if !strings.ContainsRune(text, '/') {
		end := strings.IndexAny(text, ":@")
		if end < 0 {
			end = len(text)
		}
		if !oci.IsValidID(text[:end]) {
			return "", fmt.Errorf("invalid image reference %q", text)
		}
		if envRegistry == "" {
			envRegistry = DefaultImageRegistry
		}
		text = strings.TrimRight(envRegistry, "/") + "/" + strings.ToLower(text[:end]) + text[end:]
	}
	ref, err := registry.ParseReference(text)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %w", text, err)
	}
	if ref.Tag == "" && ref.Digest == "" {
		ref.Tag = "latest"
	}
	return ref.String(), nil
}

func (s *Store) readImageIndex() (*ImageIndex, error) {
	data, err := os.ReadFile(s.Path("images", "index.json"))
	if err != nil {
		return nil, err
	}
	var idx ImageIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("reading image catalog: %w", err)
	}
	if idx.Version != 1 || idx.Images == nil || idx.Names == nil || idx.Defaults == nil {
		return nil, errors.New("unsupported or incomplete image catalog")
	}
	for digest, ref := range idx.Images {
		if oci.DigestHex(digest) == "" || ref.Manifest != digest || !oci.IsValidID(ref.ID) {
			return nil, fmt.Errorf("invalid image catalog record %q", digest)
		}
	}
	for name, digest := range idx.Names {
		if _, err := registry.ParseReference(name); err != nil {
			return nil, fmt.Errorf("image catalog has an invalid name %q", name)
		}
		if _, ok := idx.Images[digest]; !ok {
			return nil, errors.New("image catalog has a dangling name")
		}
	}
	for id, digest := range idx.Defaults {
		if ref, ok := idx.Images[digest]; !ok || !strings.EqualFold(ref.ID, id) {
			return nil, errors.New("image catalog has an invalid app default")
		}
	}
	return &idx, nil
}

func (s *Store) writeImageIndex(idx *ImageIndex) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.TmpPath("image-index")
	defer func() { _ = os.Remove(tmp) }()
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path("images", "index.json"))
}

// initializeImages imports the legacy app refs once. The catalog is the sole
// authority after this commit; retained legacy files cannot resurrect removals.
func (s *Store) initializeImages() error {
	if Exists(s.Path("images", "index.json")) {
		return nil
	}
	lock, err := s.Lock(true)
	if err != nil {
		return err
	}
	defer lock.Release()
	if Exists(s.Path("images", "index.json")) {
		return nil
	}
	refs, err := s.legacyRefs()
	if err != nil {
		return err
	}
	idx := newImageIndex()
	for _, ref := range refs {
		if IsLocked(s.RunsLockPath(ref.ID)) {
			return fmt.Errorf("finish existing runs of %s before migrating this store", ref.ID)
		}
		img, err := s.ReadImage(ref.ID, ref.Manifest)
		if err != nil {
			return fmt.Errorf("migrating %s: %w", ref.ID, err)
		}
		if !strings.EqualFold(img.Config.ID, ref.ID) || img.Config.Version != ref.Version {
			return fmt.Errorf("migrating %s: legacy metadata disagrees with manifest", ref.ID)
		}
		name, err := ImageName(os.Getenv("QUASIDOCK_REGISTRY"), ref.Source)
		if err != nil {
			name, err = ImageName(os.Getenv("QUASIDOCK_REGISTRY"), ref.ID+":"+ref.Version)
		}
		if err != nil {
			return err
		}
		idx.Images[ref.Manifest] = ref
		rememberRepository(idx, ref.Manifest, name)
		idx.Names[name] = ref.Manifest
		idx.Defaults[strings.ToLower(ref.ID)] = ref.Manifest
	}
	return s.writeImageIndex(idx)
}

// ImageCatalog returns a consistent snapshot. Lock order throughout the store is
// operation lock, catalog lock, app guard. No caller upgrades an operation lock.
func (s *Store) ImageCatalog() (*ImageIndex, error) {
	lock, err := OpenLock(s.Path("images", "index.lock"), false, true)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	return s.readImageIndex()
}

func (s *Store) changeImages(change func(*ImageIndex) error) error {
	lock, err := OpenLock(s.Path("images", "index.lock"), true, true)
	if err != nil {
		return err
	}
	defer lock.Release()
	idx, err := s.readImageIndex()
	if err != nil {
		return err
	}
	if err := change(idx); err != nil {
		return err
	}
	return s.writeImageIndex(idx)
}

func (idx *ImageIndex) Resolve(envRegistry, text string) (*Ref, error) {
	digest := ""
	if oci.DigestHex(text) != "" {
		digest = text
	} else if oci.IsValidID(text) {
		digest = idx.Defaults[strings.ToLower(text)]
	}
	if digest == "" {
		name, err := ImageName(envRegistry, text)
		if err != nil {
			return nil, err
		}
		digest = idx.Names[name]
		// A repository-qualified digest selects its exact known manifest even
		// when it was originally cached through another tag of that repository.
		if digest == "" {
			ref, _ := registry.ParseReference(name)
			if ref.Digest != "" {
				if cached, ok := idx.Images[ref.Digest]; ok {
					origin, _ := registry.ParseReference(cached.Source)
					if origin.Host == ref.Host && origin.Repository == ref.Repository {
						digest = ref.Digest
					}
					for _, repository := range cached.Repositories {
						if repository == ref.Host+"/"+ref.Repository {
							digest = ref.Digest
							break
						}
					}
				}
				for n, d := range idx.Names {
					r, _ := registry.ParseReference(n)
					if d == ref.Digest && r.Host == ref.Host && r.Repository == ref.Repository {
						digest = d
						break
					}
				}
			}
		}
	}
	ref, ok := idx.Images[digest]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrImageNotFound, text)
	}
	return &ref, nil
}

func (s *Store) ResolveLocalImage(envRegistry, text string) (*Ref, error) {
	idx, err := s.ImageCatalog()
	if err != nil {
		return nil, err
	}
	return idx.Resolve(envRegistry, text)
}

func (s *Store) RegisterImage(ref Ref, name string, makeDefault bool) error {
	if !oci.IsValidID(ref.ID) || oci.DigestHex(ref.Manifest) == "" {
		return errors.New("invalid image record")
	}
	if _, err := registry.ParseReference(name); name != "" && err != nil {
		return err
	}
	return s.changeImages(func(idx *ImageIndex) error {
		if previous, ok := idx.Images[ref.Manifest]; ok {
			ref.Repositories = previous.Repositories
		}
		idx.Images[ref.Manifest] = ref
		if name != "" {
			rememberRepository(idx, ref.Manifest, name)
			idx.Names[name] = ref.Manifest
		}
		if makeDefault {
			idx.Defaults[strings.ToLower(ref.ID)] = ref.Manifest
		}
		return nil
	})
}

func (s *Store) TagImage(envRegistry, source, target string) error {
	if oci.DigestHex(target) != "" {
		return errors.New("tag target must be a name and tag, not a local manifest ID")
	}
	name, err := ImageName(envRegistry, target)
	if err != nil {
		return err
	}
	r, _ := registry.ParseReference(name)
	if r.Digest != "" {
		return errors.New("tag target must be a name and tag, not a digest")
	}
	return s.changeImages(func(idx *ImageIndex) error {
		ref, err := idx.Resolve(envRegistry, source)
		if err != nil {
			return err
		}
		idx.Names[name] = ref.Manifest
		rememberRepository(idx, ref.Manifest, name)
		return nil
	})
}

// Keep repository provenance when a mutable tag moves away from this manifest.
// This also permits offline repository@digest lookups of locally built images.
func rememberRepository(idx *ImageIndex, digest, name string) {
	r, err := registry.ParseReference(name)
	if err != nil {
		return
	}
	repository := r.Host + "/" + r.Repository
	ref := idx.Images[digest]
	for _, existing := range ref.Repositories {
		if existing == repository {
			return
		}
	}
	ref.Repositories = append(ref.Repositories, repository)
	sort.Strings(ref.Repositories)
	idx.Images[digest] = ref
}

func (s *Store) ListImages() ([]Ref, error) {
	idx, err := s.ImageCatalog()
	if err != nil {
		return nil, err
	}
	refs := make([]Ref, 0, len(idx.Images))
	for _, ref := range idx.Images {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Manifest < refs[j].Manifest })
	return refs, nil
}

// RemoveImage removes a name, or all names when given a local ID/app shortcut.
// The caller holds the exclusive operation lock, excluding launches and pulls.
func (s *Store) RemoveImage(envRegistry, text string) (*Ref, bool, error) {
	var removed *Ref
	lastApp := false
	err := s.changeImages(func(idx *ImageIndex) error {
		ref, err := idx.Resolve(envRegistry, text)
		if err != nil {
			return err
		}
		removed = ref
		if oci.DigestHex(text) != "" || oci.IsValidID(text) {
			if err := s.imageInUseError(*ref); err != nil {
				return err
			}
			for name, digest := range idx.Names {
				if digest == ref.Manifest {
					delete(idx.Names, name)
				}
			}
		} else {
			name, err := ImageName(envRegistry, text)
			if err != nil {
				return err
			}
			if _, ok := idx.Names[name]; !ok {
				return fmt.Errorf("%w: reference %s", ErrImageNotFound, text)
			}
			otherNames := false
			for n, digest := range idx.Names {
				if digest == ref.Manifest && n != name {
					otherNames = true
					break
				}
			}
			if !otherNames {
				if err := s.imageInUseError(*ref); err != nil {
					return err
				}
			}
			delete(idx.Names, name)
		}
		for _, digest := range idx.Names {
			if digest == ref.Manifest {
				return nil
			}
		}
		delete(idx.Images, ref.Manifest)
		for id, digest := range idx.Defaults {
			if digest == ref.Manifest {
				delete(idx.Defaults, id)
			}
		}
		lastApp = true
		for _, other := range idx.Images {
			if strings.EqualFold(other.ID, ref.ID) {
				lastApp = false
			}
		}
		return nil
	})
	return removed, lastApp, err
}

func (s *Store) imageInUseError(ref Ref) error {
	if IsLocked(s.Path("images", "use-"+oci.DigestHex(ref.Manifest)+".lock")) {
		if c, err := s.ContainerUsingImage(ref.Manifest); err == nil && c != nil {
			return fmt.Errorf("image %s is in use by container %s", ref.Manifest, c.ShortID())
		}
		return fmt.Errorf("image %s is in use", ref.Manifest)
	}
	if c, err := s.ContainerUsingImage(ref.Manifest); err == nil && c != nil {
		return fmt.Errorf("image %s is in use by container %s", ref.Manifest, c.ShortID())
	}
	return nil
}

func (s *Store) ImageInUse(ref Ref) bool {
	return s.imageInUseError(ref) != nil
}

// BeginImageRun acquires shared app and manifest leases. Multiple versions of
// the same app can run concurrently in separate containers with isolated data.
func (s *Store) BeginImageRun(ref Ref) (*Lock, *Lock, error) {
	app, err := OpenLock(s.RunsLockPath(ref.ID), false, true)
	if err != nil {
		return nil, nil, err
	}
	image, err := OpenLock(s.Path("images", "use-"+oci.DigestHex(ref.Manifest)+".lock"), false, true)
	if err != nil {
		app.Release()
		return nil, nil, err
	}
	return app, image, nil
}

// PruneImages collects only unnamed, nondefault manifests which are not running.
func (s *Store) PruneImages(dryRun bool) ([]Ref, error) {
	var removed []Ref
	collect := func(idx *ImageIndex) error {
		kept := map[string]bool{}
		for _, d := range idx.Names {
			kept[d] = true
		}
		for _, d := range idx.Defaults {
			kept[d] = true
		}
		for d, ref := range idx.Images {
			if !kept[d] && !s.ImageInUse(ref) {
				removed = append(removed, ref)
				if !dryRun {
					delete(idx.Images, d)
				}
			}
		}
		return nil
	}
	var err error
	if dryRun {
		var idx *ImageIndex
		idx, err = s.ImageCatalog()
		if err == nil {
			err = collect(idx)
		}
	} else {
		err = s.changeImages(collect)
	}
	return removed, err
}
