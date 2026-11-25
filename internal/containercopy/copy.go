package containercopy

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/store"
)

// Copy transfers files between one container and a host path or tar stream.
// Malformed input archives are rejected before destination files are changed.
// Completed files are retained if a later file fails; this is not a transaction.
func Copy(ctx context.Context, st *store.Store, source, destination string, stdin io.Reader, stdout io.Writer) error {
	src, err := parseEndpoint(source)
	if err != nil {
		return err
	}
	dst, err := parseEndpoint(destination)
	if err != nil {
		return err
	}
	if (src.container == "") == (dst.container == "") {
		return errors.New("cp requires exactly one CONTAINER:PATH endpoint")
	}
	lock, err := acquire(ctx, st.Path("lock"), false)
	if err != nil {
		return err
	}
	defer lock.Release()
	container := src.container
	if container == "" {
		container = dst.container
	}
	c, err := st.ResolveContainer(container)
	if err != nil {
		return err
	}
	op, err := acquire(ctx, st.ContainerOperationLockPath(c.ID), true)
	if err != nil {
		return err
	}
	defer op.Release()
	c, err = st.ReadContainer(c.ID)
	if err != nil {
		return err
	}
	cf, err := containerFilesystem(st, c)
	if err != nil {
		return err
	}
	defer cf.close()
	if dst.stream {
		if stdout == nil {
			return errors.New("tar output is unavailable")
		}
		return writeTar(ctx, cf, src, stdout)
	}
	if src.stream {
		info, err := cf.stat(dst.name)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("tar destination must be an existing directory")
		}
		if stdin == nil {
			return errors.New("tar input is unavailable")
		}
		stage, err := st.MakeTmpDir("container-copy")
		if err != nil {
			return err
		}
		defer func() { _ = mounts.RemoveTree(stage) }()
		if err := stageTar(ctx, stdin, stage); err != nil {
			return err
		}
		src = endpoint{name: stage, contents: true}
		host := hostFilesystem(&src)
		defer host.close()
		return copyTree(ctx, host, src, cf, dst)
	}
	if src.container != "" {
		host := hostFilesystem(&dst)
		defer host.close()
		return copyTree(ctx, cf, src, host, dst)
	}
	host := hostFilesystem(&src)
	defer host.close()
	return copyTree(ctx, host, src, cf, dst)
}

