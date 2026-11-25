package run

import (
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/store"
)

// CaptureData copies private data and declared HKCU keys into an empty staging
// directory. The caller owns the shared store lock and container operation lock,
// and must remove the staging directory on failure. Any temporary pause is
// restored before this function returns, including on capture errors.
func CaptureData(cancel stdcontext.Context, ctx *context.Context, c *store.ContainerRecord, keys []string, dest string, pause bool) (retErr error) {
	if err := cancel.Err(); err != nil {
		return err
	}
	excluded, err := store.DataExclusions(c)
	if err != nil {
		return err
	}
	privateRegistry := len(keys) > 0 && !excluded[""] && !excluded["registry"]
	if privateRegistry {
		for rel := range excluded {
			if strings.HasPrefix(rel, "registry/") {
				return errors.New("cannot capture registry with mounts inside its hive directory")
			}
		}
		excluded["registry"] = true
	}
	if c.State.Running && pause {
		lease, err := BeginCapture(ctx, c)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, lease.Close()) }()
	}
	var rootGuard *os.File
	if !excluded[""] {
		rootGuard, err = store.OpenDataRoot(c.DataDir)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, rootGuard.Close()) }()
	}
	if err := store.CopyData(cancel, c.DataDir, dest, excluded); err != nil {
		return fmt.Errorf("capturing private data: %w", err)
	}
	if err := cancel.Err(); err != nil {
		return err
	}
	if privateRegistry {
		if err := hkcu.Snapshot(keys, c.DataDir, filepath.Join(dest, "registry"), c.State.Running); err != nil {
			return err
		}
	}
	return cancel.Err()
}
