package store

import (
	"fmt"
	"os"
	"time"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
)

type BuildCachePruneEntry struct {
	Area, Name, Manifest string
	Size                 uint64
}

// PlanBuildCachePrune reads cache entries under the exclusive store lock and
// mount guard. A surviving result marker pins its manifest just like an image
// marker, even when the image is absent from the catalog.
func (s *Store) PlanBuildCachePrune(until []time.Time, protected func(string) (bool, error)) ([]BuildCachePruneEntry, []string, error) {
	if exists, err := diskDirectory(s.Path("cache")); !exists || err != nil {
		return nil, nil, err
	}
	var selected []BuildCachePruneEntry
	var retained []string
	for _, area := range []string{"builds", "downloads", "images", "tools", "work"} {
		path := s.Path("cache", area)
		exists, err := diskDirectory(path)
		if err != nil {
			return nil, nil, err
		}
		if !exists {
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, nil, err
		}
		for _, entry := range entries {
			item := BuildCachePruneEntry{Area: area, Name: entry.Name()}
			path := s.Path("cache", area, item.Name)
			info, err := diskEntry(path)
			if err != nil {
				return nil, nil, err
			}
			if info == nil || !info.IsDir() && !info.Mode().IsRegular() {
				continue
			}
			item.Manifest, err = s.buildCacheManifest(item, info)
			if err != nil {
				return nil, nil, err
			}
			size, age, err := buildCacheEntrySizeAndAge(path, info)
			if err != nil {
				return nil, nil, err
			}
			item.Size = size
			if PruneBefore(age, until) && (item.Manifest == "" || !s.imageLeased(item.Manifest)) {
				inUse, err := protected(path)
				if err != nil {
					return nil, nil, fmt.Errorf("check build cache entry %s/%s: %w", area, item.Name, err)
				}
				if !inUse {
					selected = append(selected, item)
					continue
				}
			}
			if item.Manifest != "" {
				retained = append(retained, item.Manifest)
			}
		}
	}
	return selected, retained, nil
}

func (s *Store) buildCacheManifest(item BuildCachePruneEntry, info os.FileInfo) (string, error) {
	if !info.Mode().IsRegular() {
		return "", nil
	}
	var digest string
	switch {
	case item.Area == "images" && oci.IsSha256Hex(item.Name):
		digest = "sha256:" + item.Name
	case item.Area == "builds":
		var err error
		digest, err = s.ReadBuildCache(item.Name)
		if err != nil {
			return "", err
		}
	}
	if digest == "" {
		return "", nil
	}
	info, err := diskEntry(s.BlobPath(oci.DigestHex(digest)))
	if info == nil || err != nil {
		return "", err // A stale marker can be removed without collecting content.
	}
	return digest, nil
}

func buildCacheEntrySizeAndAge(path string, info os.FileInfo) (uint64, time.Time, error) {
	age := info.ModTime()
	if !info.IsDir() {
		return uint64(info.Size()), age, nil
	}
	var size uint64
	err := walkContainerMetadata(path, "", nil, func(_ string, info os.FileInfo) error {
		if diffReparse(info) {
			return nil
		}
		if changed := info.ModTime(); changed.After(age) {
			age = changed
		}
		if info.Mode().IsRegular() {
			size += uint64(info.Size())
		}
		return nil
	})
	return size, age, err
}

func (s *Store) RemoveBuildCacheEntry(entry BuildCachePruneEntry) error {
	return mounts.RemoveTree(s.Path("cache", entry.Area, entry.Name))
}
