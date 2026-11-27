package store

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/quasidock/quasidock/internal/oci"
)

type DiskUsageTotals struct {
	Total, Active     int
	Size, Reclaimable uint64
}

func (t *DiskUsageTotals) add(size, reclaimable uint64, active bool) {
	t.Total++
	t.Size += size
	t.Reclaimable += reclaimable
	if active {
		t.Active++
	}
}

type ImageDiskUsage struct {
	ID         string
	Names      []string
	SharedSize uint64
	UniqueSize uint64
	Containers int
}

type ContainerDiskUsage struct {
	ID, Name, Image, Status string
	Size                    uint64
}

type VolumeDiskUsage struct {
	Name       string
	References int
	Size       uint64
}

type CacheDiskUsage struct {
	Area, Name string
	Size       uint64
}

type DiskUsage struct {
	ImageTotals, ContainerTotals, VolumeTotals, CacheTotals DiskUsageTotals
	Images                                                  []ImageDiskUsage
	Containers                                              []ContainerDiskUsage
	Volumes                                                 []VolumeDiskUsage
	Cache                                                   []CacheDiskUsage
}

// DiskUsage measures the inventory under the caller's exclusive store lock.
// Applications can still write files or exit while the scan runs.
func (s *Store) DiskUsage(inventory *SystemInventory) (*DiskUsage, error) {
	usage := &DiskUsage{}
	if err := s.imageDiskUsage(usage, inventory); err != nil {
		return nil, err
	}
	for _, c := range inventory.Containers {
		size, err := s.containerDiskUsage(c.Record)
		if err != nil {
			return nil, fmt.Errorf("size container %s: %w", c.Record.BareName(), err)
		}
		var reclaimable uint64
		if c.Reclaimable {
			reclaimable = size
		}
		usage.ContainerTotals.add(size, reclaimable, c.Active)
		usage.Containers = append(usage.Containers, ContainerDiskUsage{
			ID: c.Record.ID, Name: c.Record.BareName(), Image: c.Record.Image, Status: c.Status, Size: size,
		})
	}
	for _, v := range inventory.Volumes {
		size, err := diskTreeSize(s.volumeDir(v.Name), nil)
		if err != nil {
			return nil, fmt.Errorf("size volume %q: %w", v.Name, err)
		}
		var reclaimable uint64
		if !v.Active {
			reclaimable = size
		}
		usage.VolumeTotals.add(size, reclaimable, v.Active)
		usage.Volumes = append(usage.Volumes, VolumeDiskUsage{Name: v.Name, References: v.References, Size: size})
	}
	if err := s.cacheDiskUsage(usage); err != nil {
		return nil, err
	}
	slices.SortFunc(usage.Cache, func(a, b CacheDiskUsage) int {
		if order := strings.Compare(a.Area, b.Area); order != 0 {
			return order
		}
		return strings.Compare(a.Name, b.Name)
	})
	return usage, nil
}

func (s *Store) containerDiskUsage(c ContainerRecord) (uint64, error) {
	mounts, err := DataExclusions(&c)
	if err != nil {
		return 0, err
	}
	excluded := map[string]bool{"run.lock": true}
	for mount := range mounts {
		path := "data"
		if mount != "" {
			path += "/" + mount
		}
		excluded[path] = true
	}
	return diskTreeSize(s.ContainerDir(c.ID), excluded)
}

// Lstat and the metadata walker keep the scan from expanding junctions or
// opening file data in a lazily deployed tree.
func diskEntry(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) || err == nil && diffReparse(info) {
		return nil, nil
	}
	return info, err
}

