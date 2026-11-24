package hkcu

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/win32"
)

// Snapshot copies only declared keys into a new private hive. Live keys are read
// only after matching their ownership record to the source container's hive.
// It never settles keys, changes ownership records, or copies host keys by name.
func Snapshot(keys []string, dataDir, dest string, running bool) error {
	if len(keys) == 0 {
		return nil
	}
	mutex, err := acquireMutex()
	if err != nil {
		return err
	}
	defer mutex.Release()
	sourceDir := filepath.Join(dataDir, "registry")
	if _, err := os.Stat(sourceDir); os.IsNotExist(err) {
		if running {
			return fmt.Errorf("container registry directory disappeared: %w", err)
		}
		return nil
	} else if err != nil {
		return err
	}
	// Keep the hive directory and database name from being replaced while the
	// registry API opens them. Never load a hive through an application junction.
	dirGuard, err := guardHivePath(sourceDir, true)
	if err != nil {
		return err
	}
	defer func() { _ = dirGuard.Close() }()
	fileGuard, err := guardHivePath(filepath.Join(sourceDir, "user.dat"), false)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if fileGuard != nil {
		defer func() { _ = fileGuard.Close() }()
	}
	sourceDir, err = mounts.CanonicalPath(sourceDir)
	if err != nil {
		return err
	}
	var source *Hive
	if !running && fileGuard != nil {
		source, err = openHive(sourceDir, false)
		if err != nil {
			return err
		}
	}
	if source != nil {
		defer source.Close()
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	to, err := openHive(dest, true)
	if err != nil {
		return err
	}
	defer to.Close()
	for _, key := range keys {
		r, err := readRecord(key)
		if err != nil {
			return err
		}
		owned := r != nil && samePath(r.HiveDir, sourceDir) && r.State == StateInPlace
		if running && !owned {
			return fmt.Errorf("cannot capture HKCU\\%s: container registry ownership changed", key)
		}
		root := win32.HKEY(0)
		if owned {
			root = hkcuRoot
		} else if source != nil {
			root = source.Root
		}
		if root == 0 {
			continue
		}
		from, err := regkey.Open(root, key, win32.KEY_READ)
		if err != nil {
			return err
		}
		if from == nil {
			continue
		}
		out, err := regkey.CreateWith(to.Root, key, win32.KEY_ALL_ACCESS)
		if err != nil {
			from.Close()
			return err
		}
		err = out.CopyFrom(*from)
		out.Close()
		from.Close()
		if err != nil {
			return err
		}
	}
	if status := win32.RegFlushKey(to.Root); status != 0 {
		return fmt.Errorf("flushing captured registry hive: error %d", status)
	}
	return nil
}

func guardHivePath(path string, directory bool) (*os.File, error) {
	h, err := win32.CreateFile(path, win32.FILE_READ_ATTRIBUTES, win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE, nil, win32.OPEN_EXISTING, win32.FILE_FLAG_OPEN_REPARSE_POINT|win32.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	info, err := f.Stat()
	if err == nil {
		var reparse bool
		reparse, err = mounts.IsReparse(path)
		if err == nil && (reparse || info.IsDir() != directory) {
			err = fmt.Errorf("invalid registry path or reparse point: %s", path)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
