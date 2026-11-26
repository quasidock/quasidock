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
}

func (opts ImagePruneOptions) matchesAge(ref Ref) bool {
	if len(opts.Until) == 0 {
		return true
	}
	cached, err := time.Parse(time.RFC3339Nano, ref.CachedAt)
	if err != nil {
		return false
	}
	for _, until := range opts.Until {
		if cached.Before(until) {
			return true
		}
	}
	return false
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
	for _, c := range containers {
		kept[c.Image] = true
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
	plan := &ImagePrunePlan{catalog: idx, Containers: containers, removed: map[string]bool{}}
	for _, digest := range digests {
		ref := idx.Images[digest]
		selected := !kept[digest] && opts.matchesAge(ref) && !IsLocked(s.Path("images", "use-"+oci.DigestHex(digest)+".lock"))
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
			if IsLocked(s.Path("images", "use-"+oci.DigestHex(ref.Manifest)+".lock")) {
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
