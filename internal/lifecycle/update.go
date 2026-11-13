package lifecycle

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/builder"
	"github.com/zigsaw-io/zigsaw/internal/context"
	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/registry"
	"github.com/zigsaw-io/zigsaw/internal/remote"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

type Waiting struct {
	ID         string
	From       string
	To         string
	FromDigest string
	ToDigest   string
	Added      string
}

func (w Waiting) Write(out io.Writer) error {
	_, err := fmt.Fprintf(out, "%s %s -> %s (%s -> %s)\n%s",
		w.ID, w.From, w.To,
		oci.ShortDigest(w.FromDigest), oci.ShortDigest(w.ToDigest),
		w.Added)
	return err
}

func PassesOver(skip *store.Skip, digest string) *store.Skip {
	if skip == nil {
		return nil
	}
	if skip.Manifest == digest {
		return skip
	}
	return nil
}

func Update(ctx *context.Context, ids []string, consent remote.Consent, lazily bool) (uint32, error) {
	refs, err := RefsOf(ctx, ids)
	if err != nil {
		return 1, err
	}
	failed := 0
	for _, ref := range refs {
		if err := updateApp(ctx, ref, consent, lazily); err != nil {
			failed++
		}
	}
	if failed == 0 {
		return 0, nil
	}
	ctx.Note("%d app(s) couldn't be updated", failed)
	return 1, nil
}

func Check(ctx *context.Context, ids []string) (uint32, error) {
	refs, err := RefsOf(ctx, ids)
	if err != nil {
		return 1, err
	}
	failed := 0
	for _, ref := range refs {
		app, err := NewImage(ctx, ref)
		if err != nil {
			failed++
			continue
		}
		if app != nil {
			_ = app.Write(os.Stdout)
		}
	}
	if failed == 0 {
		return 0, nil
	}
	ctx.Note("%d app(s) couldn't be checked", failed)
	return 1, nil
}

func RefsOf(ctx *context.Context, ids []string) ([]store.Ref, error) {
	if len(ids) == 0 {
		refs, err := ctx.Store.ListRefs()
		if err != nil {
			return nil, err
		}
		if len(refs) == 0 {
			ctx.Note("no apps are installed")
		}
		return refs, nil
	}
	var refs []store.Ref
	for _, id := range ids {
		ref, err := ctx.Store.ReadRef(id)
		if err != nil || ref == nil {
			return nil, fmt.Errorf("%s is not installed", id)
		}
		refs = append(refs, *ref)
	}
	return refs, nil
}

func isHeld(ctx *context.Context, ref store.Ref) bool {
	if ctx.Store.IsHeld(ref.ID) {
		ctx.Note("%s %s: held; `zigsaw unhold %s` lets updates in", ref.ID, ref.Version, ref.ID)
		return true
	}
	return false
}

func isSkipped(ctx *context.Context, ref store.Ref, digest string) bool {
	skip, err := ctx.Store.ReadSkip(ref.ID)
	if err == nil && skip != nil {
		if s := PassesOver(skip, digest); s != nil {
			ctx.Note("%s %s: skipping %s (%s), which it went back from", ref.ID, ref.Version, s.Version, oci.ShortDigest(s.Manifest))
			return true
		}
	}
	return false
}

