package lifecycle

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/lazy"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/services"
	"github.com/quasidock/quasidock/internal/store"
)

// CleanRemovedApp settles per-app resources after the final catalog image is
// removed. Application data is retained, as with image rm.
func CleanRemovedApp(ctx *context.Context, id string) error {
	var failures []error
	failures = append(failures, grants.UndoAll(ctx, id))
	if err := appcontainer.DeleteProfile(id); err != nil {
		ctx.Note("warning: cleaning profile for %s: %v", id, err)
	}
	failures = append(failures, services.Forget(ctx.Store, id), hkcu.SettleLeftovers(ctx.Store, id))
	return errors.Join(failures...)
}

// PruneImages plans, reports, and collects only selected image content. The
// caller holds an exclusive store operation lock, including for dry runs.
func PruneImages(ctx *context.Context, opts store.ImagePruneOptions, dry bool) error {
	plan, err := ctx.Store.PlanImagePrune(opts)
	if err != nil {
		return err
	}
	// Read and validate required metadata before changing the catalog. Only
	// metadata is read: absent lazy layer blobs are permitted.
	images := map[string]*store.LoadedImage{}
	read := func(digest string) (*store.LoadedImage, error) {
		if img := images[digest]; img != nil {
			return img, nil
		}
		img, err := ctx.Store.ReadImage(digest, digest)
		if err != nil {
			return nil, err
		}
		if img.Manifest.SchemaVersion != 2 || img.Manifest.MediaType != "" && img.Manifest.MediaType != oci.MediaTypeManifest {
			return nil, fmt.Errorf("invalid image manifest %s", digest)
		}
		for _, desc := range append([]oci.Descriptor{img.Manifest.Config}, img.Manifest.Layers...) {
			if oci.DigestHex(desc.Digest) == "" || desc.Size < 0 {
				return nil, fmt.Errorf("invalid blob descriptor in image %s: %s", digest, desc.Digest)
			}
		}
		images[digest] = img
		return img, nil
	}
	used := map[string]bool{}
	keep := func(digest string) error {
		img, err := read(digest)
		if err != nil {
			return err
		}
		used[digest] = true
		used[img.Manifest.Config.Digest] = true
		for _, layer := range img.Manifest.Layers {
			used[layer.Digest] = true
		}
		return nil
	}
	for _, ref := range plan.Retained {
		if err := keep(ref.Manifest); err != nil {
			return err
		}
	}
	for _, c := range plan.Containers {
		if err := keep(c.Image); err != nil {
			return err
		}
	}
	builds, err := ctx.Store.ListBuildImages()
	if err != nil {
		return err
	}
	for _, digest := range builds {
		if err := keep(digest); err != nil {
			return err
		}
	}
	blobs, layers := map[string]bool{}, map[string]bool{}
	for _, ref := range plan.Images {
		img, err := read(ref.Manifest)
		if err != nil {
			return err
		}
		blobs[ref.Manifest] = true
		blobs[img.Manifest.Config.Digest] = true
		for _, layer := range img.Manifest.Layers {
			blobs[layer.Digest] = true
			layers[layer.Digest] = true
		}
	}
	for digest := range used {
		delete(blobs, digest)
		delete(layers, digest)
	}
	if !dry {
		if err := ctx.Store.ApplyImagePrune(plan); err != nil {
			return err
		}
	}
	var failures []error
	var freed uint64
	for _, digest := range slices.Sorted(maps.Keys(layers)) {
		n, err := lazy.PruneLayer(ctx.Store, digest, dry)
		freed += n
		if err != nil {
			failures = append(failures, fmt.Errorf("cleaning layer %s: %w", digest, err))
			delete(blobs, digest)
		}
		if ctx.Store.DeploymentInUse(digest) {
			delete(blobs, digest)
		}
	}
	for _, digest := range slices.Sorted(maps.Keys(blobs)) {
		path := ctx.Store.BlobPath(oci.DigestHex(digest))
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("expected a regular blob file: %s", path)
		}
		if err == nil && !dry {
			err = os.Remove(path)
		}
		if err != nil {
			failures = append(failures, err)
		} else {
			freed += uint64(info.Size())
		}
	}
	if !dry {
		apps := map[string]bool{}
		for _, ref := range plan.Images {
			apps[strings.ToLower(ref.ID)] = true
		}
		for _, ref := range plan.Retained {
			delete(apps, strings.ToLower(ref.ID))
		}
		for _, app := range slices.Sorted(maps.Keys(apps)) {
			failures = append(failures, CleanRemovedApp(ctx, app))
		}
	}
	heading, untagged, deleted, total := "Deleted Images:", "untagged:", "deleted:", "Total reclaimed space:"
	if dry {
		heading, untagged, deleted, total = "Would remove images:", "would untag:", "would remove:", "Would reclaim:"
	}
	var report strings.Builder
	if len(plan.Images) != 0 {
		fmt.Fprintln(&report, heading)
		for _, name := range plan.Names {
			fmt.Fprintln(&report, untagged, name)
		}
		for _, ref := range plan.Images {
			fmt.Fprintln(&report, deleted, ref.Manifest)
		}
		fmt.Fprintln(&report)
	}
	fmt.Fprintln(&report, total, FormatBytes(freed))
	_, err = fmt.Fprint(ctx.Stdout, report.String())
	failures = append(failures, err)
	return errors.Join(failures...)
}