func diskTreeSize(path string, excluded map[string]bool) (uint64, error) {
	info, err := diskEntry(path)
	if err != nil || info == nil {
		return 0, err
	}
	if info.Mode().IsRegular() {
		return uint64(info.Size()), nil
	}
	if !info.IsDir() {
		return 0, nil
	}
	var size uint64
	err = walkContainerMetadata(path, "", excluded, func(_ string, info os.FileInfo) error {
		if info.Mode().IsRegular() && !diffReparse(info) {
			size += uint64(info.Size())
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return size, err
}

func diskDirectory(path string) (bool, error) {
	info, err := diskEntry(path)
	if err != nil || info == nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("expected a directory: %s", path)
	}
	return true, nil
}

func (s *Store) cacheDiskUsage(usage *DiskUsage) error {
	if exists, err := diskDirectory(s.Path("cache")); !exists || err != nil {
		return err
	}
	for _, area := range []string{"builds", "downloads", "tools", "work"} {
		path := s.Path("cache", area)
		exists, err := diskDirectory(path)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if diffReparse(info) || !info.IsDir() && !info.Mode().IsRegular() {
				continue
			}
			size, err := diskTreeSize(s.Path("cache", area, entry.Name()), nil)
			if err != nil {
				return err
			}
			usage.CacheTotals.add(size, size, false)
			usage.Cache = append(usage.Cache, CacheDiskUsage{Area: area, Name: entry.Name(), Size: size})
		}
	}
	return nil
}

// An image can have both a local blob and on-disk deployment/streaming state
// for the same layer digest. Each is measured and attributed once.
type diskContent struct {
	size       uint64
	lazySize   uint64
	present    bool
	leased     bool
	retained   bool
	build      bool
	imageCount int
}

type diskContentIndex struct {
	store      *Store
	blobs      map[string]*diskContent
	layers     map[string]*diskContent
	manifests  map[string][]*diskContent
	haveBlobs  bool
	haveDeploy bool
	haveLazy   bool
}

func (s *Store) newDiskContentIndex() (*diskContentIndex, error) {
	index := &diskContentIndex{
		store: s, blobs: map[string]*diskContent{}, layers: map[string]*diskContent{}, manifests: map[string][]*diskContent{},
	}
	for _, dir := range []struct {
		path string
		out  *bool
	}{
		{s.Path("blobs"), &index.haveBlobs},
		{s.Path("deploy"), &index.haveDeploy},
		{s.Path("lazy"), &index.haveLazy},
	} {
		var err error
		*dir.out, err = diskDirectory(dir.path)
		if err != nil {
			return nil, err
		}
	}
	if index.haveBlobs {
		var err error
		index.haveBlobs, err = diskDirectory(s.Path("blobs", "sha256"))
		if err != nil {
			return nil, err
		}
	}
	return index, nil
}

func (index *diskContentIndex) blob(digest string, required bool) (*diskContent, error) {
	hex := oci.DigestHex(digest)
	if hex == "" {
		return nil, fmt.Errorf("%w: %s", ErrMalformedDig, digest)
	}
	part := index.blobs[digest]
	if part == nil {
		part = &diskContent{}
		if index.haveBlobs {
			path := index.store.BlobPath(hex)
			info, err := diskEntry(path)
			if err != nil {
				return nil, err
			}
			if info != nil {
				if !info.Mode().IsRegular() {
					return nil, fmt.Errorf("expected a regular blob file: %s", path)
				}
				part.size, part.present = uint64(info.Size()), true
			}
		}
		index.blobs[digest] = part
	}
	if required && !part.present {
		return nil, fmt.Errorf("%w: %s", ErrMissingBlob, digest)
	}
	return part, nil
}

func (index *diskContentIndex) layer(digest string) (*diskContent, error) {
	if part := index.layers[digest]; part != nil {
		return part, nil
	}
	part := &diskContent{}
	if index.haveDeploy {
		deploy, err := index.store.DeployPath(digest)
		if err != nil {
			return nil, err
		}
		part.size, err = diskTreeSize(deploy, nil)
		if err != nil {
			return nil, err
		}
		part.leased = IsLocked(LockPathFor(deploy))
	}
	if index.haveLazy {
		var err error
		part.lazySize, err = diskTreeSize(index.store.Path("lazy", oci.DigestHex(digest)), nil)
		if err != nil {
			return nil, err
		}
		part.size += part.lazySize
	}
	index.layers[digest] = part
	return part, nil
}

func (index *diskContentIndex) image(digest string) ([]*diskContent, error) {
	if parts := index.manifests[digest]; parts != nil {
		return parts, nil
	}
	manifest, err := index.blob(digest, true)
	if err != nil {
		return nil, err
	}
	m, err := index.store.ReadManifest(digest)
	if err != nil {
		return nil, err
	}
	if m.SchemaVersion != 2 || m.MediaType != "" && m.MediaType != oci.MediaTypeManifest || m.Config.Size < 0 {
		return nil, fmt.Errorf("invalid local manifest %s", digest)
	}
	config, err := index.blob(m.Config.Digest, true)
	if err != nil {
		return nil, err
	}
	parts := map[*diskContent]struct{}{manifest: {}, config: {}}
	for _, layer := range m.Layers {
		if layer.Size < 0 {
			return nil, fmt.Errorf("invalid layer size for %s", layer.Digest)
		}
		blob, err := index.blob(layer.Digest, false)
		if err != nil {
			return nil, err
		}
		deployed, err := index.layer(layer.Digest)
		if err != nil {
			return nil, fmt.Errorf("size layer %s: %w", layer.Digest, err)
		}
		blob.leased = deployed.leased
		parts[blob], parts[deployed] = struct{}{}, struct{}{}
	}
	index.manifests[digest] = slices.Collect(maps.Keys(parts))
	return index.manifests[digest], nil
}

func (s *Store) imageDiskUsage(usage *DiskUsage, inventory *SystemInventory) error {
	index, err := s.newDiskContentIndex()
	if err != nil {
		return err
	}
	containerRefs := make(map[string]int)
	for _, c := range inventory.Containers {
		if c.Record.Image != "" {
			containerRefs[c.Record.Image]++
		}
	}
	names := make(map[string][]string)
	for _, name := range slices.Sorted(maps.Keys(inventory.Images.Names)) {
		digest := inventory.Images.Names[name]
		names[digest] = append(names[digest], name)
	}
	active := make(map[string]bool)
	digests := slices.Sorted(maps.Keys(inventory.Images.Images))
	for _, digest := range digests {
		parts, err := index.image(digest)
		if err != nil {
			return fmt.Errorf("size image %s: %w", digest, err)
		}
		active[digest] = containerRefs[digest] > 0 || s.imageLeased(digest)
		for _, part := range parts {
			part.imageCount++
			part.retained = part.retained || active[digest]
		}
	}
	for _, digest := range slices.Sorted(maps.Keys(containerRefs)) {
		if _, catalogued := active[digest]; catalogued {
			continue
		}
		parts, err := index.image(digest)
		if err != nil {
			return fmt.Errorf("size container image %s: %w", digest, err)
		}
		for _, part := range parts {
			part.retained = true
		}
	}
	if err := index.buildDiskUsage(usage); err != nil {
		return err
	}
	for _, digest := range digests {
		row := ImageDiskUsage{ID: digest, Names: names[digest], Containers: containerRefs[digest]}
		for _, part := range index.manifests[digest] {
			if part.imageCount > 1 {
				row.SharedSize += part.size
			} else {
				row.UniqueSize += part.size
			}
		}
		usage.Images = append(usage.Images, row)
		usage.ImageTotals.add(0, 0, active[digest])
	}
	for _, parts := range []map[string]*diskContent{index.blobs, index.layers} {
		for _, part := range parts {
			if part.imageCount == 0 {
				continue
			}
			usage.ImageTotals.Size += part.size
			if !part.retained && !part.build && !part.leased {
				usage.ImageTotals.Reclaimable += part.size
			}
		}
	}
	return nil
}

func (index *diskContentIndex) buildDiskUsage(usage *DiskUsage) error {
	s := index.store
	for _, path := range []string{s.Path("cache"), s.Path("cache", "images")} {
		if exists, err := diskDirectory(path); !exists || err != nil {
			return err
		}
	}
	digests, err := s.ListBuildImages()
	if err != nil {
		return err
	}
	for _, digest := range digests {
		marker, err := diskEntry(s.Path("cache", "images", oci.DigestHex(digest)))
		if err != nil {
			return err
		}
		if marker == nil || !marker.Mode().IsRegular() {
			continue
		}
		parts, err := index.image(digest)
		if err != nil {
			return fmt.Errorf("size build image %s: %w", digest, err)
		}
		size := uint64(marker.Size())
		reclaimable := size
		for _, part := range parts {
			if !part.build && part.imageCount == 0 {
				size += part.size
				if !part.retained && !part.leased {
					// Prune checks lazy state before removing build dependency markers.
					reclaimable += part.size - part.lazySize
				}
			}
			part.build = true
		}
		usage.CacheTotals.add(size, reclaimable, false)
		usage.Cache = append(usage.Cache, CacheDiskUsage{Area: "images", Name: digest, Size: size})
	}
	return nil
}
