package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
)

type Volume struct {
	Name       string `json:"Name"`
	Driver     string `json:"Driver"`
	Mountpoint string `json:"Mountpoint"`
	CreatedAt  string `json:"CreatedAt"`
	Anonymous  bool   `json:"Anonymous,omitempty"`
}

func (s *Store) volumeDir(name string) string { return s.Path("volumes", strings.ToLower(name)) }

func (s *Store) readVolumeNoLock(name string) (*Volume, error) {
	if err := mounts.ValidateName(name); err != nil {
		return nil, err
	}
	if err := winLocalVolumeRoot(s); err != nil {
		return nil, err
	}
	dir := s.volumeDir(name)
	if linked, err := mounts.IsReparse(dir); err == nil && linked {
		return nil, fmt.Errorf("volume directory is a reparse point: %s", dir)
	}
	b, err := os.ReadFile(filepath.Join(dir, "volume.json"))
	if err != nil {
		return nil, fmt.Errorf("reading volume %q: %w", name, err)
	}
	var v Volume
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if !strings.EqualFold(v.Name, name) || v.Driver != "local" {
		return nil, fmt.Errorf("invalid metadata for volume %q", name)
	}
	v.Mountpoint = filepath.Join(s.FullRoot(), "volumes", strings.ToLower(name), "data")
	return &v, nil
}

func (s *Store) createVolumeNoLock(name string, anonymous bool) (*Volume, error) {
	if err := mounts.ValidateName(name); err != nil {
		return nil, err
	}
	if v, err := s.readVolumeNoLock(name); err == nil {
		return v, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dir := s.volumeDir(name)
	if err := winLocalVolumeRoot(s); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return nil, err
	}
	if linked, err := mounts.IsReparse(filepath.Join(dir, "data")); err != nil || linked {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("volume data directory is a reparse point: %s", dir)
	}
	v := Volume{Name: name, Driver: "local", Mountpoint: filepath.Join(s.FullRoot(), "volumes", strings.ToLower(name), "data"), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Anonymous: anonymous}
	b, _ := json.MarshalIndent(v, "", "  ")
	tmp := s.TmpPath("volume")
	defer func() { _ = os.Remove(tmp) }()
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return nil, err
	}
	if err := RenameRetrying(tmp, filepath.Join(dir, "volume.json"), 5000); err != nil {
		return nil, err
	}
	return &v, nil
}

func winLocalVolumeRoot(s *Store) error {
	p, err := mounts.CanonicalDir(s.Path("volumes"))
	if err != nil {
		return err
	}
	if !strings.EqualFold(p, filepath.Join(s.FullRoot(), "volumes")) {
		return fmt.Errorf("volume root cannot be a reparse point: %s", s.Path("volumes"))
	}
	return nil
}

func (s *Store) CreateVolume(name string) (*Volume, error) {
	l, err := s.containersLock(true)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	return s.createVolumeNoLock(name, false)
}

func (s *Store) ReadVolume(name string) (*Volume, error) {
	l, err := s.containersLock(false)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	return s.readVolumeNoLock(name)
}

func (s *Store) listVolumesNoLock() ([]Volume, error) {
	entries, err := os.ReadDir(s.Path("volumes"))
	if err != nil {
		return nil, err
	}
	volumes := []Volume{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v, err := s.readVolumeNoLock(e.Name())
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, *v)
	}
	sort.Slice(volumes, func(i, j int) bool { return strings.ToLower(volumes[i].Name) < strings.ToLower(volumes[j].Name) })
	return volumes, nil
}

func (s *Store) ListVolumes() ([]Volume, error) {
	l, err := s.containersLock(false)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	return s.listVolumesNoLock()
}

// Raw reads never reconcile containers: volume dry runs must be non-mutating,
// and even stale retained container records pin their volumes.
func (s *Store) rawContainersNoLock() ([]ContainerRecord, error) {
	entries, err := os.ReadDir(s.Path("containers"))
	if err != nil {
		return nil, err
	}
	var all []ContainerRecord
	for _, e := range entries {
		if !e.IsDir() || !oci.IsSha256Hex(e.Name()) {
			continue
		}
		c, err := s.readRawContainer(e.Name())
		if err != nil {
			return nil, err
		}
		if c.ID != e.Name() {
			return nil, fmt.Errorf("container ID %q does not match its directory %q", c.ID, e.Name())
		}
		all = append(all, *c)
	}
	return all, nil
}

func volumeUser(all []ContainerRecord, name string) *ContainerRecord {
	for i := range all {
		for _, m := range all[i].HostConfig.Mounts {
			if m.Type == "volume" && strings.EqualFold(m.Source, name) {
				return &all[i]
			}
		}
	}
	return nil
}

func (s *Store) RemoveVolume(name string, guards ...func(string) (bool, error)) error {
	l, err := s.containersLock(true)
	if err != nil {
		return err
	}
	defer l.Release()
	v, err := s.readVolumeNoLock(name)
	if err != nil {
		return err
	}
	all, err := s.rawContainersNoLock()
	if err != nil {
		return err
	}
	if c := volumeUser(all, name); c != nil {
		return fmt.Errorf("volume %q is referenced by container %s (%s); remove the container first", name, c.ShortID(), c.BareName())
	}
	for _, check := range guards {
		protected, err := checkVolumePath(v, check)
		if err != nil {
			return err
		}
		if protected {
			return fmt.Errorf("volume %q is used by a running container", name)
		}
	}
	return s.deleteVolumeNoLock(v)
}

