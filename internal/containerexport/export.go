// Package containerexport writes filesystem archives of pinned container images
// and current private data, without fetching images or publishing store objects.
package containerexport

import (
	"archive/tar"
	stdcontext "context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/estargz"
	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/store"
)

type Options struct {
	Container string
	Pause     bool
}

// Export writes an uncompressed tar. Errors can leave a partial output stream.
func Export(cancel stdcontext.Context, ctx *context.Context, opts Options, dst io.Writer) error {
	return withSnapshot(cancel, ctx, opts, "", func(root string) error {
		return writeTar(cancel, root, dst)
	})
}

// ExportFile replaces output only after the complete archive has been synced.
func ExportFile(cancel stdcontext.Context, ctx *context.Context, opts Options, output string) error {
	return withSnapshot(cancel, ctx, opts, output, func(root string) error {
		f, err := os.CreateTemp(filepath.Dir(output), ".quasidock-export-*.tar")
		if err != nil {
			return err
		}
		defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
		if err := writeTar(cancel, root, f); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := cancel.Err(); err != nil {
			return err
		}
		return os.Rename(f.Name(), output)
	})
}

func withSnapshot(cancel stdcontext.Context, ctx *context.Context, opts Options, output string, consume func(string) error) error {
	lk, err := acquire(cancel, ctx.Store.Path("lock"), false)
	if err != nil {
		return err
	}
	defer lk.Release()
	c, err := ctx.Store.ResolveContainer(opts.Container)
	if err != nil {
		return err
	}
	op, err := acquire(cancel, ctx.Store.ContainerOperationLockPath(c.ID), true)
	if err != nil {
		return err
	}
	defer op.Release()
	c, err = ctx.Store.ReadContainer(c.ID)
	if err != nil {
		return err
	}
	if output != "" {
		if err := checkOutput(ctx.Store, c, output); err != nil {
			return err
		}
	}
	img, err := ctx.Store.ReadImage(c.Image, c.Image)
	if err != nil {
		if errors.Is(err, store.ErrMissingBlob) {
			return missingBlob(ctx.Store, c.Image, err)
		}
		return err
	}
	if err := verifyBlob(cancel, ctx.Store, img.Manifest.Config); err != nil {
		return err
	}
	layers := oci.AppLayers(&img.Manifest, &img.Config)
	seen := map[string]oci.Descriptor{}
	for _, desc := range layers {
		if previous, ok := seen[desc.Digest]; ok {
			if previous.Size != desc.Size {
				return fmt.Errorf("%w: %s (inconsistent descriptor sizes)", store.ErrCorruptBlob, desc.Digest)
			}
			continue
		}
		seen[desc.Digest] = desc
		if err := verifyBlob(cancel, ctx.Store, desc); err != nil {
			if errors.Is(err, store.ErrMissingBlob) {
				return missingBlob(ctx.Store, c.Image, err)
			}
			return err
		}
	}
	stage, err := ctx.Store.MakeTmpDir("container-export")
	if err != nil {
		return err
	}
	defer func() { _ = mounts.RemoveTree(stage) }()
	root := filepath.Join(stage, "root")
	for _, name := range []string{"app", "data", "runtimes"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			return err
		}
	}
	// Decode directly from verified blobs: deployed layers may still contain
	// cloud placeholders even after their complete blobs become available.
	if err := extractLayer(cancel, ctx.Store, oci.OwnLayer(&img.Manifest), stage, filepath.Join(root, "app")); err != nil {
		return err
	}
	byDigest := map[string]oci.Descriptor{}
	for _, desc := range layers {
		byDigest[desc.Digest] = desc
	}
	for _, alias := range slices.Sorted(maps.Keys(img.Config.From)) {
		desc := byDigest[img.Config.From[alias].Layer]
		if err := extractLayer(cancel, ctx.Store, desc, stage, filepath.Join(root, "runtimes", "runtime-"+alias)); err != nil {
			return err
		}
	}
	if err := run.CaptureData(cancel, ctx, c, img.Config.Registry, filepath.Join(root, "data"), opts.Pause); err != nil {
		return err
	}
	op.Release()
	return consume(root)
}

