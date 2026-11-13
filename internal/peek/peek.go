package peek

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/estargz"
	"github.com/zigsaw-io/zigsaw/internal/format"
	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/registry"
	"github.com/zigsaw-io/zigsaw/internal/remote"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

type LayerInfo struct {
	Desc    oci.Descriptor
	ID      string
	Version string
	Format  format.Format
}

func getLayers(app remote.Info) []LayerInfo {
	layers := app.Manifest.Layers
	result := make([]LayerInfo, len(layers))
	runtimes := make([]oci.Runtime, 0, len(app.Config.Runtimes))
	for _, r := range app.Config.Runtimes {
		runtimes = append(runtimes, r)
	}

	for n := 0; n < len(layers); n++ {
		i := len(layers) - 1 - n
		desc := layers[i]
		id := app.Config.ID
		ver := app.Config.Version
		if i < len(runtimes) {
			id = runtimes[i].ID
			ver = runtimes[i].Version
		}
		fmtKind, ok := format.FormatOfDescriptor(desc)
		if !ok {
			fmtKind = format.FormatUnknown
		}
		result[n] = LayerInfo{
			Desc:    desc,
			ID:      id,
			Version: ver,
			Format:  fmtKind,
		}
	}
	return result
}

func List(st *store.Store, envRegistry, imageText string, verbose bool, w io.Writer) error {
	app, err := remote.FetchInfo(st, envRegistry, imageText, verbose)
	if err != nil {
		return err
	}

	reg, err := registry.New(app.Target.Ref, "pull", verbose)
	if err != nil {
		return err
	}

	all := getLayers(app)
	headings := len(all) > 1

	for _, l := range all {
		if !l.Format.Indexed() {
			if headings {
				fmt.Fprintf(w, "%s %s: %s, which can't be listed without pulling it\n", l.ID, l.Version, l.Format.String())
			} else {
				return fmt.Errorf("%s's layer is %s, which can't be listed without pulling it", app.Target.Text, l.Format.String())
			}
			continue
		}

		blob, err := remote.OpenRemoteBlob(st, reg, l.Desc)
		if err != nil {
			return err
		}
		defer blob.Close()

		sr := io.NewSectionReader(blob, 0, l.Desc.Size)
		reader, err := estargz.Open(sr)
		if err != nil {
			return err
		}

		type fileEntry struct {
			name string
			size int64
		}
		var files []fileEntry

		for _, e := range reader.AllEntries() {
			if e.Type == "reg" && estargz.IsLayerFile(e.Name) {
				files = append(files, fileEntry{name: e.Name, size: e.Size})
			}
		}

		sort.Slice(files, func(i, j int) bool {
			return files[i].name < files[j].name
		})

		if headings {
			fmt.Fprintf(w, "%s %s: %d files\n", l.ID, l.Version, len(files))
		}
		prefix := ""
		if headings {
			prefix = "  "
		}
		for _, f := range files {
			fmt.Fprintf(w, "%s%12d  %s\n", prefix, f.size, f.name)
		}
	}
	return nil
}

func Cat(st *store.Store, envRegistry, imageText, path string, verbose bool, w io.Writer) error {
	app, err := remote.FetchInfo(st, envRegistry, imageText, verbose)
	if err != nil {
		return err
	}

	reg, err := registry.New(app.Target.Ref, "pull", verbose)
	if err != nil {
		return err
	}

	normPath := strings.ReplaceAll(path, "\\", "/")
	all := getLayers(app)
	var skipped []string

	for _, l := range all {
		if !l.Format.Indexed() {
			skipped = append(skipped, fmt.Sprintf("%s's (%s)", l.ID, l.Format.String()))
			continue
		}

		blob, err := remote.OpenRemoteBlob(st, reg, l.Desc)
		if err != nil {
			return err
		}
		defer blob.Close()

		sr := io.NewSectionReader(blob, 0, l.Desc.Size)
		reader, err := estargz.Open(sr)
		if err != nil {
			return err
		}

		entry, ok := reader.Lookup(normPath)
		if !ok || !estargz.IsLayerFile(entry.Name) {
			continue
		}

		if entry.Type != "reg" {
			return fmt.Errorf("%s is a directory in %s", path, app.Target.Text)
		}

		rc, err := reader.OpenFile(entry.Name)
		if err != nil {
			return fmt.Errorf("reading %s from layer %s: %w", path, oci.ShortDigest(l.Desc.Digest), err)
		}

		_, err = io.Copy(w, rc)
		return err
	}

	if len(skipped) > 0 {
		return fmt.Errorf("%s has no %s in the layers zigsaw can read without pulling; %s can't be",
			app.Target.Text, path, oci.JoinAnd(skipped))
	}
	return fmt.Errorf("%s has no file %s", app.Target.Text, path)
}
