package store

import (
	"fmt"
	"strings"
	"time"
)

type SystemContainer struct {
	Record      ContainerRecord
	Status      string
	Active      bool
	Reclaimable bool
}

type SystemVolume struct {
	Volume
	References int
	Active     bool
}

// SystemInventory holds the local catalog and storage records for reporting.
type SystemInventory struct {
	Images     *ImageIndex
	Containers []SystemContainer
	Volumes    []SystemVolume
}

// ReadSystemInventory reads records without reconciling them. The caller holds the
// store lock and, when checking mount protection, the mount source guard.
func (s *Store) ReadSystemInventory(protected func(string) (bool, error)) (*SystemInventory, error) {
	images, err := s.ImageCatalog()
	if err != nil {
		return nil, err
	}
	lock, err := s.containersLock(false)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	containers, err := s.rawContainersNoLock()
	if err != nil {
		return nil, err
	}
	volumes, err := s.listVolumesNoLock()
	if err != nil {
		return nil, err
	}

	inventory := &SystemInventory{Images: images}
	volumeReferences := make(map[string]int)
	now := time.Now()
	for _, c := range containers {
		live := s.IsContainerRunning(c.ID)
		inventory.Containers = append(inventory.Containers, SystemContainer{
			Record:      c,
			Status:      observedContainerStatus(c.State, live),
			Active:      live,
			Reclaimable: !live && !containerLaunchPending(c, now) && !IsLocked(s.ContainerOperationLockPath(c.ID)),
		})
		seen := make(map[string]bool)
		for _, m := range c.HostConfig.Mounts {
			name := strings.ToLower(m.Source)
			if m.Type == "volume" && !seen[name] {
				seen[name] = true
				volumeReferences[name]++
			}
		}
	}
	for _, v := range volumes {
		refs := volumeReferences[strings.ToLower(v.Name)]
		active := refs > 0
		if !active && protected != nil {
			active, err = protected(v.Mountpoint)
			if err != nil {
				return nil, fmt.Errorf("checking volume %q: %w", v.Name, err)
			}
		}
		inventory.Volumes = append(inventory.Volumes, SystemVolume{Volume: v, References: refs, Active: active})
	}
	return inventory, nil
}

func observedContainerStatus(state ContainerState, live bool) string {
	switch {
	case live && state.Restarting:
		return "restarting"
	case live && state.Paused:
		return "paused"
	case live && state.Running:
		return "running"
	case state.Status == "created" && (live || !state.Launched && !state.Running && !state.Restarting && !state.Paused):
		return "created"
	default:
		return "exited"
	}
}
