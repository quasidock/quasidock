package store

import (
	"fmt"
	"os"
)

// ContainerSize sums regular-file lengths in private data, including copied
// image seed data. Mounts, reparse points, logs, and record metadata are excluded.
// Live applications may keep writing while the data is enumerated.
func (s *Store) ContainerSize(target string) (uint64, error) {
	lock, err := s.Lock(false)
	if err != nil {
		return 0, err
	}
	defer lock.Release()
	c, err := s.ResolveContainer(target)
	if err != nil {
		return 0, err
	}
	op, err := s.LockContainer(c.ID)
	if err != nil {
		return 0, err
	}
	defer op.Release()
	c, err = s.ReadContainer(c.ID)
	if err != nil {
		return 0, err
	}
	excluded, err := DataExclusions(c)
	if err != nil {
		return 0, err
	}
	if excluded[""] {
		return 0, nil
	}
	var size uint64
	err = walkContainerMetadata(s.ContainerDataDir(c.ID), "", excluded, func(_ string, info os.FileInfo) error {
		if info.Mode().IsRegular() && !diffReparse(info) {
			size += uint64(info.Size())
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("size container %s: %w", c.BareName(), err)
	}
	return size, nil
}
