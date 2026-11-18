package builder

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/tree"
)

// A scope owns its drive, dependency leases and input handles until its output
// has been packaged or copied to an independent unit snapshot.
type scopeResult struct {
	tree     *tree.Tree
	from     map[string]oci.Runtime
	layers   []oci.Descriptor
	runtimes []oci.DirPlaceholder
	info     *oci.Build
	root     *BuildRoot
	sources  *Sources
	releases []func()
}

func (s *scopeResult) close(ctx *context.Context, keep bool) {
	if s.root != nil {
		s.root.Finish(ctx, keep)
		s.root = nil
	}
	if s.sources != nil {
		s.sources.Close()
		s.sources = nil
	}
	for i := len(s.releases) - 1; i >= 0; i-- {
		s.releases[i]()
	}
	s.releases = nil
}

func executeScope(ctx *context.Context, r *recipe.Recipe, recipePath string, projects *Snapshots, work *Work, opts Options) (*scopeResult, error) {
	building := r.HasBuildCommands()
	result := &scopeResult{}
	complete := false
	defer func() {
		if !complete {
			result.close(ctx, opts.KeepBuildDir)
		}
	}()
	tools := &Tools{recipePath: recipePath}
	buildInfo := &oci.Build{
		SDK:    make(map[string]string),
		Vendor: make(map[string]string),
		Host:   make(map[string]string),
	}

	for _, alias := range slices.Sorted(maps.Keys(r.SDK)) {
		reference := r.SDK[alias]
		dep, err := ResolveDependency(ctx, recipePath, DependencySDK, alias, reference)
		if err != nil {
			return nil, err
		}
		buildInfo.SDK[alias] = dep.ManifestDigest
		if !building {
			continue
		}
		dirs, locks, err := ctx.Store.UseLayers(dep.Manifest.Layers)
		if err != nil {
			return nil, err
		}
		result.releases = append(result.releases, func() {
			for _, lock := range locks {
				lock.Release()
			}
		})
		what := fmt.Sprintf("sdk %s", alias)
		ownRuntimes := runtimeDirs(dep.Config, dirs[:len(dirs)-1])
		sdkIdx, err := tools.Add(Tool{
			Image:    dep.ManifestDigest,
			ID:       dep.Config.ID,
			Version:  dep.Config.Version,
			What:     what,
			Dir:      dirs[len(dirs)-1],
			Path:     dep.Config.Path,
			Env:      dep.Config.Env,
			Runtimes: ownRuntimes,
		})
		if err != nil {
			return nil, err
		}
		if sdkIdx == nil {
			continue
		}
		for _, named := range ownRuntimes {
			rtAlias, rt := named.Alias, dep.Config.From[named.Alias]
			rtWhat := fmt.Sprintf("%s's base image %s", what, rtAlias)
			added, err := tools.Add(Tool{
				Image:   rt.Image,
				ID:      rt.ID,
				Version: rt.Version,
				What:    rtWhat,
				Dir:     named.Path,
				Path:    rt.Path,
				Env:     rt.Env,
			})
			if err != nil {
				return nil, err
			}
			if added != nil {
				tools.list[*sdkIdx].Brings++
			}
		}
	}

	from := make(map[string]oci.Runtime)
	var layers []oci.Descriptor
	var runtimeDirsList []oci.DirPlaceholder

	for _, alias := range slices.Sorted(maps.Keys(r.From)) {
		reference := r.From[alias]
		dep, err := ResolveDependency(ctx, recipePath, DependencyRuntime, alias, reference)
		if err != nil {
			return nil, err
		}
		dirs, locks, err := ctx.Store.UseLayers([]oci.Descriptor{dep.Layer()})
		if err != nil {
			return nil, err
		}
		result.releases = append(result.releases, func() {
			for _, lock := range locks {
				lock.Release()
			}
		})
		from[alias] = dep.Runtime()
		layers = append(layers, dep.Layer())
		runtimeDirsList = append(runtimeDirsList, oci.DirPlaceholder{Alias: alias, Path: dirs[0]})
		_, addErr := tools.Add(Tool{
			Image:   dep.ManifestDigest,
			ID:      dep.Config.ID,
			Version: dep.Config.Version,
			What:    fmt.Sprintf("base image %s", alias),
			Dir:     dirs[0],
			Path:    dep.Config.Path,
			Env:     dep.Config.Env,
		})
		if addErr != nil {
			return nil, addErr
		}
	}

	for _, m := range r.Stages {
		if m.Network {
			buildInfo.Network = true
		}
	}

	sources := NewSources(ctx, filepath.Dir(recipePath), r.ID+":"+r.Version, projects, r.SourceMirrors...)
	result.sources = sources

	var appTree *tree.Tree
	var root *BuildRoot

	var downloads []recipe.Source
	for _, m := range r.Stages {
		for i, src := range m.Sources {
			if src.Image == nil && src.Build == nil && projects.Get(m.Name, i) == nil {
				downloads = append(downloads, src)
			}
		}
	}
	if err := sources.fetcher.FetchAll(downloads); err != nil {
		return nil, err
	}

	if building {
		br, err := initBuildRoot(ctx, r, tools.list)
		if err != nil {
			return nil, err
		}
		root = br
		result.root = br
		root.work = work
		if root.msvc != nil {
			buildInfo.Host["msvc"] = root.msvc.ToolsVersion
			buildInfo.Host["windows-sdk"] = root.msvc.SDKVersion
		}

		for _, m := range r.Stages {
			if m.Vendor != nil {
				sha, err := root.vendor(ctx, m, recipePath, projects, r.ID+":"+r.Version)
				if err != nil {
					return nil, err
				}
				buildInfo.Vendor[m.Name] = sha
			}
		}

		t, err := root.buildStages(ctx, sources, r, recipePath)
		if err != nil {
			return nil, err
		}
		appTree = t
	} else {
		appTree = tree.New()
		for _, m := range r.Stages {
			if err := sources.Add(appTree, m, recipePath); err != nil {
				return nil, err
			}
		}
	}

	// Vendor stages unpack through their own Sources instance. Include their
	// original downloads as well as the files seen by the final layer builder.
	sourceHashes := map[string]bool{}
	for _, hash := range sources.hashes {
		sourceHashes[hash] = true
	}
	for _, src := range downloads {
		if src.URL != nil && src.Sha256 != nil {
			sourceHashes[*src.Sha256] = true
		}
	}
	buildInfo.Sources = slices.Sorted(maps.Keys(sourceHashes))
	if len(sources.images) > 0 {
		buildInfo.Images = sources.images
	}
	appTree.Cleanup(r.Cleanup)
	if err := projects.Recheck(ctx, ""); err != nil {
		return nil, err
	}
	result.tree, result.from, result.layers = appTree, from, layers
	result.runtimes, result.info = runtimeDirsList, buildInfo
	complete = true
	return result, nil
}