func checkVolumePath(v *Volume, guard func(string) (bool, error)) (bool, error) {
	if _, err := os.Lstat(v.Mountpoint); errors.Is(err, os.ErrNotExist) {
		// A previous deletion may have removed the data before the metadata.
		return false, nil
	}
	return guard(v.Mountpoint)
}

func (s *Store) deleteVolumeNoLock(v *Volume) error {
	dir := s.volumeDir(v.Name)
	if linked, err := mounts.IsReparse(dir); err != nil || linked {
		if err != nil {
			return err
		}
		return fmt.Errorf("volume directory is a reparse point: %s", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	// Keep the volume addressable if any of its contents cannot be removed.
	for _, entry := range entries {
		if !strings.EqualFold(entry.Name(), "volume.json") {
			if err := mounts.RemoveTree(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	trash := s.TmpPath("trash-volume")
	if err := RenameRetrying(dir, trash, 2000); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(trash, "volume.json")); err != nil {
		return errors.Join(err, RenameRetrying(trash, dir, 2000))
	}
	_ = os.Remove(trash)
	return nil
}

// removeAnonymousVolumesNoLock runs after deleting the owning container, with
// both the container index and the mount-source guard held.
func (s *Store) removeAnonymousVolumesNoLock(rec *ContainerRecord, guard func(string) (bool, error)) error {
	all, err := s.rawContainersNoLock()
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	var errs []error
	for _, spec := range rec.HostConfig.Mounts {
		name := strings.ToLower(spec.Source)
		if spec.Type != "volume" || !spec.Anonymous || seen[name] || volumeUser(all, name) != nil {
			continue
		}
		seen[name] = true
		v, err := s.readVolumeNoLock(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil && v.Anonymous {
			var protected bool
			protected, err = checkVolumePath(v, guard)
			if err == nil && !protected {
				err = s.deleteVolumeNoLock(v)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("volume %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Store) PruneVolumes(dry bool, guards ...func(string) (bool, error)) ([]Volume, uint64, error) {
	return s.pruneVolumes(dry, nil, nil, guards...)
}

func (s *Store) PruneSystemVolumes(dry bool, until []time.Time, excluded map[string]bool, guard func(string) (bool, error)) ([]Volume, uint64, error) {
	return s.pruneVolumes(dry, until, excluded, guard)
}

func (s *Store) pruneVolumes(dry bool, until []time.Time, excluded map[string]bool, guards ...func(string) (bool, error)) ([]Volume, uint64, error) {
	l, err := s.containersLock(true)
	if err != nil {
		return nil, 0, err
	}
	defer l.Release()
	all, err := s.rawContainersNoLock()
	if err != nil {
		return nil, 0, err
	}
	remaining := all[:0]
	for _, c := range all {
		if !excluded[c.ID] {
			remaining = append(remaining, c)
		}
	}
	volumes, err := s.listVolumesNoLock()
	if err != nil {
		return nil, 0, err
	}
	removed := []Volume{}
	var size uint64
volumeLoop:
	for _, v := range volumes {
		if !pruneTimestampBefore(v.CreatedAt, until) || volumeUser(remaining, v.Name) != nil {
			continue
		}
		for _, check := range guards {
			protected, err := checkVolumePath(&v, check)
			if err != nil {
				return removed, size, err
			}
			if protected {
				continue volumeLoop
			}
		}
		bytes := mounts.TreeSize(s.volumeDir(v.Name))
		if !dry {
			if err := s.deleteVolumeNoLock(&v); err != nil {
				return removed, size, err
			}
		}
		size += bytes
		removed = append(removed, v)
	}
	return removed, size, nil
}

func (s *Store) resolveContainerMountsNoLock(rec *ContainerRecord, created *[]string) error {
	rec.HostConfig.Mounts = slices.Clone(rec.HostConfig.Mounts)
	for i := range rec.HostConfig.Mounts {
		target, err := mounts.NormalizeTarget(rec.HostConfig.Mounts[i].Target)
		if err != nil {
			return err
		}
		rec.HostConfig.Mounts[i].Target = target
	}
	if err := mounts.ValidateTargets(rec.HostConfig.Mounts); err != nil {
		return err
	}
	for i, spec := range rec.HostConfig.Mounts {
		m := mounts.Mount{Type: spec.Type, Destination: mounts.Destination(rec.DataDir, spec.Target), RW: true}
		switch spec.Type {
		case "volume":
			if spec.Anonymous {
				if spec.Source != "" {
					return fmt.Errorf("anonymous mount at %s cannot specify a source", spec.Target)
				}
				if err := winLocalVolumeRoot(s); err != nil {
					return err
				}
				for {
					name, err := generateContainerID()
					if err != nil {
						return err
					}
					if _, err := os.Lstat(s.volumeDir(name)); errors.Is(err, os.ErrNotExist) {
						spec.Source = name
						break
					} else if err != nil {
						return err
					}
				}
				*created = append(*created, spec.Source)
			}
			v, err := s.createVolumeNoLock(spec.Source, spec.Anonymous)
			if err != nil {
				return err
			}
			m.Name = v.Name
			m.Source = v.Mountpoint
			rec.HostConfig.Mounts[i].Source = v.Name
			p, err := mounts.CanonicalDir(m.Source)
			if err != nil {
				return err
			}
			if !strings.EqualFold(p, m.Source) {
				return fmt.Errorf("volume data directory cannot be a reparse point: %s", m.Source)
			}
		case "bind":
			p, err := mounts.CanonicalDir(spec.Source)
			if err != nil {
				return err
			}
			m.Source = p
			rec.HostConfig.Mounts[i].Source = p
		default:
			return fmt.Errorf("unsupported mount type %q", spec.Type)
		}
		rec.Mounts = append(rec.Mounts, m)
	}
	return nil
}
