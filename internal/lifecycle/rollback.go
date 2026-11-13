package lifecycle

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/context"
	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/registry"
	"github.com/zigsaw-io/zigsaw/internal/remote"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

func Rollback(ctx *context.Context, id string, consent remote.Consent) error {
	st := ctx.Store
	current, err := st.ReadRef(id)
	if err != nil || current == nil {
		return fmt.Errorf("%s is not installed", id)
	}

	previous, err := st.ReadPrevious(current.ID)
	if err != nil || previous == nil {
		return fmt.Errorf("%s %s: nothing to go back to; zigsaw keeps the version an update replaces from 0.76 on", current.ID, current.Version)
	}

	if previous.Manifest == current.Manifest {
		return fmt.Errorf("%s %s: nothing to go back to; its previous version is this one", current.ID, current.Version)
	}

	loaded, err := st.ReadCompleteImage(previous.Manifest)
	if err != nil || loaded == nil {
		return fmt.Errorf("%s: the files of %s (%s) are gone, as `zigsaw prune --previous` or a zigsaw before 0.76 deletes them. %s",
			current.ID, previous.Version, oci.ShortDigest(previous.Manifest), howToGetBack(*previous))
	}

	if !strings.EqualFold(loaded.Config.ID, current.ID) {
		return fmt.Errorf("%s: its previous version is an image of %s", current.ID, loaded.Config.ID)
	}

	installed, err := st.ReadImage(current.ID, current.Manifest)
	if err == nil && installed != nil {
		installedAccess := oci.AccessInfoOf(installed.Config)
		if err := remote.AgreeTo(ctx, loaded.Manifest, loaded.Config, &remote.ToldAccess{
			Version: current.Version,
			Access:  installedAccess,
		}, consent, remote.KindRollback); err != nil {
			return err
		}
	}

	if err := Install(ctx, &oci.Image{
		ManifestDigest: previous.Manifest,
		Manifest:       loaded.Manifest,
		Config:         loaded.Config,
		Source:         previous.Source,
		Described:      true,
	}); err != nil {
		return err
	}

	if previous.Source == "" {
		_ = st.WriteRef(*previous)
	}
	_ = st.WriteSkip(current.ID, store.Skip{Version: current.Version, Manifest: current.Manifest})

	ctx.Note("went back: %s %s -> %s (%s -> %s)",
		current.ID, current.Version, previous.Version,
		oci.ShortDigest(current.Manifest), oci.ShortDigest(previous.Manifest))
	ctx.Note("  `zigsaw update` passes over %s from now on, and takes a newer version; `zigsaw rollback %s` returns to it", current.Version, current.ID)
	return nil
}

func howToGetBack(previous store.Ref) string {
	if previous.Source == "" {
		return "Nothing records where it came from."
	}
	if filepath.IsAbs(previous.Source) {
		return fmt.Sprintf("Rebuilding it from its recipe as it was then brings it back; the recipe was %s.", previous.Source)
	}
	ref, err := registry.ParseReference(previous.Source)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("`zigsaw pull %s/%s@%s` brings it back, while its registry has it.", ref.Host, ref.Repository, previous.Manifest)
}

func SetHeld(ctx *context.Context, ids []string, held bool) error {
	var refs []store.Ref
	for _, id := range ids {
		ref, err := ctx.Store.ReadRef(id)
		if err != nil || ref == nil {
			return fmt.Errorf("%s is not installed", id)
		}
		refs = append(refs, *ref)
	}

	for _, ref := range refs {
		changed, err := ctx.Store.SetHeld(ref.ID, held)
		if err != nil {
			return err
		}
		if held {
			if changed {
				ctx.Note("held %s at %s: updates leave it alone until `zigsaw unhold %s`", ref.ID, ref.Version, ref.ID)
			} else {
				ctx.Note("%s was held already", ref.ID)
			}
		} else {
			if changed {
				ctx.Note("%s %s: updates reach it again", ref.ID, ref.Version)
			} else {
				ctx.Note("%s wasn't held", ref.ID)
			}
		}
	}
	return nil
}
