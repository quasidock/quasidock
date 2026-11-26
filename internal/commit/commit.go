// Package commit captures a container's private storage into a runnable image.
package commit

import (
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/imageconfig"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/store"
)

type Options struct {
	Container, Target, Author, Message string
	Pause                              bool
	Changes                            []string
}

func Commit(cancel stdcontext.Context, ctx *context.Context, opts Options) (string, error) {
	lock, err := ctx.Store.Lock(false)
	if err != nil {
		return "", err
	}
	defer lock.Release()
	name := ""
	var target registry.Reference
	if opts.Target != "" {
		if oci.DigestHex(opts.Target) != "" {
			return "", errors.New("commit target must be a repository and tag, not a digest")
		}
		name, err = store.ImageName(ctx.Env["QUASIDOCK_REGISTRY"], opts.Target)
		if err != nil {
			return "", err
		}
		target, _ = registry.ParseReference(name)
		if target.Digest != "" || !oci.IsValidID(path.Base(target.Repository)) {
			return "", errors.New("commit target must have a valid app ID as its final repository component and a tag, not a digest")
		}
	}
	c, err := ctx.Store.ResolveContainer(opts.Container)
	if err != nil {
		return "", err
	}
	op, err := ctx.Store.LockContainer(c.ID)
	if err != nil {
		return "", err
	}
	defer op.Release()
	c, err = ctx.Store.ReadContainer(c.ID)
	if err != nil {
		return "", err
	}
	ref, err := ctx.Store.ResolveLocalImage("", c.Image)
	if err != nil {
		return "", err
	}
	img, err := ctx.Store.LoadImageRef(*ref)
	if err != nil {
		return "", err
	}
	defer img.Release()
	cfg, err := run.CommitConfiguration(ctx, c, img)
	if err != nil {
		replaced := false
		for _, change := range opts.Changes {
			words := strings.Fields(change)
			if len(words) > 0 && strings.EqualFold(words[0], "ENTRYPOINT") {
				replaced = true
			}
		}
		if !replaced {
			return "", err
		}
	}
	if name != "" {
		cfg.ID, cfg.Version = path.Base(target.Repository), target.Tag
	}
	// The actual digest is known after capture; validate the rest beforehand.
	cfg.DataLayer = "sha256:" + strings.Repeat("0", 64)
	if err := imageconfig.ApplyChanges("committed image", &cfg, opts.Changes); err != nil {
		return "", err
	}
	stage, err := ctx.Store.MakeTmpDir("commit")
	if err != nil {
		return "", err
	}
	defer func() { _ = mounts.RemoveTree(stage) }()
	if err := run.CaptureData(cancel, ctx, c, cfg.Registry, stage, opts.Pause); err != nil {
		return "", err
	}
	if err := validateWorkdir(img.DeployDir, stage, cfg.Workdir); err != nil {
		return "", err
	}
	op.Release()
	seed, err := ctx.Store.WriteDataLayer(cancel, stage)
	if err != nil {
		return "", err
	}
	cfg.DataLayer = seed.Digest
	configBytes, err := oci.ToJSON(cfg)
	if err != nil {
		return "", err
	}
	config, err := ctx.Store.PutBlob(configBytes, oci.MediaTypeConfigV4)
	if err != nil {
		return "", err
	}
	layers := oci.AppLayers(&img.Manifest, &img.Config)
	result := oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: config,
		Layers:      append(append(append([]oci.Descriptor(nil), layers[:len(layers)-1]...), seed), oci.OwnLayer(&img.Manifest)),
		Annotations: map[string]string{"org.opencontainers.image.created": time.Now().UTC().Format(time.RFC3339Nano), "io.quasidock.commit.parent": c.Image}}
	if opts.Author != "" {
		result.Annotations["org.opencontainers.image.authors"] = opts.Author
	}
	if opts.Message != "" {
		result.Annotations["io.quasidock.commit.message"] = opts.Message
	}
	if err := oci.ValidateLayers("committed image", &result, &cfg); err != nil {
		return "", err
	}
	bytes, err := oci.ToJSON(result)
	if err != nil {
		return "", err
	}
	manifest, err := ctx.Store.PutBlob(bytes, oci.MediaTypeManifest)
	if err != nil {
		return "", err
	}
	if err := cancel.Err(); err != nil {
		return "", err
	}
	err = ctx.Store.RegisterImage(store.Ref{ID: cfg.ID, Version: cfg.Version, Manifest: manifest.Digest, Source: name}, name, name != "")
	if err != nil {
		return "", err
	}
	return manifest.Digest, nil
}

func validateWorkdir(app, data, workdir string) error {
	if workdir == "" {
		return nil
	}
	p := oci.Placeholders{App: app, Data: data, Cache: filepath.Join(data, "cache")}
	dir := filepath.Join(app, filepath.FromSlash(workdir))
	if oci.IsPlaceholderPath(workdir) {
		dir = p.Expand(workdir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("committed WORKDIR: %w", err)
	}
	if !info.IsDir() {
		return errors.New("committed WORKDIR is not a directory")
	}
	return nil
}