func NewImage(ctx *context.Context, ref store.Ref) (*Waiting, error) {
	if isHeld(ctx, ref) {
		return nil, nil
	}
	if ref.Source == "" {
		ctx.Note("%s: doesn't record where it came from; build or pull it again to update it", ref.ID)
		return nil, nil
	}
	if filepath.IsAbs(ref.Source) {
		if !store.Exists(ref.Source) {
			return nil, fmt.Errorf("%s: its recipe %s is gone", ref.ID, ref.Source)
		}
		ctx.Note("%s %s: built from %s, so only rebuilding it tells", ref.ID, ref.Version, ref.Source)
		return nil, nil
	}
	parsedRef, err := registry.ParseReference(ref.Source)
	if err != nil {
		return nil, fmt.Errorf("%s: its source %q isn't a recipe path or an image reference", ref.ID, ref.Source)
	}
	if parsedRef.Digest != "" {
		ctx.Note("%s %s: pinned to %s", ref.ID, ref.Version, ref.Source)
		return nil, nil
	}

	envReg := ""
	if ctx != nil && ctx.Env != nil {
		envReg = ctx.Env["ZIGSAW_REGISTRY"]
	}
	info, err := remote.FetchInfo(ctx.Store, envReg, ref.Source, false)
	if err != nil {
		return nil, err
	}
	if info.ManifestDigest == ref.Manifest {
		ctx.Note("%s %s: up to date", ref.ID, ref.Version)
		return nil, nil
	}
	if isSkipped(ctx, ref, info.ManifestDigest) {
		return nil, nil
	}
	if !strings.EqualFold(info.Config.ID, ref.ID) {
		return nil, fmt.Errorf("%s: %s now provides %s; build or pull that explicitly", ref.ID, ref.Source, info.Config.ID)
	}

	installed, err := ctx.Store.ReadImage(ref.ID, ref.Manifest)
	addedStr := ""
	if err == nil && installed != nil {
		installedAccess := oci.AccessInfoOf(installed.Config)
		newAccess := oci.AccessInfoOf(*info.Config)
		added := oci.AddedAccess(installedAccess, newAccess)
		if !added.IsEmpty() {
			var sb strings.Builder
			remote.WriteAddedAccess(&sb, added)
			addedStr = sb.String()
		}
	}

	return &Waiting{
		ID:         ref.ID,
		From:       ref.Version,
		To:         info.Config.Version,
		FromDigest: ref.Manifest,
		ToDigest:   info.ManifestDigest,
		Added:      addedStr,
	}, nil
}

func updateApp(ctx *context.Context, ref store.Ref, consent remote.Consent, lazily bool) error {
	if isHeld(ctx, ref) {
		return nil
	}
	if ref.Source == "" {
		ctx.Note("%s: doesn't record where it came from; build or pull it again to update it", ref.ID)
		return nil
	}

	var newer *oci.Image
	if filepath.IsAbs(ref.Source) {
		if !store.Exists(ref.Source) {
			return fmt.Errorf("%s: its recipe %s is gone", ref.ID, ref.Source)
		}
		img, err := builder.Build(ctx, ref.Source, builder.Options{})
		if err != nil {
			return err
		}
		if img.ManifestDigest != ref.Manifest && isSkipped(ctx, ref, img.ManifestDigest) {
			return nil
		}
		if img.ManifestDigest != ref.Manifest {
			newer = img
		}
	} else {
		parsedRef, err := registry.ParseReference(ref.Source)
		if err != nil {
			return fmt.Errorf("%s: its source %q isn't a recipe path or an image reference", ref.ID, ref.Source)
		}
		if parsedRef.Digest != "" {
			ctx.Note("%s %s: pinned to %s", ref.ID, ref.Version, ref.Source)
			return nil
		}
		envReg := ""
		if ctx != nil && ctx.Env != nil {
			envReg = ctx.Env["ZIGSAW_REGISTRY"]
		}
		img, err := remote.Fetch(ctx.Store, envReg, ref.Source, ctx.Verbose)
		if err != nil {
			return err
		}
		if img.ManifestDigest != ref.Manifest && isSkipped(ctx, ref, img.ManifestDigest) {
			return nil
		}
		if img.ManifestDigest != ref.Manifest {
			newer = img
		}
	}

	if newer == nil {
		ctx.Note("%s %s: up to date", ref.ID, ref.Version)
		return nil
	}
	if !strings.EqualFold(newer.Config.ID, ref.ID) {
		return fmt.Errorf("%s: %s now provides %s; build or pull that explicitly", ref.ID, ref.Source, newer.Config.ID)
	}

	if err := Install(ctx, newer); err != nil {
		return err
	}
	ctx.Note("updated %s %s -> %s (%s -> %s)",
		ref.ID, ref.Version, newer.Config.Version,
		oci.ShortDigest(ref.Manifest), oci.ShortDigest(newer.ManifestDigest))
	return nil
}
