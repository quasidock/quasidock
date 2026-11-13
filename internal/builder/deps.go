package builder

import (
	"fmt"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/context"
	"github.com/zigsaw-io/zigsaw/internal/lazy"
	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/remote"
)

type DependencyKind string

const (
	DependencyRuntime DependencyKind = "runtime"
	DependencySDK     DependencyKind = "sdk"
	DependencySource  DependencyKind = "source"
)

type Dependency struct {
	Alias          string
	ManifestDigest string
	Manifest       oci.Manifest
	Config         oci.AppConfig
}

func (d *Dependency) Layer() oci.Descriptor {
	return oci.OwnLayer(&d.Manifest)
}

func (d *Dependency) Runtime() oci.Runtime {
	var aliases map[string]oci.Export
	if len(d.Config.Aliases) > 0 {
		aliases = d.Config.Aliases
	}
	var extensions map[string]oci.ExtensionPoint
	if len(d.Config.Extensions) > 0 {
		extensions = d.Config.Extensions
	}

	return oci.Runtime{
		ID:         d.Config.ID,
		Version:    d.Config.Version,
		Image:      d.ManifestDigest,
		Layer:      d.Layer().Digest,
		Path:       d.Config.Path,
		Env:        d.Config.Env,
		Aliases:    aliases,
		Extensions: extensions,
		Uses:       d.Config.Permissions.Uses,
	}
}

func ResolveDependency(ctx *context.Context, recipePath string, kind DependencyKind, alias, reference string) (*Dependency, error) {
	envRegistry := ""
	if ctx != nil && ctx.Env != nil {
		envRegistry = ctx.Env["ZIGSAW_REGISTRY"]
	}

	target, err := remote.Resolve(envRegistry, reference)
	if err != nil {
		return nil, fmt.Errorf("%s: %s %s: %q isn't an app id or an image reference: %w", recipePath, kind, alias, reference, err)
	}

	if target.Ref.Digest == "" {
		return nil, failUnpinned(ctx, recipePath, kind, alias, reference, target)
	}
	digest := target.Ref.Digest

	var manifest oci.Manifest
	var config oci.AppConfig

	usable, err := ctx.Store.ReadUsableImage(digest)
	if err == nil && usable != nil {
		_ = lazy.EnsureServed(ctx.Store, usable.Manifest.Layers, ctx.Verbose)
		manifest = usable.Manifest
		config = usable.Config
	} else {
		fetched, err := remote.Fetch(ctx.Store, envRegistry, reference, ctx.Verbose)
		if err != nil {
			return nil, err
		}
		if fetched.ManifestDigest != digest {
			atIdx := strings.IndexByte(reference, '@')
			refPrefix := reference
			if atIdx >= 0 {
				refPrefix = reference[:atIdx]
			}
			return nil, fmt.Errorf("%s: %s %s: %s is a multi-platform index; pin its image for windows/amd64:\n  %q@%s",
				recipePath, kind, alias, digest, refPrefix, fetched.ManifestDigest)
		}
		manifest = fetched.Manifest
		config = fetched.Config
	}

	if target.AppID != nil && !strings.EqualFold(*target.AppID, config.ID) {
		return nil, fmt.Errorf("%s: %s %s: %s is %s, not %s", recipePath, kind, alias, digest, config.ID, *target.AppID)
	}
	if kind == DependencyRuntime && len(config.Runtimes) > 0 {
		return nil, fmt.Errorf("%s: %s %s is %s, which has runtimes of its own; zigsaw doesn't support that", recipePath, kind, alias, config.ID)
	}

	_ = ctx.Store.MarkBuildImage(digest)
	return &Dependency{
		Alias:          alias,
		ManifestDigest: digest,
		Manifest:       manifest,
		Config:         config,
	}, nil
}

func failUnpinned(ctx *context.Context, recipePath string, kind DependencyKind, alias, reference string, target remote.Target) error {
	envRegistry := ""
	if ctx != nil && ctx.Env != nil {
		envRegistry = ctx.Env["ZIGSAW_REGISTRY"]
	}

	digest := ""
	if target.AppID != nil {
		if ref, err := ctx.Store.ReadRef(*target.AppID); err == nil && ref != nil {
			if target.Ref.Tag == "" || target.Ref.Tag == ref.Version {
				digest = ref.Manifest
			}
		}
	}
	if digest == "" {
		info, err := remote.FetchInfo(ctx.Store, envRegistry, reference, false)
		if err == nil && info.ManifestDigest != "" {
			digest = info.ManifestDigest
		}
	}

	key := string(alias)
	if kind == DependencySource {
		key = "image"
	}
	if digest == "" {
		return fmt.Errorf("%s: %s %s %q must be pinned with \"@sha256:...\", and zigsaw can't find the digest: build or pull the image first", recipePath, kind, alias, reference)
	}
	return fmt.Errorf("%s: %s %s isn't pinned to a digest. Pin it with:\n  %q: %q@%s", recipePath, kind, alias, key, reference, digest)
}
