package remote

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/store"
)

func pushSources(ctx *context.Context, reg *registry.Registry, cfg oci.AppConfig, digest, recipePath string) error {
	unique := map[string]bool{}
	// A local recipe distinguishes downloaded sources from local files/snapshots.
	// Without it, require every recorded source rather than silently omitting one.
	localRecipe := false
	if data, err := os.ReadFile(recipePath); err == nil {
		if r, err := recipe.Parse(recipePath, data); err == nil && r.ID == cfg.ID && r.Version == cfg.Version {
			localRecipe = true
			units, err := r.ReachableBuilds()
			if err != nil {
				return err
			}
			scopes := [][]recipe.Stage{r.Stages}
			for _, unit := range units {
				scopes = append(scopes, unit.Stages)
			}
			for _, stages := range scopes {
				for _, stage := range stages {
					for _, src := range stage.Sources {
						if src.URL != nil && src.Sha256 != nil {
							unique[*src.Sha256] = true
						}
					}
				}
			}
		}
	}
	if cfg.Build != nil {
		for _, hash := range cfg.Build.Sources {
			if !unique[hash] {
				unique[hash] = !localRecipe
			}
		}
		for _, hash := range cfg.Build.Vendor {
			unique[hash] = true
		}
	}
	hashes := make([]string, 0, len(unique))
	for hash := range unique {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	layers := []oci.Descriptor{}
	for _, hash := range hashes {
		if !oci.IsSha256Hex(hash) {
			return fmt.Errorf("invalid source hash %q", hash)
		}
		path := ctx.Store.Path("cache", "downloads", hash)
		h, err := store.Sha256File(path)
		// Local recipe files and directory snapshots are kept in source control,
		// rather than the download cache. Vendor archives must always be present.
		if os.IsNotExist(err) && !unique[hash] {
			continue
		}
		if err != nil {
			return fmt.Errorf("publishing source %s: %w", hash, err)
		}
		if h.Hex != hash {
			return fmt.Errorf("cached source %s has digest %s", hash, h.Hex)
		}
		desc := oci.Descriptor{MediaType: oci.MediaTypeSource, Digest: "sha256:" + hash, Size: h.Size}
		if err := UploadIfMissing(ctx.Store, reg, desc, path, "", ctx.Verbose); err != nil {
			return err
		}
		layers = append(layers, desc)
	}
	data, err := json.Marshal(oci.SourcesConfig{ID: cfg.ID, Version: cfg.Version, Image: digest})
	if err != nil {
		return err
	}
	config, err := ctx.Store.PutBlob(data, oci.MediaTypeSourcesConfig)
	if err != nil {
		return err
	}
	if err := UploadIfMissing(ctx.Store, reg, config, ctx.Store.BlobPath(oci.DigestHex(config.Digest)), "", ctx.Verbose); err != nil {
		return err
	}
	manifest, err := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: config, Layers: layers})
	if err != nil {
		return err
	}
	tag, err := oci.SourcesTag(digest)
	if err != nil {
		return err
	}
	if err := reg.PushManifest(tag, manifest); err != nil {
		return err
	}
	ctx.Note("pushed its %d source file(s) next to it, as %s", len(layers), tag)
	return nil
}

// FetchSource retrieves only the requested content-addressed source and checks
// its bytes before placing it in the download cache. It never trusts a tag as a hash.
func FetchSource(st *store.Store, envRegistry, image, hash string, verbose bool) (string, error) {
	if !oci.IsSha256Hex(hash) {
		return "", fmt.Errorf("invalid source hash %q", hash)
	}
	target, err := Resolve(envRegistry, image)
	if err != nil {
		return "", err
	}
	reg, err := registry.New(target.Ref, "pull", verbose)
	if err != nil {
		return "", err
	}
	resolved, err := ResolveImage(reg, target.Ref, "")
	if err != nil {
		return "", err
	}
	tag, err := oci.SourcesTag(resolved.Digest)
	if err != nil {
		return "", err
	}
	found, err := reg.FetchManifestRef(tag)
	if err != nil {
		return "", err
	}
	var manifest oci.Manifest
	if err := json.Unmarshal(found.Bytes, &manifest); err != nil {
		return "", err
	}
	if manifest.SchemaVersion != 2 || (manifest.Config.MediaType != oci.MediaTypeSourcesConfig && manifest.Config.MediaType != oci.LegacyMediaTypeSourcesConfig) {
		return "", fmt.Errorf("%s is not a source artifact", tag)
	}
	for _, desc := range manifest.Layers {
		if desc.Digest != "sha256:"+hash {
			continue
		}
		if desc.Size < 0 || (desc.MediaType != oci.MediaTypeSource && desc.MediaType != oci.LegacyMediaTypeSource) {
			return "", fmt.Errorf("invalid source descriptor for %s", hash)
		}
		tmp := st.TmpPath("source")
		defer os.Remove(tmp)
		got, err := reg.DownloadBlob(desc, tmp)
		if err != nil {
			return "", err
		}
		if got.Hex != hash {
			return "", fmt.Errorf("source digest mismatch: expected %s, got %s", hash, got.Hex)
		}
		path := st.Path("cache", "downloads", hash)
		if err := os.MkdirAll(st.Path("cache", "downloads"), 0755); err != nil {
			return "", err
		}
		if err := store.RenameRetrying(tmp, path, 5000); err != nil {
			return "", err
		}
		context.Note("fetching sha256 %.12s from the sources next to %s", hash, target.Ref.String())
		return path, nil
	}
	return "", fmt.Errorf("the sources next to %s don't have %s", target.Ref.String(), hash)
}
