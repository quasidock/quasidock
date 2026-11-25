package containercopy

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

func writeTar(ctx context.Context, fs *filesystem, src endpoint, out io.Writer) error {
	tw := tar.NewWriter(out)
	base := path.Base(src.name)
	if src.contents {
		base = ""
	}
	err := fs.walk(ctx, src.name, func(name string, info os.FileInfo, file *os.File) error {
		if src.directory && name == src.name && !info.IsDir() {
			return errors.New("source path requires a directory")
		}
		rel := strings.TrimPrefix(name[len(src.name):], "/")
		name = join(base, rel)
		if name == "" {
			name = "."
		}
		h := &tar.Header{Name: name, Mode: 0o644, ModTime: info.ModTime(), Typeflag: tar.TypeReg, Size: info.Size()}
		if info.IsDir() {
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.IsDir() {
			if _, err := io.CopyN(tw, cancelReader{ctx, file}, info.Size()); err != nil {
				return err
			}
			after, err := file.Stat()
			if err != nil {
				return err
			}
			if !unchanged(info, after) {
				return fmt.Errorf("source file changed during copy: %s", name)
			}
		}
		return nil
	})
	if err != nil {
		return err
	} // Do not finish a failed output stream.
	if err := ctx.Err(); err != nil {
		return err
	}
	return tw.Close()
}

func stageTar(ctx context.Context, in io.Reader, stage string) error {
	root, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	input := &countReader{r: cancelReader{ctx, in}}
	tr := tar.NewReader(input)
	seen := map[string]bool{}
	times := map[string]time.Time{}
	for {
		before := input.n
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// archive/tar also accepts physical EOF without end markers. Require
			// both zero blocks so truncated or empty input cannot be applied.
			if input.n-before < 1024 {
				return errors.New("tar input is missing its end-of-archive blocks")
			}
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar input: %w", err)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA && h.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsupported tar entry type for %q: only regular files and directories are supported", h.Name)
		}
		name, err := relativePath(h.Name)
		if err != nil {
			return fmt.Errorf("unsafe tar path %q: %w", h.Name, err)
		}
		if name == "" && h.Typeflag != tar.TypeDir {
			return errors.New("tar file has an empty path")
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("duplicate tar path %q", h.Name)
		}
		seen[key] = true
		if name == "" {
			name = "."
		}
		if h.Typeflag == tar.TypeDir {
			if h.Size != 0 {
				return fmt.Errorf("tar directory %q has a payload", h.Name)
			}
			if err := root.MkdirAll(filepath.FromSlash(name), 0o755); err != nil {
				return err
			}
		} else {
			if err := root.MkdirAll(filepath.FromSlash(path.Dir(name)), 0o755); err != nil {
				return err
			}
			f, err := root.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("conflicting tar path %q: %w", h.Name, err)
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		}
		times[name] = h.ModTime
	}
	// Tar writers may pad to a larger block size; only zero padding may follow
	// the end markers. Consume it to catch late input errors and concatenated data.
	if _, err := io.Copy(zeroPadding{}, input); err != nil {
		return err
	}
	if input.n%512 != 0 {
		return errors.New("tar input has incomplete block padding")
	}
	for name, mtime := range times {
		if !mtime.IsZero() {
			if err := root.Chtimes(filepath.FromSlash(name), mtime, mtime); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

type countReader struct {
	r io.Reader
	n int64
}

func (r *countReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

type zeroPadding struct{}

func (zeroPadding) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != 0 {
			return 0, errors.New("nonzero data after tar end-of-archive blocks")
		}
	}
	return len(p), nil
}
