package store

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/quasidock/quasidock/internal/oci"
)

// ImageLabelFilter matches a label's presence, or its exact value.
type ImageLabelFilter struct {
	Key, Value string
	HasValue   bool
}

func (f ImageLabelFilter) matches(labels map[string]string) bool {
	value, exists := labels[f.Key]
	return exists && (!f.HasValue || value == f.Value)
}

// ImagePruneOptions selects catalog images. Repeated values within each filter
// key are ORed; different keys are ANDed. Until tests local cache age.
type ImagePruneOptions struct {
	All               bool
	Until             []time.Time
	Labels, NotLabels []ImageLabelFilter
	// ExcludedContainers is used when previewing a combined container and image prune.
	ExcludedContainers map[string]bool
}

// PruneBefore reports whether the age matches any cutoff. Zero denotes an
// unknown age, which only matches an unfiltered prune.
func PruneBefore(age time.Time, until []time.Time) bool {
	if len(until) == 0 {
		return true
	}
	if age.IsZero() {
		return false
	}
	return beforeAnyCutoff(age, until)
}

func beforeAnyCutoff(age time.Time, until []time.Time) bool {
	for _, cutoff := range until {
		if age.Before(cutoff) {
			return true
		}
	}
	return false
}

func pruneTimestampBefore(raw string, until []time.Time) bool {
	if len(until) == 0 {
		return true
	}
	age, err := time.Parse(time.RFC3339Nano, raw)
	return err == nil && beforeAnyCutoff(age, until)
}

func (opts ImagePruneOptions) matchesAge(ref Ref) bool {
	return pruneTimestampBefore(ref.CachedAt, opts.Until)
}

func (opts ImagePruneOptions) matchesLabels(labels map[string]string) bool {
	for _, group := range []struct {
		filters []ImageLabelFilter
		negate  bool
	}{{opts.Labels, false}, {opts.NotLabels, true}} {
		matched := len(group.filters) == 0
		for _, filter := range group.filters {
			matched = matched || filter.matches(labels) != group.negate
		}
		if !matched {
			return false
		}
	}
	return true
}

// ImagePrunePlan freezes a selection for preview or atomic catalog removal.
// Callers hold an exclusive store operation lock through planning and applying.
// Containers are raw records: planning never reconciles or deletes them.
type ImagePrunePlan struct {
	Images     []Ref
	Names      []string
	Retained   []Ref
	Containers []ContainerRecord
	catalog    *ImageIndex
	removed    map[string]bool
}

func (s *Store) imagePruneContainers() ([]ContainerRecord, error) {
	lock, err := s.containersLock(false)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	return s.rawContainersNoLock()
}

func (s *Store) imageLeased(digest string) bool {
	return IsLocked(s.Path("images", "use-"+oci.DigestHex(digest)+".lock"))
}

func (s *Store) PlanImagePrune(opts ImagePruneOptions) (*ImagePrunePlan, error) {
	idx, err := s.ImageCatalog()
	if err != nil {
		return nil, err
	}
	containers, err := s.imagePruneContainers()
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	remaining := containers[:0]
	for _, c := range containers {
		if !opts.ExcludedContainers[c.ID] {
			remaining = append(remaining, c)
			kept[c.Image] = true
		}
	}
	if !opts.All {
		for _, digest := range idx.Names {
			kept[digest] = true
		}
		for _, digest := range idx.Defaults {
			kept[digest] = true
		}
	}
	digests := make([]string, 0, len(idx.Images))
	for digest := range idx.Images {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	plan := &ImagePrunePlan{catalog: idx, Containers: remaining, removed: map[string]bool{}}
	for _, digest := range digests {
		ref := idx.Images[digest]
		selected := !kept[digest] && opts.matchesAge(ref) && !s.imageLeased(digest)
		if selected && (len(opts.Labels) != 0 || len(opts.NotLabels) != 0) {
			img, err := s.ReadImage(ref.ID, digest)
			if err != nil {
				return nil, err
			}
			selected = opts.matchesLabels(img.Config.Labels)
		}
		if selected {
			plan.Images = append(plan.Images, ref)
			plan.removed[digest] = true
		} else {
			plan.Retained = append(plan.Retained, ref)
		}
	}
	for name, digest := range idx.Names {
		if plan.removed[digest] {
			plan.Names = append(plan.Names, name)
		}
	}
	sort.Strings(plan.Names)
	return plan, nil
}

// ApplyImagePrune only updates catalog metadata; resource deletion follows the
// successful commit. A changed catalog or new pin invalidates the entire plan.
func (s *Store) ApplyImagePrune(plan *ImagePrunePlan) error {
	if len(plan.Images) == 0 {
		return nil
	}
	return s.changeImages(func(idx *ImageIndex) error {
		if !reflect.DeepEqual(idx, plan.catalog) {
			return errors.New("image catalog changed since prune was planned")
		}
		containers, err := s.imagePruneContainers()
		if err != nil {
			return err
		}
		for _, c := range containers {
			if plan.removed[c.Image] {
				return fmt.Errorf("image %s is now in use by container %s", c.Image, c.ShortID())
			}
		}
		for _, ref := range plan.Images {
			if s.imageLeased(ref.Manifest) {
				return fmt.Errorf("image %s is now in use", ref.Manifest)
			}
			delete(idx.Images, ref.Manifest)
		}
		for name, digest := range idx.Names {
			if plan.removed[digest] {
				delete(idx.Names, name)
			}
		}
		for id, digest := range idx.Defaults {
			if plan.removed[digest] {
				delete(idx.Defaults, id)
			}
		}
		return nil
	})
}
