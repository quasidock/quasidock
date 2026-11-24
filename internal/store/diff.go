package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/quasidock/quasidock/internal/win32"
)

type DiffEntry struct {
	Kind byte
	Path string
}

// ContainerDiff compares private data with the pinned image's initial seed.
// Paths are slash-prefixed and relative to data; mounted storage is excluded.
// A live container may keep writing while its data is enumerated.
func (s *Store) ContainerDiff(target string) ([]DiffEntry, error) {
	lock, err := s.Lock(false)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	c, err := s.ResolveContainer(target)
	if err != nil {
		return nil, err
	}
	op, err := s.LockContainer(c.ID)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	c, err = s.ReadContainer(c.ID)
	if err != nil {
		return nil, err
	}

	excluded, err := DataExclusions(c)
	if err != nil {
		return nil, err
	}
	seed, err := s.DataSeed(c.Image)
	if err != nil {
		return nil, err
	}
	if seed != "" {
		base, err := dataFingerprints(context.Background(), seed, excluded)
		if err != nil {
			return nil, err
		}
		current, err := dataFingerprints(context.Background(), c.DataDir, excluded)
		if err != nil {
			return nil, err
		}
		changes := []DiffEntry{}
		for key, entry := range current {
			previous, exists := base[key]
			if !exists {
				changes = append(changes, DiffEntry{'A', entry.path})
			} else if previous.kind != entry.kind || previous.hash != entry.hash {
				changes = append(changes, DiffEntry{'C', entry.path})
			}
		}
		for key, entry := range base {
			if _, exists := current[key]; !exists {
				changes = append(changes, DiffEntry{'D', entry.path})
			}
		}
		sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
		return changes, nil
	}
	paths := []string{}
	if !excluded[""] {
		if err := walkContainerMetadata(s.ContainerDataDir(c.ID), "", excluded, func(rel string, _ os.FileInfo) error {
			paths = append(paths, "/"+rel)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("diff container %s: %w", c.BareName(), err)
		}
	}
	sort.Strings(paths)
	changes := make([]DiffEntry, len(paths))
	for i, path := range paths {
		changes[i] = DiffEntry{'A', path}
	}
	return changes, nil
}

func diffReparse(info os.FileInfo) bool {
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// walkContainerMetadata enumerates private data without opening file contents
// or following reparse points. Disappearing descendants are skipped.
func walkContainerMetadata(path, rel string, excluded map[string]bool, visit func(string, os.FileInfo) error) error {
	// Open the directory itself, never a reparse target. Keeping ancestor
	// handles open without delete sharing prevents replacement during descent.
	var h win32.HANDLE
	var err error
	for attempt := 0; ; attempt++ {
		h, err = win32.CreateFile(path, win32.FILE_LIST_DIRECTORY|win32.FILE_READ_ATTRIBUTES,
			win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE, nil, win32.OPEN_EXISTING,
			win32.FILE_FLAG_BACKUP_SEMANTICS|win32.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		transient := errors.Is(err, syscall.Errno(win32.ERROR_SHARING_VIOLATION)) ||
			errors.Is(err, syscall.Errno(win32.ERROR_ACCESS_DENIED))
		if !transient || attempt == 5 {
			break
		}
		// RemoveDirectory can leave a short-lived sharing violation or
		// delete-pending access denial. Wait before checking for disappearance.
		time.Sleep(time.Millisecond << attempt)
	}
	if err != nil {
		if rel != "" && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("opening %s: %w", path, err)
	}
	dir := os.NewFile(uintptr(h), path)
	defer func() { _ = dir.Close() }()
	info, err := dir.Stat()
	if err != nil {
		return err
	}
	if rel != "" {
		if err := visit(rel, info); err != nil {
			return err
		}
	}
	if diffReparse(info) {
		if rel == "" {
			return fmt.Errorf("data root is a reparse point: %s", path)
		}
		return nil
	}
	if !info.IsDir() {
		if rel == "" {
			return fmt.Errorf("data root is not a directory: %s", path)
		}
		return nil // A directory became a file during the scan.
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if rel != "" {
			name = rel + "/" + name
		}
		if excluded[strings.ToLower(name)] {
			continue
		}
		child := filepath.Join(path, entry.Name())
		// Windows directory enumeration already includes these attributes.
		// Reopening a child can fail with access denied while it is deleted.
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.IsDir() && !diffReparse(info) {
			if err := walkContainerMetadata(child, name, excluded, visit); err != nil {
				return err
			}
		} else {
			if err := visit(name, info); err != nil {
				return err
			}
		}
	}
	return nil
}
