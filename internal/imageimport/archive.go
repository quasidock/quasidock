package imageimport

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
)

func stageArchive(ctx context.Context, src io.Reader, dest string, runtimes map[string]oci.Runtime) error {
	br := bufio.NewReader(cancelReader{ctx, src})
	header, err := br.Peek(2)
	if err != nil {
		return fmt.Errorf("reading import archive: %w", err)
	}
	var reader io.Reader = br
	if header[0] == 0x1f && header[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("reading gzip archive: %w", err)
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	expected := map[string]bool{"app": true, "data": true, "runtimes": true}
	for alias := range runtimes {
		expected["runtimes/runtime-"+alias] = true
	}
	seen := map[string]bool{}
	input := &countReader{r: cancelReader{ctx, reader}}
	tr := tar.NewReader(input)
	for {
		before := input.n
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			if input.n-before < 1024 {
				return errors.New("import tar is missing its end-of-archive blocks")
			}
			break
		}
		if err != nil {
			return fmt.Errorf("reading import tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA && h.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsupported import entry %q: only regular files and directories are supported", h.Name)
		}
		name, err := memberPath(h.Name)
		if err != nil {
			return err
		}
		if name == "" && h.Typeflag != tar.TypeDir {
			return errors.New("import file has an empty path")
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("duplicate import path %q", h.Name)
		}
		seen[key] = true
		if name != "" {
			parts := strings.Split(name, "/")
			if parts[0] != "app" && parts[0] != "data" && parts[0] != "runtimes" {
				return fmt.Errorf("unknown import root %q; expected app/, data/, and runtimes/", parts[0])
			}
			if parts[0] == "runtimes" && len(parts) > 1 && !expected["runtimes/"+parts[1]] {
				return fmt.Errorf("import runtime %q has no matching config alias", parts[1])
			}
			if expected[name] && h.Typeflag != tar.TypeDir {
				return fmt.Errorf("import path %q must be a directory", name)
			}
		}
		if h.Typeflag == tar.TypeDir {
			if h.Size != 0 {
				return fmt.Errorf("import directory %q has a payload", h.Name)
			}
			if name == "" {
				name = "."
			}
			if err := root.MkdirAll(filepath.FromSlash(name), 0o755); err != nil {
				return err
			}
			continue
		}
		if err := root.MkdirAll(filepath.Dir(filepath.FromSlash(name)), 0o755); err != nil {
			return err
		}
		f, err := root.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("conflicting import path %q: %w", name, err)
		}
		_, copyErr := io.Copy(f, tr)
		if err := errors.Join(copyErr, f.Close()); err != nil {
			return err
		}
	}
	// Drain through the gzip footer and late read errors. Only complete zero tar
	// padding may follow the end markers, never another archive or payload.
	if _, err := io.Copy(zeroPadding{}, input); err != nil {
		return fmt.Errorf("finishing import archive: %w", err)
	}
	if input.n%512 != 0 {
		return errors.New("import tar has incomplete block padding")
	}
	for name := range expected {
		if !seen[name] {
			return fmt.Errorf("import archive is missing directory %q", name)
		}
	}
	return ctx.Err()
}

func memberPath(raw string) (string, error) {
	name := strings.TrimSuffix(raw, "/")
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if name == "." {
		return "", nil
	}
	if !oci.IsSafeRelPath(name) || strings.Contains(name, `\`) {
		return "", fmt.Errorf("unsafe import path %q", raw)
	}
	for _, part := range strings.Split(name, "/") {
		if err := mounts.ValidateComponent(part); err != nil {
			return "", fmt.Errorf("unsafe import path %q: %w", raw, err)
		}
	}
	return name, nil
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
			return 0, errors.New("nonzero data after import tar end markers")
		}
	}
	return len(p), nil
}
