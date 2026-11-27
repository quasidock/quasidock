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
	builds, err := ctx.Store.ListBuildImages()
	if err != nil {
		return err
	}
	content, err := planPruneContent(ctx.Store, plan, builds, nil)
	if err != nil {
		return err
	}
	if err := applyPruneCatalog(ctx.Store, plan, dry); err != nil {
		return err
	}
	freed, err := content.collect(ctx.Store, dry)
	if !dry {
		err = errors.Join(err, cleanPrunedApps(ctx, plan))
	}
	var report strings.Builder
	ReportPrunedImages(&report, plan, dry)
	total := "Total reclaimed space:"
	if dry {
		total = "Would reclaim:"
	}
	fmt.Fprintln(&report, total, FormatBytes(freed))
	_, writeErr := fmt.Fprint(ctx.Stdout, report.String())
	return errors.Join(err, writeErr)
}

type pruneContent struct {
	blobs, layers map[string]bool
}

func planPruneContent(st *store.Store, plan *store.ImagePrunePlan, retainedBuilds, removedBuilds []string) (pruneContent, error) {
	// Read and validate required metadata before changing the catalog. Only
	// metadata is read: absent lazy layer blobs are permitted.
	images := map[string]*store.LoadedImage{}
	read := func(digest string) (*store.LoadedImage, error) {
		if img := images[digest]; img != nil {
			return img, nil
		}
		img, err := st.ReadImage(digest, digest)
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
			return pruneContent{}, err
		}
	}
	for _, c := range plan.Containers {
		if err := keep(c.Image); err != nil {
			return pruneContent{}, err
		}
	}
	for _, digest := range retainedBuilds {
		if err := keep(digest); err != nil {
			return pruneContent{}, err
		}
	}
	content := pruneContent{blobs: map[string]bool{}, layers: map[string]bool{}}
	drop := func(digest string) error {
		img, err := read(digest)
		if err != nil {
			return err
		}
		content.blobs[digest] = true
		content.blobs[img.Manifest.Config.Digest] = true
		for _, layer := range img.Manifest.Layers {
			content.blobs[layer.Digest] = true
			content.layers[layer.Digest] = true
		}
		return nil
	}
	for _, ref := range plan.Images {
		if err := drop(ref.Manifest); err != nil {
			return pruneContent{}, err
		}
	}
	for _, digest := range removedBuilds {
		if err := drop(digest); err != nil {
			return pruneContent{}, err
		}
	}
	for digest := range used {
		delete(content.blobs, digest)
		delete(content.layers, digest)
	}
	return content, nil
}

func applyPruneCatalog(st *store.Store, plan *store.ImagePrunePlan, dry bool) error {
	if !dry {
		return st.ApplyImagePrune(plan)
	}
	return nil
}

func (content pruneContent) collect(st *store.Store, dry bool) (uint64, error) {
	var failures []error
	var freed uint64
	for _, digest := range slices.Sorted(maps.Keys(content.layers)) {
		n, err := lazy.PruneLayer(st, digest, dry)
		freed += n
		if err != nil {
			failures = append(failures, fmt.Errorf("cleaning layer %s: %w", digest, err))
			delete(content.blobs, digest)
		}
		if st.DeploymentInUse(digest) {
			delete(content.blobs, digest)
		}
	}
	for _, digest := range slices.Sorted(maps.Keys(content.blobs)) {
		path := st.BlobPath(oci.DigestHex(digest))
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
	return freed, errors.Join(failures...)
}

func cleanPrunedApps(ctx *context.Context, plan *store.ImagePrunePlan) error {
	apps := map[string]bool{}
	for _, ref := range plan.Images {
		apps[strings.ToLower(ref.ID)] = true
	}
	for _, ref := range plan.Retained {
		delete(apps, strings.ToLower(ref.ID))
	}
	var failures []error
	for _, app := range slices.Sorted(maps.Keys(apps)) {
		failures = append(failures, CleanRemovedApp(ctx, app))
	}
	return errors.Join(failures...)
}

// ReportPrunedImages adds the image section to a prune report.
func ReportPrunedImages(report *strings.Builder, plan *store.ImagePrunePlan, dry bool) {
	if plan == nil || len(plan.Images) == 0 {
		return
	}
	heading, untagged, deleted := "Deleted Images:", "untagged:", "deleted:"
	if dry {
		heading, untagged, deleted = "Would remove images:", "would untag:", "would remove:"
	}
	fmt.Fprintln(report, heading)
	for _, name := range plan.Names {
		fmt.Fprintln(report, untagged, name)
	}
	for _, ref := range plan.Images {
		fmt.Fprintln(report, deleted, ref.Manifest)
	}
	fmt.Fprintln(report)
}
