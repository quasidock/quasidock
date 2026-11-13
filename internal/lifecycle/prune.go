package lifecycle

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/context"
	"github.com/zigsaw-io/zigsaw/internal/lazy"
	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/roots"
	"github.com/zigsaw-io/zigsaw/internal/services"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

// PruneOptions configures zigsaw prune.
type PruneOptions struct {
	// Only report what would be deleted.
	DryRun bool
	// Also delete cached build sources, build tools' caches, and work directories.
	Downloads bool
	// Also delete the data directories of apps that aren't installed.
	Data bool
	// Also forget the versions kept for rollback.
	Previous bool
}

type tally struct {
	count int
	bytes uint64
}

func (t *tally) add(b uint64) {
	t.count++
	t.bytes += b
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func treeSize(p string) (uint64, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if !fi.IsDir() {
		return uint64(fi.Size()), nil
	}
	var total uint64
	_ = filepath.WalkDir(p, func(item string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += uint64(info.Size())
			}
		}
		return nil
	})
	return total, nil
}

func isInstalled(refs []store.Ref, id string) bool {
	for _, ref := range refs {
		if strings.EqualFold(ref.ID, id) {
			return true
		}
	}
	return false
}

func isInstalledManifest(refs []store.Ref, digest string) bool {
	for _, ref := range refs {
		if ref.Manifest == digest {
			return true
		}
	}
	return false
}

func deleteStaleLocks(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".lock") {
			continue
		}
		lockPath := filepath.Join(dir, name)
		targetPath := strings.TrimSuffix(lockPath, ".lock")
		if store.Exists(targetPath) {
			continue
		}
		if store.IsLocked(lockPath) {
			continue
		}
		_ = os.Remove(lockPath)
	}
}

