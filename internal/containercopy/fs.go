package containercopy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

// pinned retains ancestors without delete sharing to prevent replacement.
// Rooted I/O additionally confines operations during in-place reparse changes.
type pinned struct {
	file    *os.File
	parents *pinned
}

func (p *pinned) close() {
	if p != nil {
		_ = p.file.Close()
		p.parents.close()
	}
}

func reparse(info os.FileInfo) bool {
	a, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && a.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func pinDirectory(name string) (*pinned, error) {
	parent := filepath.Dir(name)
	var ancestors *pinned
	var err error
	if !strings.EqualFold(parent, name) {
		ancestors, err = pinDirectory(parent)
		if err != nil {
			return nil, err
		}
	}
	fail := func(err error) (*pinned, error) { ancestors.close(); return nil, err }
	h, err := win32.CreateFile(name, win32.FILE_READ_ATTRIBUTES|win32.FILE_LIST_DIRECTORY, win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE, nil, win32.OPEN_EXISTING,
		win32.FILE_FLAG_BACKUP_SEMANTICS|win32.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fail(fmt.Errorf("opening %s: %w", name, err))
	}
	f := os.NewFile(uintptr(h), name)
	info, err := f.Stat()
	if err == nil && reparse(info) {
		err = fmt.Errorf("cannot copy through reparse point %s", name)
	}
	if err == nil && !info.IsDir() {
		err = fmt.Errorf("not a directory: %s", name)
	}
	if err != nil {
		_ = f.Close()
		return fail(err)
	}
	return &pinned{f, ancestors}, nil
}

type backing struct {
	target, source  string
	mount, writable bool
	guard           *pinned
	root            *os.Root
}

type filesystem struct {
	roots []*backing
}

func hostFilesystem(e *endpoint) *filesystem {
	volume := filepath.VolumeName(e.name)
	root := volume + string(filepath.Separator)
	e.name = filepath.ToSlash(strings.TrimPrefix(e.name, root))
	return &filesystem{roots: []*backing{{source: root, writable: true}}}
}

func containerFilesystem(st *store.Store, c *store.ContainerRecord) (*filesystem, error) {
	if err := mounts.ValidateTargets(c.HostConfig.Mounts); err != nil {
		return nil, err
	}
	if len(c.HostConfig.Mounts) != len(c.Mounts) {
		return nil, errors.New("container mount metadata is inconsistent")
	}
	fs := &filesystem{}
	data := st.ContainerDataDir(c.ID)
	for _, spec := range c.HostConfig.Mounts {
		target, _ := mounts.NormalizeTarget(spec.Target)
		dest := mounts.Destination(data, target)
		var found bool
		for _, m := range c.Mounts {
			if !strings.EqualFold(m.Destination, dest) {
				continue
			}
			if m.Type != spec.Type || !filepath.IsAbs(m.Source) {
				return nil, errors.New("container mount metadata is inconsistent")
			}
			if m.Type == "volume" {
				v, err := st.ReadVolume(spec.Source)
				if err != nil {
					return nil, err
				}
				if !strings.EqualFold(v.Mountpoint, m.Source) {
					return nil, errors.New("container volume source changed")
				}
			} else if m.Type != "bind" || !strings.EqualFold(spec.Source, m.Source) {
				return nil, errors.New("container bind source changed")
			}
			fs.roots = append(fs.roots, &backing{target: strings.TrimPrefix(strings.TrimPrefix(target, "${data}"), "/"), source: m.Source, mount: true, writable: m.RW})
			found = true
			break
		}
		if !found {
			return nil, errors.New("container mount destination changed")
		}
	}
	// Mount mappings precede the fallback, including a mount covering all data.
	fs.roots = append(fs.roots, &backing{source: data, writable: true})
	return fs, nil
}

func (fs *filesystem) close() {
	for _, root := range fs.roots {
		if root.root != nil {
			_ = root.root.Close()
		}
		root.guard.close()
	}
}

func (fs *filesystem) physical(name string, write bool) (string, error) {
	root, rel, err := fs.locate(name, write)
	if err != nil {
		return "", err
	}
	return filepath.Join(root.source, filepath.FromSlash(rel)), nil
}

func (fs *filesystem) locate(name string, write bool) (*backing, string, error) {
	for _, root := range fs.roots {
		if !below(name, root.target) {
			continue
		}
		if write && !root.writable {
			return nil, "", errors.New("copy destination is a read-only mount")
		}
		if root.guard == nil {
			g, err := pinDirectory(root.source)
			if err != nil {
				return nil, "", err
			}
			if root.mount {
				if err := win32.RequireLocalNTFS(root.source); err != nil {
					g.close()
					return nil, "", err
				}
			}
			r, err := os.OpenRoot(root.source)
			if err != nil {
				g.close()
				return nil, "", err
			}
			a, aerr := g.file.Stat()
			b, berr := r.Stat(".")
			if aerr != nil || berr != nil || !os.SameFile(a, b) {
				_ = r.Close()
				g.close()
				return nil, "", errors.New("copy backing directory changed while opening")
			}
			root.guard, root.root = g, r
		}
		rel := relativeTo(name, root.target)
		return root, localName(rel), nil
	}
	panic("filesystem has no fallback")
}

func (fs *filesystem) virtualDirectory(name string) bool {
	for _, root := range fs.roots {
		if root.mount && !strings.EqualFold(root.target, name) && below(root.target, name) {
			return true
		}
	}
	return false
}

type virtualInfo struct{ name string }

func (v virtualInfo) Name() string     { return path.Base(v.name) }
func (virtualInfo) Size() int64        { return 0 }
func (virtualInfo) Mode() os.FileMode  { return os.ModeDir | 0o755 }
func (virtualInfo) ModTime() time.Time { return time.Time{} }
func (virtualInfo) IsDir() bool        { return true }
func (virtualInfo) Sys() any           { return nil }

func (fs *filesystem) open(name string) (*pinned, os.FileInfo, error) {
	root, rel, err := fs.locate(name, false)
	if err != nil {
		return nil, nil, err
	}
	// Pin and inspect ancestry without following aliases. Content is opened
	// through os.Root as well: even an in-place reparse mutation cannot redirect
	// the operation outside this backing tree.
	p := filepath.Join(root.source, rel)
	parents, err := pinDirectory(filepath.Dir(p))
	if err == nil {
		var info os.FileInfo
		info, err = root.root.Lstat(rel)
		if err == nil && reparse(info) {
			err = fmt.Errorf("cannot copy reparse point %s", name)
		}
	}
	if errors.Is(err, os.ErrNotExist) && fs.virtualDirectory(name) {
		parents.close()
		return nil, virtualInfo{name}, nil
	}
	if err != nil {
		parents.close()
		return nil, nil, err
	}
	f, err := root.root.Open(rel)
	if err != nil {
		parents.close()
		return nil, nil, err
	}
	g := &pinned{f, parents}
	info, err := f.Stat()
	if err != nil {
		g.close()
		return nil, nil, err
	}
	if reparse(info) || !info.IsDir() && !info.Mode().IsRegular() {
		g.close()
		return nil, nil, fmt.Errorf("cannot copy nonregular entry %s", name)
	}
	if !info.IsDir() && fs.virtualDirectory(name) {
		g.close()
		return nil, nil, fmt.Errorf("mount ancestor is not a directory: %s", name)
	}
	return g, info, nil
}

func (fs *filesystem) stat(name string) (os.FileInfo, error) {
	g, info, err := fs.open(name)
	g.close()
	return info, err
}

// walk merges saved mount paths into directory enumeration, even when the
// runtime junctions and their private ancestor directories do not exist.
func (fs *filesystem) walk(ctx context.Context, name string, visit func(string, os.FileInfo, *os.File) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g, info, err := fs.open(name)
	if err != nil {
		return err
	}
	defer g.close()
	var file *os.File
	if g != nil {
		file = g.file
	}
	if err := visit(name, info, file); err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	children := map[string]string{}
	if file != nil {
		entries, err := file.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, e := range entries {
			children[strings.ToLower(e.Name())] = e.Name()
		}
	}
	for _, root := range fs.roots {
		if !root.mount || !below(root.target, name) || strings.EqualFold(root.target, name) {
			continue
		}
		rel := relativeTo(root.target, name)
		child := strings.SplitN(rel, "/", 2)[0]
		children[strings.ToLower(child)] = child
	}
	names := make([]string, 0, len(children))
	for _, child := range children {
		names = append(names, child)
	}
	sort.Strings(names)
	for _, child := range names {
		if err := mounts.ValidateComponent(child); err != nil {
			return err
		}
		if err := fs.walk(ctx, join(name, child), visit); err != nil {
			return err
		}
	}
	return nil
}

func localName(name string) string {
	if name == "" {
		return "."
	}
	return filepath.FromSlash(name)
}

func (fs *filesystem) directory(name string, create bool) (*pinned, *os.Root, error) {
	root, rel, err := fs.locate(name, true)
	if err != nil {
		return nil, nil, err
	}
	if create {
		if err := root.root.MkdirAll(rel, 0o755); err != nil {
			return nil, nil, err
		}
	}
	g, err := pinDirectory(filepath.Join(root.source, rel))
	if err != nil {
		return nil, nil, err
	}
	r, err := root.root.OpenRoot(rel)
	if err != nil {
		g.close()
		return nil, nil, err
	}
	a, aerr := g.file.Stat()
	b, berr := r.Stat(".")
	if aerr != nil || berr != nil || !os.SameFile(a, b) {
		_ = r.Close()
		g.close()
		return nil, nil, errors.New("copy destination directory changed while opening")
	}
	return g, r, nil
}
