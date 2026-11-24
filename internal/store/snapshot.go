package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/win32"
)

// DataExclusions returns mount destinations relative to the private data root.
func DataExclusions(c *ContainerRecord) (map[string]bool, error) {
	excluded := make(map[string]bool)
	for _, m := range c.HostConfig.Mounts {
		target, err := mounts.NormalizeTarget(m.Target)
		if err != nil {
			return nil, err
		}
		excluded[strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(target, "${data}"), "/"))] = true
	}
	return excluded, nil
}

// OpenDataRoot keeps a private data root from being replaced between file and
// registry capture. Root mounts are excluded before calling this helper.
func OpenDataRoot(root string) (*os.File, error) {
	h, err := win32.CreateFile(root, win32.FILE_READ_ATTRIBUTES, win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE, nil, win32.OPEN_EXISTING, win32.FILE_FLAG_BACKUP_SEMANTICS|win32.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), root)
	info, err := f.Stat()
	if err == nil && (diffReparse(info) || !info.IsDir()) {
		err = fmt.Errorf("data root is not a plain directory: %s", root)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// WalkData opens every entry without following reparse points and retains ancestor
// handles without delete sharing. The callback must consume files before returning.
// Reparse points are reported with a nil file; callers must never reopen them.
func WalkData(ctx context.Context, root string, excluded map[string]bool, visit func(string, os.FileInfo, *os.File) error) error {
	if excluded[""] {
		return nil
	}
	var walk func(string, string) error
	walk = func(path, rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := win32.CreateFile(path, win32.GENERIC_READ, win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE, nil, win32.OPEN_EXISTING,
			win32.FILE_FLAG_BACKUP_SEMANTICS|win32.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return fmt.Errorf("opening %s: %w", path, err)
		}
		f := os.NewFile(uintptr(h), path)
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if diffReparse(info) {
			if rel == "" {
				return fmt.Errorf("data root is a reparse point: %s", path)
			}
			return visit(rel, info, nil)
		}
		if rel == "" && !info.IsDir() {
			return fmt.Errorf("data root is not a directory: %s", path)
		}
		if rel != "" {
			if err := visit(rel, info, f); err != nil {
				return err
			}
		}
		if !info.IsDir() {
			return nil
		}
		entries, err := f.ReadDir(-1)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			name := entry.Name()
			if rel != "" {
				name = rel + "/" + name
			}
			if excluded[strings.ToLower(name)] {
				continue
			}
			if err := walk(filepath.Join(path, entry.Name()), name); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root, "")
}

func CopyData(ctx context.Context, source, dest string, excluded map[string]bool) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return WalkData(ctx, source, excluded, func(rel string, info os.FileInfo, f *os.File) error {
		if f == nil {
			return fmt.Errorf("cannot capture reparse point %s", rel)
		}
		path := filepath.Join(dest, filepath.FromSlash(rel))
		if info.IsDir() {
			return os.MkdirAll(path, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("cannot capture nonregular file %s", rel)
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, &cancelReader{ctx: ctx, r: f})
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		after, err := f.Stat()
		if err != nil {
			return err
		}
		if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return fmt.Errorf("file changed during capture: %s; retry with the application stopped", rel)
		}
		return nil
	})
}

type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *cancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// WriteDataLayer writes a staged snapshot with canonical ordering and metadata.
func (s *Store) WriteDataLayer(ctx context.Context, root string) (oci.Descriptor, error) {
	tmp := s.TmpPath("commit-layer")
	defer func() { _ = os.Remove(tmp) }()
	blob, err := format.WriteLayer(tmp, "", format.FormatGzip, func(w io.Writer) error {
		lw := format.NewLayerWriter(w)
		err := WalkData(ctx, root, nil, func(rel string, info os.FileInfo, f *os.File) error {
			if f == nil {
				return fmt.Errorf("cannot capture reparse point %s", rel)
			}
			if info.IsDir() {
				return lw.AddDir(rel)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("cannot capture nonregular file %s", rel)
			}
			return lw.AddFile(rel, info.Size(), &cancelReader{ctx: ctx, r: f})
		})
		if err != nil {
			return err
		}
		return lw.Finish()
	}, nil)
	if err != nil {
		return oci.Descriptor{}, err
	}
	return s.PutBlobFile(blob.Path, FileHash{Hex: oci.DigestHex(blob.Digest), Size: blob.Size}, blob.MediaType)
}

// DataSeed deploys only the seed, without preparing the executable runtime.
func (s *Store) DataSeed(image string) (string, error) {
	if image == "" {
		return "", nil
	}
	img, err := s.ReadImage(image, image)
	if err != nil {
		return "", err
	}
	if img.Config.DataLayer == "" {
		return "", nil
	}
	return s.Deploy(oci.DataDescriptor(&img.Manifest, &img.Config))
}

func (s *Store) SeedContainerData(c *ContainerRecord) error {
	seed, err := s.DataSeed(c.Image)
	if err != nil || seed == "" {
		return err
	}
	excluded, err := DataExclusions(c)
	if err != nil {
		return err
	}
	return CopyData(context.Background(), seed, c.DataDir, excluded)
}

type dataFingerprint struct {
	path string
	kind byte
	hash [32]byte
}

func dataFingerprints(ctx context.Context, root string, excluded map[string]bool) (map[string]dataFingerprint, error) {
	entries := make(map[string]dataFingerprint)
	err := WalkData(ctx, root, excluded, func(rel string, info os.FileInfo, f *os.File) error {
		e := dataFingerprint{path: "/" + rel, kind: 'f'}
		if f == nil {
			e.kind = 'r'
		} else if info.IsDir() {
			e.kind = 'd'
		} else {
			h := sha256.New()
			if _, err := io.Copy(h, f); err != nil {
				return err
			}
			copy(e.hash[:], h.Sum(nil))
		}
		entries[strings.ToLower(rel)] = e
		return nil
	})
	return entries, err
}