// Prune deletes what no installed app needs.
func Prune(ctx *context.Context, opts PruneOptions) error {
	refs, err := ctx.Store.ListRefs()
	if err != nil {
		return err
	}
	buildImages, err := ctx.Store.ListBuildImages()
	if err != nil {
		buildImages = nil
	}

	var previous []store.Ref
	if rawPrev, err := ctx.Store.ListPrevious(); err == nil {
		for _, p := range rawPrev {
			if isInstalled(refs, p.ID) {
				previous = append(previous, p)
			}
		}
	}
	keptPrevious := !opts.Previous

	var manifests []string
	for _, ref := range refs {
		manifests = append(manifests, ref.Manifest)
	}
	if keptPrevious {
		for _, p := range previous {
			hex := oci.DigestHex(p.Manifest)
			if hex == "" {
				continue
			}
			if store.Exists(ctx.Store.BlobPath(hex)) {
				manifests = append(manifests, p.Manifest)
			}
		}
	}
	if !opts.Downloads {
		manifests = append(manifests, buildImages...)
	}

	blobsUsed := make(map[string]struct{})
	for _, digest := range manifests {
		manifest, err := ctx.Store.ReadManifest(digest)
		if err != nil {
			continue
		}
		descs := append([]oci.Descriptor{manifest.Config}, manifest.Layers...)
		for _, d := range descs {
			if h := oci.DigestHex(d.Digest); h != "" {
				blobsUsed[h] = struct{}{}
			}
		}
		if h := oci.DigestHex(digest); h != "" {
			blobsUsed[h] = struct{}{}
		}
	}

	var previousOnly tally
	if keptPrevious {
		others := make(map[string]struct{})
		for _, ref := range refs {
			m, err := ctx.Store.ReadManifest(ref.Manifest)
			if err != nil {
				continue
			}
			if h := oci.DigestHex(ref.Manifest); h != "" {
				others[h] = struct{}{}
			}
			if h := oci.DigestHex(m.Config.Digest); h != "" {
				others[h] = struct{}{}
			}
			for _, l := range m.Layers {
				if h := oci.DigestHex(l.Digest); h != "" {
					others[h] = struct{}{}
				}
			}
		}
		counted := make(map[string]struct{})
		for _, p := range previous {
			hex := oci.DigestHex(p.Manifest)
			if hex == "" {
				continue
			}
			bp := ctx.Store.BlobPath(hex)
			if _, exists := others[hex]; exists || !store.Exists(bp) {
				continue
			}
			previousOnly.count++
			m, err := ctx.Store.ReadManifest(p.Manifest)
			if err != nil {
				continue
			}
			descs := append([]oci.Descriptor{m.Config}, m.Layers...)
			for _, d := range descs {
				h := oci.DigestHex(d.Digest)
				if h == "" {
					continue
				}
				if _, exists := others[h]; exists {
					continue
				}
				if _, seen := counted[h]; seen {
					continue
				}
				counted[h] = struct{}{}
				pPath := ctx.Store.BlobPath(h)
				if store.Exists(pPath) {
					sz, _ := treeSize(pPath)
					previousOnly.bytes += sz
				}
			}
		}
	} else if !opts.DryRun {
		for _, p := range previous {
			_, _ = ctx.Store.ForgetPrevious(p.ID)
		}
	}

	deploymentsUsed, err := ctx.Store.UsedLayers(!opts.Downloads)
	if err != nil {
		deploymentsUsed = make(map[string]struct{})
	}

	var blobs tally
	blobDir := ctx.Store.Path("blobs", "sha256")
	if entries, err := os.ReadDir(blobDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if _, used := blobsUsed[name]; used {
				continue
			}
			p := filepath.Join(blobDir, name)
			sz, _ := treeSize(p)
			blobs.add(sz)
			if !opts.DryRun {
				_ = os.Remove(p)
			}
		}
	}

	var deployments tally
	var deploymentsInUse int
	deployDir := ctx.Store.Path("deploy")
	if entries, err := os.ReadDir(deployDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if entry.Type().IsRegular() || !oci.IsSha256Hex(name) {
				continue
			}
			if _, used := deploymentsUsed[name]; used {
				continue
			}
			digest := "sha256:" + name
			sz, _ := treeSize(filepath.Join(deployDir, name))
			var inUse bool
			if opts.DryRun {
				inUse = ctx.Store.DeploymentInUse(digest)
			} else {
				del, _ := ctx.Store.DeleteDeployment(digest)
				inUse = (del == store.DeletionInUse)
			}
			if inUse {
				deploymentsInUse++
			} else {
				deployments.add(sz)
			}
		}
	}
	if !opts.DryRun {
		deleteStaleLocks(deployDir)
		_, _ = lazy.Prune(ctx.Store, "")
	}

	var tmp tally
	var tmpInUse int
	tmpDir := ctx.Store.Path("tmp")
	if entries, err := os.ReadDir(tmpDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasSuffix(name, ".lock") {
				continue
			}
			p := filepath.Join(tmpDir, name)
			if store.IsLocked(store.LockPathFor(p)) {
				tmpInUse++
				continue
			}
			sz, _ := treeSize(p)
			tmp.add(sz)
			if !opts.DryRun {
				_ = store.DeleteTree(p)
			}
		}
	}
	if !opts.DryRun {
		deleteStaleLocks(tmpDir)
		deleteStaleLocks(ctx.Store.Path("grants"))
	}

	var downloads tally
	downloadsDir := ctx.Store.Path("cache", "downloads")
	if entries, err := os.ReadDir(downloadsDir); err == nil {
		for _, entry := range entries {
			p := filepath.Join(downloadsDir, entry.Name())
			sz, _ := treeSize(p)
			downloads.add(sz)
			if opts.Downloads && !opts.DryRun {
				_ = os.RemoveAll(p)
			}
		}
	}

	var toolCaches tally
	toolsDir := ctx.Store.Path("cache", "tools")
	if entries, err := os.ReadDir(toolsDir); err == nil {
		for _, entry := range entries {
			p := filepath.Join(toolsDir, entry.Name())
			sz, _ := treeSize(p)
			toolCaches.add(sz)
			if opts.Downloads && !opts.DryRun {
				_ = store.DeleteTree(p)
			}
		}
	}

	var work tally
	workDir := ctx.Store.Path("cache", "work")
	if entries, err := os.ReadDir(workDir); err == nil {
		for _, entry := range entries {
			p := filepath.Join(workDir, entry.Name())
			sz, _ := treeSize(p)
			work.add(sz)
			if opts.Downloads && !opts.DryRun {
				_ = store.DeleteTree(p)
			}
		}
	}

	buildsDir := ctx.Store.Path("cache", "builds")
	if !opts.DryRun {
		if entries, err := os.ReadDir(buildsDir); err == nil {
			for _, entry := range entries {
				digest, _ := ctx.Store.ReadBuildCache(entry.Name())
				gone := true
				if digest != "" {
					if h := oci.DigestHex(digest); h != "" {
						gone = !store.Exists(ctx.Store.BlobPath(h))
					}
				}
				if opts.Downloads || gone {
					_ = os.Remove(filepath.Join(buildsDir, entry.Name()))
				}
			}
		}
	}

	var buildOnly int
	for _, digest := range buildImages {
		if !isInstalledManifest(refs, digest) {
			buildOnly++
		}
		if opts.Downloads && !opts.DryRun {
			if h := oci.DigestHex(digest); h != "" {
				_ = os.Remove(ctx.Store.Path("cache", "images", h))
			}
		}
	}

	var data tally
	var orphans []string
	dataDir := ctx.Store.Path("data")
	if entries, err := os.ReadDir(dataDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() || isInstalled(refs, entry.Name()) {
				continue
			}
			p := filepath.Join(dataDir, entry.Name())
			sz, _ := treeSize(p)
			data.add(sz)
			orphans = append(orphans, fmt.Sprintf("%s (%s)", entry.Name(), formatBytes(sz)))
			if opts.Data && !opts.DryRun {
				_ = store.DeleteTree(p)
			}
		}
	}

	aliasesDir := ctx.Store.Path("aliases")
	if !opts.DryRun {
		if entries, err := os.ReadDir(aliasesDir); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() || isInstalled(refs, entry.Name()) {
					continue
				}
				l, err := ctx.Store.LockAliases(entry.Name())
				if err == nil && l != nil {
					_ = store.DeleteTree(filepath.Join(aliasesDir, entry.Name()))
					l.Release()
				}
			}
			deleteStaleLocks(aliasesDir)
		}

		openDir := ctx.Store.Path("open")
		if entries, err := os.ReadDir(openDir); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() || isInstalled(refs, entry.Name()) {
					continue
				}
				_ = store.DeleteTree(filepath.Join(openDir, entry.Name()))
			}
		}

		if vids, err := ctx.Store.ListVersionIds(); err == nil {
			for _, id := range vids {
				if !isInstalled(refs, id) {
					_ = ctx.Store.ForgetVersions(id)
				}
			}
		}

		var ids []string
		for _, r := range refs {
			ids = append(ids, r.ID)
		}
		_ = services.Prune(ctx.Store, ids)
		_ = roots.Prune(ctx.Store)
	}

	// Reporting
	type partInfo struct {
		t        tally
		selected bool
		one      string
		many     string
	}
	parts := []partInfo{
		{blobs, true, "blob", "blobs"},
		{deployments, true, "deployment", "deployments"},
		{tmp, true, "temporary item", "temporary items"},
		{downloads, opts.Downloads, "cached download", "cached downloads"},
		{toolCaches, opts.Downloads, "build tool cache", "build tool caches"},
		{work, opts.Downloads, "project's work directories", "projects' work directories"},
		{data, opts.Data, "data directory", "data directories"},
	}

	var removed []string
	var freed uint64
	for _, p := range parts {
		if !p.selected || p.t.count == 0 {
			continue
		}
		desc := p.many
		if p.t.count == 1 {
			desc = p.one
		}
		removed = append(removed, fmt.Sprintf("%d %s (%s)", p.t.count, desc, formatBytes(p.t.bytes)))
		freed += p.t.bytes
	}

	verb := "removed"
	if opts.DryRun {
		verb = "would remove"
	}
	if len(removed) == 0 {
		context.Note("nothing to remove")
	} else {
		context.Note("%s %s", verb, strings.Join(removed, ", "))
		freeVerb := "freed"
		if opts.DryRun {
			freeVerb = "would free"
		}
		context.Note("%s %s", freeVerb, formatBytes(freed))
	}

	if deploymentsInUse > 0 {
		context.Note("kept %d unused deployment(s) that a running app still uses; prune again once it exits", deploymentsInUse)
	}
	if tmpInUse > 0 {
		context.Note("kept the data of %d running --ephemeral app(s)", tmpInUse)
	}
	if !opts.Data && len(orphans) > 0 {
		context.Note("kept data of apps that aren't installed: %s; --data deletes it", strings.Join(orphans, ", "))
	}
	if !opts.Downloads && downloads.count > 0 {
		context.Note("kept %d cached download(s) (%s) for rebuilds; --downloads deletes them", downloads.count, formatBytes(downloads.bytes))
	}
	if !opts.Downloads && toolCaches.count > 0 {
		context.Note("kept the caches of %d build tool(s) (%s) for faster builds; --downloads deletes them", toolCaches.count, formatBytes(toolCaches.bytes))
	}
	if !opts.Downloads && work.count > 0 {
		context.Note("kept the work directories of %d project(s) (%s) for incremental builds; --downloads deletes them", work.count, formatBytes(work.bytes))
	}
	if !opts.Downloads && buildOnly > 0 {
		context.Note("kept %d image(s) that builds use as runtimes or SDKs; --downloads deletes them", buildOnly)
	}
	if keptPrevious && previousOnly.count > 0 {
		context.Note("kept %d previous version(s) (%s) for `zigsaw rollback`; --previous deletes them", previousOnly.count, formatBytes(previousOnly.bytes))
	}
	if !keptPrevious && len(previous) > 0 {
		forgetVerb := "forgot"
		if opts.DryRun {
			forgetVerb = "would forget"
		}
		context.Note("%s %d previous version(s): `zigsaw rollback` has nothing to go back to", forgetVerb, len(previous))
	}

	return nil
}
