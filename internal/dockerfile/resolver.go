package dockerfile

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/remote"
)

type ImageResolver struct{ Context *context.Context }

func versionMatches(version, prefix string) bool {
	return prefix == "" || version == prefix || strings.HasPrefix(version, prefix+".")
}
func newer(a, b string) bool {
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(aa) {
			x, _ = strconv.Atoi(aa[i])
		}
		if i < len(bb) {
			y, _ = strconv.Atoi(bb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func (r ImageResolver) Resolve(image, version, override string) (Dependency, error) {
	ctx := r.Context
	reg := ctx.Env[remote.RegistryVar]
	check := func(ref string, config oci.AppConfig) (Dependency, error) {
		if config.ID != image || len(config.From) > 0 {
			return Dependency{}, fmt.Errorf("%s must be a standalone %s image", ref, image)
		}
		if override == "" && !versionMatches(config.Version, version) {
			return Dependency{}, fmt.Errorf("%s provides %s, not requested %s; select a replacement with --base or --stage-base", ref, config.Version, version)
		}
		return Dependency{ref, config}, nil
	}
	if override == "" && version != "" {
		idx, err := ctx.Store.ImageCatalog()
		if err != nil {
			return Dependency{}, err
		}
		var candidates []Dependency
		for _, ref := range idx.Images {
			if ref.ID != image || !versionMatches(ref.Version, version) {
				continue
			}
			img, err := ctx.Store.ReadImage(ref.ID, ref.Manifest)
			if err != nil {
				continue
			}
			target, err := remote.Resolve(reg, image)
			if err != nil {
				return Dependency{}, err
			}
			candidates = append(candidates, Dependency{target.Ref.Host + "/" + target.Ref.Repository + "@" + ref.Manifest, img.Config})
		}
		if len(candidates) != 0 {
			sort.Slice(candidates, func(i, j int) bool { return newer(candidates[i].Config.Version, candidates[j].Config.Version) })
			return check(candidates[0].Reference, candidates[0].Config)
		}
	}
	text := override
	if text == "" {
		text = image
		if version != "" {
			target, err := remote.Resolve(reg, image)
			if err != nil {
				return Dependency{}, err
			}
			client, err := registry.New(target.Ref, "pull", ctx.Verbose)
			if err != nil {
				return Dependency{}, err
			}
			tags, err := client.ListTags()
			if err != nil {
				return Dependency{}, fmt.Errorf("finding %s %s: %w; select a pinned image with --base or --stage-base", image, version, err)
			}
			var matches []string
			for _, tag := range tags {
				if numericTag.MatchString(tag) && !strings.Contains(tag, "-") && versionMatches(tag, version) {
					matches = append(matches, tag)
				}
			}
			if len(matches) == 0 {
				return Dependency{}, fmt.Errorf("no native %s version matching %s; select a replacement with --base or --stage-base", image, version)
			}
			sort.Slice(matches, func(i, j int) bool { return newer(matches[i], matches[j]) })
			text += ":" + matches[0]
		}
	}
	// Explicit local selections avoid a network lookup when a pinned image is cached.
	if ref, err := ctx.Store.ResolveLocalImage(reg, text); err == nil {
		img, err := ctx.Store.ReadImage(ref.ID, ref.Manifest)
		if err != nil {
			return Dependency{}, err
		}
		target, err := remote.Resolve(reg, text)
		if err != nil {
			return Dependency{}, err
		}
		return check(target.Ref.Host+"/"+target.Ref.Repository+"@"+ref.Manifest, img.Config)
	}
	info, err := remote.FetchInfo(ctx.Store, reg, text, ctx.Verbose)
	if err != nil {
		return Dependency{}, err
	}
	return check(info.Target.Ref.Host+"/"+info.Target.Ref.Repository+"@"+info.ManifestDigest, *info.Config)
}