func acquire(ctx stdcontext.Context, path string, exclusive bool) (*store.Lock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lk, err := store.TryLock(path, exclusive)
		if err != nil || lk != nil {
			return lk, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func verifyBlob(ctx stdcontext.Context, st *store.Store, desc oci.Descriptor) error {
	f, err := os.Open(st.BlobPath(oci.DigestHex(desc.Digest)))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", store.ErrMissingBlob, desc.Digest)
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != desc.Size {
		return fmt.Errorf("%w: %s (size or file type mismatch)", store.ErrCorruptBlob, desc.Digest)
	}
	h := sha256.New()
	if _, err := io.Copy(h, cancelReader{ctx, f}); err != nil {
		return err
	}
	if fmt.Sprintf("sha256:%x", h.Sum(nil)) != desc.Digest {
		return fmt.Errorf("%w: %s", store.ErrCorruptBlob, desc.Digest)
	}
	return nil
}

func missingBlob(st *store.Store, image string, cause error) error {
	guidance := "fully pull the exact repository@digest before exporting"
	if ref, err := st.ResolveLocalImage("", image); err == nil {
		repository := ""
		if origin, err := registry.ParseReference(ref.Source); ref.Source != "" && err == nil {
			repository = origin.Host + "/" + origin.Repository
		} else if len(ref.Repositories) > 0 {
			repository = ref.Repositories[0]
		}
		if repository != "" {
			guidance = fmt.Sprintf("fully pull the exact image with `quasidock pull %s@%s` before exporting", repository, image)
		}
	}
	return fmt.Errorf("%w; %s", cause, guidance)
}

func checkOutput(st *store.Store, c *store.ContainerRecord, output string) error {
	parent, err := mounts.CanonicalPath(filepath.Dir(output))
	if err != nil {
		return err
	}
	output = filepath.Join(parent, filepath.Base(output))
	for _, source := range []string{c.DataDir, st.Path("blobs"), st.Path("deploy"), st.Path("tmp")} {
		source, err = mounts.CanonicalPath(source)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, output)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("export output overlaps container data or image storage; choose another directory")
		}
	}
	return nil
}

type cancelReader struct {
	ctx stdcontext.Context
	r   io.Reader
}

func (r cancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func safePath(name string) error {
	if !oci.IsSafeRelPath(name) || strings.Contains(name, `\`) {
		return fmt.Errorf("unsafe export path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if err := mounts.ValidateComponent(part); err != nil {
			return fmt.Errorf("unsafe export path %q: %w", name, err)
		}
	}
	return nil
}

func extractLayer(ctx stdcontext.Context, st *store.Store, desc oci.Descriptor, work, dest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(st.BlobPath(oci.DigestHex(desc.Digest)))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	kind, ok := format.FormatOfDescriptor(desc)
	if !ok {
		return fmt.Errorf("unsupported layer type %s", desc.MediaType)
	}
	if kind == format.FormatEstargz {
		r, err := estargz.Open(io.NewSectionReader(f, 0, desc.Size))
		if err != nil {
			return err
		}
		if want := desc.Annotations[estargz.TOCJSONDigestAnnotation]; want != r.TOCDigest() {
			return fmt.Errorf("layer %s: TOC digest mismatch", desc.Digest)
		}
		if err := format.CheckDeployable(desc, r); err != nil {
			return err
		}
	}
	var input io.Reader = cancelReader{ctx, f}
	if kind != format.FormatTar {
		// Gunzip handles concatenated eStargz members and checks trailing data.
		tmp, err := os.CreateTemp(work, "layer-*.tar")
		if err != nil {
			return err
		}
		defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
		if err := format.Gunzip(input, tmp); err != nil {
			return err
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		input = cancelReader{ctx, tmp}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(input)
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if kind == format.FormatEstargz && !estargz.IsLayerFile(h.Name) {
			continue
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "." && h.Typeflag == tar.TypeDir {
			continue
		}
		if err := safePath(name); err != nil {
			return err
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("duplicate layer path %q", name)
		}
		seen[key] = true
		switch h.Typeflag {
		case tar.TypeDir:
			if h.Size != 0 {
				return fmt.Errorf("layer directory %q has a payload", name)
			}
			if err := root.MkdirAll(filepath.FromSlash(name), 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := root.MkdirAll(filepath.Dir(filepath.FromSlash(name)), 0o755); err != nil {
				return err
			}
			out, err := root.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			if err := errors.Join(copyErr, out.Close()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported layer entry %q: only regular files and directories can be exported", name)
		}
	}
}

func writeTar(ctx stdcontext.Context, stage string, dst io.Writer) error {
	var names []string
	seen := map[string]bool{}
	err := store.WalkData(ctx, stage, nil, func(name string, info os.FileInfo, f *os.File) error {
		if f == nil || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("cannot export link or special file %s", name)
		}
		if err := safePath(name); err != nil {
			return err
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("duplicate export path %q", name)
		}
		seen[key] = true
		if info.IsDir() {
			name += "/"
		}
		names = append(names, name)
		return nil
	})
	if err != nil {
		return err
	}
	slices.Sort(names)
	root, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	lw := format.NewLayerWriter(dst)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.HasSuffix(name, "/") {
			if err := lw.AddDir(name); err != nil {
				return err
			}
			continue
		}
		f, err := root.Open(filepath.FromSlash(name))
		if err != nil {
			return err
		}
		info, statErr := f.Stat()
		if statErr != nil {
			_ = f.Close()
			return statErr
		}
		writeErr := lw.AddFile(name, info.Size(), cancelReader{ctx, f})
		if err := errors.Join(writeErr, f.Close()); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return lw.Finish()
}