func acquire(ctx context.Context, name string, exclusive bool) (*store.Lock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := store.TryLock(name, exclusive)
		if err != nil || lock != nil {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type copyEntry struct {
	source, destination string
	info                os.FileInfo
}

func copyTree(ctx context.Context, from *filesystem, src endpoint, to *filesystem, dst endpoint) error {
	sourceInfo, err := from.stat(src.name)
	if err != nil {
		return err
	}
	if src.directory && !sourceInfo.IsDir() {
		return errors.New("source path requires a directory")
	}
	destInfo, err := to.stat(dst.name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		if dst.directory && !sourceInfo.IsDir() {
			return errors.New("destination directory must exist")
		}
		parent, err := to.stat(parentName(dst.name))
		if err != nil {
			return err
		}
		if !parent.IsDir() {
			return errors.New("destination parent must be an existing directory")
		}
	}
	base := dst.name
	if destInfo != nil {
		if sourceInfo.IsDir() && !destInfo.IsDir() {
			return errors.New("cannot copy a directory to a file")
		}
		if destInfo.IsDir() && (!sourceInfo.IsDir() || !src.contents) {
			base = join(base, path.Base(src.name))
		}
	}
	if dst.directory && destInfo != nil && !destInfo.IsDir() {
		return errors.New("destination path requires a directory")
	}
	if sourceInfo.IsDir() {
		if err := rejectOverlap(from, src.name, to, base); err != nil {
			return err
		}
	}
	// Preflight placement and entry types before creating destination entries.
	var entries []copyEntry
	err = from.walk(ctx, src.name, func(name string, info os.FileInfo, _ *os.File) error {
		rel := strings.TrimPrefix(name[len(src.name):], "/")
		dest := join(base, rel)
		current, err := to.stat(dest)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if current != nil {
			if info.IsDir() != current.IsDir() {
				return fmt.Errorf("file/directory conflict at %s", dest)
			}
			if !info.IsDir() && os.SameFile(info, current) {
				return errors.New("source and destination are the same file")
			}
		}
		entries = append(entries, copyEntry{name, dest, info})
		return nil
	})
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.info.IsDir() {
			g, root, err := to.directory(e.destination, true)
			g.close()
			if err != nil {
				return err
			}
			_ = root.Close()
		} else if err := copyFile(ctx, from, to, e); err != nil {
			return fmt.Errorf("copying %s: %w", e.source, err)
		}
	}
	// Set directory times last: creating children changes their parent's time.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.info.IsDir() && !e.info.ModTime().IsZero() {
			g, root, err := to.directory(e.destination, false)
			if err != nil {
				return err
			}
			err = root.Chtimes(".", e.info.ModTime(), e.info.ModTime())
			_ = root.Close()
			g.close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func parentName(name string) string {
	p := path.Dir(name)
	if p == "." {
		return ""
	}
	return p
}

type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (r cancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func unchanged(before, after os.FileInfo) bool {
	return os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func copyFile(ctx context.Context, from, to *filesystem, e copyEntry) error {
	source, info, err := from.open(e.source)
	if err != nil {
		return err
	}
	defer source.close()
	if info.IsDir() || !unchanged(e.info, info) {
		return errors.New("source file changed during copy; retry with the application stopped")
	}
	parent, root, err := to.directory(parentName(e.destination), true)
	if err != nil {
		return err
	}
	defer parent.close()
	defer func() { _ = root.Close() }()
	name := path.Base(e.destination)
	if current, err := root.Lstat(name); err == nil {
		if reparse(current) || !current.Mode().IsRegular() {
			return fmt.Errorf("destination is not a regular file: %s", name)
		}
		if os.SameFile(info, current) {
			return errors.New("source and destination are the same file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A sibling inherits destination permissions and can be renamed atomically.
	tmpName := ".quasidock-cp-" + rand.Text()
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = tmp.Close(); _ = root.Remove(tmpName) }()
	if _, err := io.CopyN(tmp, cancelReader{ctx, source.file}, info.Size()); err != nil {
		return err
	}
	after, err := source.file.Stat()
	if err != nil {
		return err
	}
	if !unchanged(info, after) {
		return errors.New("source file changed during copy; retry with the application stopped")
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := root.Chtimes(tmpName, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return root.Rename(tmpName, name)
}

// A directory may consist of several physical regions because of mounts.
func regions(fs *filesystem, name string) ([]string, error) {
	p, err := fs.physical(name, false)
	if err != nil {
		return nil, err
	}
	paths := []string{p}
	for _, root := range fs.roots {
		if root.mount && below(root.target, name) && !strings.EqualFold(root.target, name) {
			p, err := fs.physical(root.target, false)
			if err != nil {
				return nil, err
			}
			paths = append(paths, p)
		}
	}
	for i, p := range paths {
		// Resolve existing ancestors too, detecting aliases such as NTFS short
		// names even when the final destination has not been created yet.
		var tail []string
		for {
			canonical, err := mounts.CanonicalPath(p)
			if err == nil {
				for j := len(tail) - 1; j >= 0; j-- {
					canonical = filepath.Join(canonical, tail[j])
				}
				paths[i] = canonical
				break
			}
			if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			tail = append(tail, filepath.Base(p))
			parent := filepath.Dir(p)
			if parent == p {
				return nil, err
			}
			p = parent
		}
	}
	return paths, nil
}

func rejectOverlap(from *filesystem, source string, to *filesystem, destination string) error {
	a, err := regions(from, source)
	if err != nil {
		return err
	}
	b, err := regions(to, destination)
	if err != nil {
		return err
	}
	for _, src := range a {
		for _, dst := range b {
			if mounts.Within(src, dst) || mounts.Within(dst, src) {
				return errors.New("source and destination directory trees overlap")
			}
		}
	}
	return nil
}
