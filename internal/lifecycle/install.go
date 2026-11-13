package lifecycle

import (
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/override"
	"github.com/quasidock/quasidock/internal/store"
)

// Install deploys an image's layers and updates the app's ref.
func Install(ctx *context.Context, image *oci.Image) error {
	cfg := image.Config
	previous, err := ctx.Store.ReadRef(cfg.ID)
	if err != nil {
		previous = nil
	}

	start := ctx.Now()
	for _, l := range image.Manifest.Layers {
		depPath, err := ctx.Store.DeployPath(l.Digest)
		if err != nil {
			return err
		}
		lockPath := store.LockPathFor(depPath)
		inUse, err := store.OpenLock(lockPath, false, true)
		if err != nil {
			return err
		}
		inUse.Release()

		if _, err := ctx.Store.Deploy(l); err != nil {
			return err
		}
	}
	ctx.Timed(start, "deploy")

	if err := ctx.Store.WriteRef(store.Ref{
		ID:       cfg.ID,
		Version:  cfg.Version,
		Manifest: image.ManifestDigest,
		Source:   image.Source,
	}); err != nil {
		return err
	}

	if previous != nil && previous.Manifest != image.ManifestDigest {
		if deleted, err := ctx.Store.DeleteUnusedDeployments(previous.Manifest); err == nil && deleted {
			ctx.Note("  kept     %s %s, which is still running; `quasidock prune` removes it later", cfg.ID, previous.Version)
		}
	}

	ctx.Note("installed %s %s\n  manifest %s", cfg.ID, cfg.Version, image.ManifestDigest)
	for alias, r := range cfg.Runtimes {
		ctx.Note("  runtime  %s: %s %s", alias, r.ID, r.Version)
	}
	for name, p := range cfg.Extensions {
		ctx.Note("  point    %s %s: installed extensions of it join its runs", name, p.Version)
	}
	if cfg.Extends != nil {
		ctx.Note("  extends  %s %s: joins the runs of what has it", cfg.Extends.Point, cfg.Extends.Version)
	}
	ownL := oci.OwnLayer(&image.Manifest)
	ctx.Note("  layer    %s (%d bytes)", ownL.Digest, ownL.Size)

	if cfg.Build != nil {
		if cfg.Build.Network {
			ctx.Note("  warning: a build step used the network, so this image may not rebuild the same")
		}
		if len(cfg.Build.Host) > 0 {
			var hostTools []string
			for k := range cfg.Build.Host {
				hostTools = append(hostTools, k)
			}
			ctx.Note("  warning: built with the host's %s, so this image won't rebuild the same elsewhere", strings.Join(hostTools, ", "))
		}
	}

	if !image.Described {
		if cfg.Permissions.Services != nil {
			ctx.Note("  serves   %s", oci.DescribeServices(cfg.Permissions.Services))
		}
		ctx.Note("  permits  %s", oci.DescribePermissions(cfg.Permissions))
		access := oci.AccessInfoOf(cfg)
		if len(access.Uses) > 0 {
			ctx.Note("  uses     %s", oci.DescribeUses(access.Uses))
		}
		if len(cfg.Registry) > 0 {
			ctx.Note("  keeps    %s with its data", oci.DescribeRegistry(cfg.Registry))
		}
	}

	saved, err := override.Load(ctx.Store, cfg.ID)
	if err == nil && !saved.IsEmpty() {
		ctx.Note("  override %s", saved.String())
	}

	if _, err := grants.SettleUnlessRunning(ctx, cfg.ID); err != nil {
		ctx.Note("warning: what %s's earlier runs were granted stays until its next run ends, `quasidock repair` or `quasidock rm`", cfg.ID)
	}
	return nil
}
