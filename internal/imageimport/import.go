// Package imageimport creates images from Quasidock filesystem exports.
package imageimport

import (
	stdcontext "context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/imageconfig"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/store"
)

const maxConfigSize = 16 << 20

type Options struct {
	Target, Source string
	Changes        []string
	Force          bool
}

// Import stages and validates the complete input before publishing one image.
// config is app-config JSON, as emitted by image inspect's .Quasidock field.
func Import(cancel stdcontext.Context, ctx *context.Context, src, config io.Reader, opts Options) (string, error) {
	if err := cancel.Err(); err != nil {
		return "", err
	}
	name := ""
	if opts.Target != "" {
		var err error
		name, err = store.ImageName(ctx.Env["QUASIDOCK_REGISTRY"], opts.Target)
		if err != nil {
			return "", err
		}
		r, err := registry.ParseReference(name)
		if err != nil || r.Digest != "" || oci.DigestHex(opts.Target) != "" {
			return "", errors.New("import target must be a repository and optional tag, not a digest")
		}
	}
	cfg, err := readConfig(cancel, config, opts.Changes)
	if err != nil {
		return "", err
	}
	lock, err := ctx.Store.Lock(false)
	if err != nil {
		return "", err
	}
	defer lock.Release()
	stage, err := ctx.Store.MakeTmpDir("image-import")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	tree := filepath.Join(stage, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		return "", err
	}
	if err := stageArchive(cancel, src, tree, cfg.From); err != nil {
		return "", err
	}
	if err := validateWorkdir(tree, cfg.Workdir); err != nil {
		return "", err
	}
	blobs := map[string]store.ImportBlob{}
	var layers []oci.Descriptor
	addLayer := func(root string) (oci.Descriptor, error) {
		blob, err := writeLayer(cancel, filepath.Join(stage, fmt.Sprintf("layer-%d.gz", len(layers))), root)
		if err != nil {
			return oci.Descriptor{}, err
		}
		layers = append(layers, blob.Descriptor(oci.Descriptor{MediaType: blob.MediaType, Digest: blob.Digest, Size: blob.Size}))
		blobs[blob.Digest] = store.ImportBlob{Path: blob.Path, Digest: blob.Digest, Size: blob.Size}
		return layers[len(layers)-1], nil
	}
	for _, alias := range slices.Sorted(maps.Keys(cfg.From)) {
		layer, err := addLayer(filepath.Join(tree, "runtimes", "runtime-"+alias))
		if err != nil {
			return "", err
		}
		r := cfg.From[alias]
		r.Layer = layer.Digest
		cfg.From[alias] = r
	}
	seed, err := addLayer(filepath.Join(tree, "data"))
	if err != nil {
		return "", err
	}
	cfg.DataLayer = seed.Digest
	if _, err := addLayer(filepath.Join(tree, "app")); err != nil {
		return "", err
	}
	if err := oci.ValidateConfig("imported image", cfg); err != nil {
		return "", err
	}
	configDesc, err := stageJSON(stage, cfg, oci.MediaTypeConfigV4, blobs)
	if err != nil {
		return "", err
	}
	manifest := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: configDesc, Layers: layers}
	if err := oci.ValidateLayers("imported image", &manifest, cfg); err != nil {
		return "", err
	}
	manifestDesc, err := stageJSON(stage, manifest, oci.MediaTypeManifest, blobs)
	if err != nil {
		return "", err
	}
	digest := manifestDesc.Digest
	in := store.ImageImport{
		Images: map[string]store.Ref{digest: {ID: cfg.ID, Version: cfg.Version, Manifest: digest, Source: opts.Source}},
		Names:  map[string]string{},
	}
	if name != "" {
		in.Names[name] = digest
	}
	for _, d := range slices.Sorted(maps.Keys(blobs)) {
		in.Blobs = append(in.Blobs, blobs[d])
	}
	if err := cancel.Err(); err != nil {
		return "", err
	}
	if err := ctx.Store.ImportImages(in, opts.Force); err != nil {
		return "", err
	}
	return digest, nil
}

func readConfig(ctx stdcontext.Context, src io.Reader, changes []string) (*oci.AppConfig, error) {
	data, err := io.ReadAll(io.LimitReader(cancelReader{ctx, src}, maxConfigSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading import config: %w", err)
	}
	if len(data) > maxConfigSize {
		return nil, errors.New("import config exceeds 16 MiB")
	}
	var cfg oci.AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("reading import config: %w", err)
	}
	cfg.FormatVersion = 4
	// Input layer references describe the old image, not the exported trees.
	// Use placeholders for validation until the canonical layers are written.
	cfg.DataLayer = "sha256:" + strings.Repeat("0", 64)
	for alias, r := range cfg.From {
		r.Layer = cfg.DataLayer
		cfg.From[alias] = r
	}
	if err := imageconfig.ApplyChanges("imported image", &cfg, changes); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func writeLayer(ctx stdcontext.Context, dest, root string) (*format.Blob, error) {
	return format.WriteLayer(dest, "", format.FormatGzip, func(out io.Writer) error {
		lw := format.NewLayerWriter(out)
		err := store.WalkData(ctx, root, nil, func(name string, info os.FileInfo, f *os.File) error {
			if f == nil || (!info.IsDir() && !info.Mode().IsRegular()) {
				return fmt.Errorf("cannot import link or special file %q", name)
			}
			if info.IsDir() {
				return lw.AddDir(name)
			}
			return lw.AddFile(name, info.Size(), cancelReader{ctx, f})
		})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return lw.Finish()
	}, nil)
}

func stageJSON(dir string, value any, kind string, blobs map[string]store.ImportBlob) (oci.Descriptor, error) {
	data, err := oci.ToJSON(value)
	if err != nil {
		return oci.Descriptor{}, err
	}
	if len(data) > maxConfigSize {
		return oci.Descriptor{}, errors.New("generated image JSON exceeds 16 MiB")
	}
	hash := sha256.Sum256(data)
	digest := fmt.Sprintf("sha256:%x", hash)
	file := filepath.Join(dir, fmt.Sprintf("%x.json", hash))
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return oci.Descriptor{}, err
	}
	blobs[digest] = store.ImportBlob{Path: file, Digest: digest, Size: int64(len(data))}
	return oci.Descriptor{MediaType: kind, Digest: digest, Size: int64(len(data))}, nil
}

func validateWorkdir(tree, workdir string) error {
	if workdir == "" {
		return nil
	}
	app, data := filepath.Join(tree, "app"), filepath.Join(tree, "data")
	p := oci.Placeholders{App: app, Data: data, Cache: filepath.Join(data, "cache")}
	dir := filepath.Join(app, filepath.FromSlash(workdir))
	if oci.IsPlaceholderPath(workdir) {
		dir = p.Expand(workdir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("imported WORKDIR: %w", err)
	}
	if !info.IsDir() {
		return errors.New("imported WORKDIR is not a directory")
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
