package lifecycle

import (
	"errors"
	"fmt"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/store"
)

type ImageAndCachePruneResult struct {
	Images *store.ImagePrunePlan
	Cache  []store.BuildCachePruneEntry
	Bytes  uint64
}

// PruneImagesAndBuildCache collects content using both sets of surviving
// references. The caller holds the exclusive store operation lock.
func PruneImagesAndBuildCache(ctx *context.Context, opts store.ImagePruneOptions, dry bool) (ImageAndCachePruneResult, error) {
	var result ImageAndCachePruneResult
	st := ctx.Store
	plan, err := st.PlanImagePrune(opts)
	if err != nil {
		return result, err
	}
	content, err := pruneBuildCache(st, plan, opts, dry, &result)
	if err != nil {
		return result, err
	}
	if err := applyPruneCatalog(st, plan, dry); err != nil {
		return result, err
	}
	result.Images = plan
	freed, err := content.collect(st, dry)
	result.Bytes += freed
	if !dry {
		err = errors.Join(err, cleanPrunedApps(ctx, plan))
	}
	return result, err
}

func pruneBuildCache(st *store.Store, plan *store.ImagePrunePlan, opts store.ImagePruneOptions, dry bool, result *ImageAndCachePruneResult) (pruneContent, error) {
	guard, err := grants.LockMountSources(st)
	if err != nil {
		return pruneContent{}, err
	}
	defer guard.Close()
	entries, retained, err := st.PlanBuildCachePrune(opts.Until, guard.Protected)
	if err != nil {
		return pruneContent{}, err
	}
	var removed []string
	for _, entry := range entries {
		if entry.Manifest != "" {
			removed = append(removed, entry.Manifest)
		}
	}
	content, err := planPruneContent(st, plan, retained, removed)
	if err != nil {
		return pruneContent{}, err
	}
	for _, entry := range entries {
		if !dry {
			if err := st.RemoveBuildCacheEntry(entry); err != nil {
				return pruneContent{}, fmt.Errorf("remove build cache entry %s/%s: %w", entry.Area, entry.Name, err)
			}
		}
		result.Cache = append(result.Cache, entry)
		result.Bytes += entry.Size
	}
	return content, nil
}
