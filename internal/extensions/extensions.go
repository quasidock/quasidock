package extensions

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

type Point struct {
	Name    string
	Version string
	Join    []string
}

func Points(cfg oci.AppConfig) []Point {
	var out []Point
	var all []map[string]oci.ExtensionPoint
	if cfg.Extensions != nil {
		all = append(all, cfg.Extensions)
	}
	for _, r := range cfg.Runtimes {
		if r.Extensions != nil {
			all = append(all, r.Extensions)
		}
	}

	for _, e := range all {
		// Sort keys for stable iteration
		keys := make([]string, 0, len(e))
		for k := range e {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, name := range keys {
			p := e[name]
			known := false
			for _, o := range out {
				if strings.EqualFold(o.Name, name) {
					known = true
					break
				}
			}
			if !known {
				out = append(out, Point{Name: name, Version: p.Version, Join: p.Join})
			}
		}
	}
	return out
}

func Joined(pts []Point) []string {
	var out []string
	for _, p := range pts {
		out = append(out, p.Join...)
	}
	return out
}

type Candidate struct {
	ID      string
	Extends oci.Extends
}

type Skip struct {
	ID     string
	Reason string
	Warn   bool
}

type Choice struct {
	Chosen  []int
	Skipped []Skip
}

func Choose(pts []Point, candidates []Candidate, present []string, off []string) Choice {
	var skipped []Skip
	var pending []int

	for i, c := range candidates {
		var matchedPoint *Point
		for j := range pts {
			if strings.EqualFold(pts[j].Name, c.Extends.Point) {
				matchedPoint = &pts[j]
				break
			}
		}
		if matchedPoint == nil {
			continue
		}
		if contains(present, c.ID) {
			continue
		}
		if contains(off, c.ID) {
			skipped = append(skipped, Skip{ID: c.ID, Reason: "left out (--noextension)", Warn: false})
			continue
		}
		if c.Extends.Version != matchedPoint.Version {
			skipped = append(skipped, Skip{
				ID:     c.ID,
				Reason: fmt.Sprintf("it was built for %s %s, and this is %s", matchedPoint.Name, c.Extends.Version, matchedPoint.Version),
				Warn:   true,
			})
			continue
		}
		pending = append(pending, i)
	}

	sort.Slice(pending, func(a, b int) bool {
		return strings.ToLower(candidates[pending[a]].ID) < strings.ToLower(candidates[pending[b]].ID)
	})

	var chosen []int
	var chosenIDs []string

place:
	for {
		for k, i := range pending {
			ready := true
			for _, need := range candidates[i].Extends.Needs {
				if !contains(present, need) && !contains(chosenIDs, need) {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			chosen = append(chosen, i)
			chosenIDs = append(chosenIDs, candidates[i].ID)
			pending = append(pending[:k], pending[k+1:]...)
			continue place
		}
		break
	}

	for _, i := range pending {
		c := candidates[i]
		var missing []string
		for _, need := range c.Extends.Needs {
			if !contains(present, need) && !contains(chosenIDs, need) {
				missing = append(missing, need)
			}
		}
		skipped = append(skipped, Skip{
			ID:     c.ID,
			Reason: fmt.Sprintf("it needs %s, which isn't installed or doesn't join", oci.JoinAnd(missing)),
			Warn:   true,
		})
	}

	return Choice{Chosen: chosen, Skipped: skipped}
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if strings.EqualFold(x, id) {
			return true
		}
	}
	return false
}

type Extension struct {
	ID      string
	Version string
	Dir     string
	Config  oci.AppConfig
}

type Found struct {
	Points  []Point
	Loaded  []Extension
	Skipped []Skip
}

func Find(st *store.Store, cfg oci.AppConfig, off []string) (*Found, error) {
	pts := Points(cfg)
	if len(pts) == 0 {
		return &Found{}, nil
	}

	var candidates []Candidate
	refs, err := st.ListRefs()
	if err != nil {
		return nil, err
	}

	for _, ref := range refs {
		if strings.EqualFold(ref.ID, cfg.ID) {
			continue
		}
		img, err := st.ReadImage(ref.ID, ref.Manifest)
		if err != nil {
			continue
		}
		if img.Config.Extends != nil {
			candidates = append(candidates, Candidate{ID: ref.ID, Extends: *img.Config.Extends})
		}
	}

	var present []string
	present = append(present, cfg.ID)
	for _, r := range cfg.Runtimes {
		present = append(present, r.ID)
	}

	choice := Choose(pts, candidates, present, off)
	loaded := make([]Extension, len(choice.Chosen))
	for idx, i := range choice.Chosen {
		img, err := st.LoadImage(candidates[i].ID)
		if err != nil {
			return nil, err
		}
		loaded[idx] = Extension{
			ID:      img.Config.ID,
			Version: img.Config.Version,
			Dir:     img.DeployDir,
			Config:  img.Config,
		}
	}

	return &Found{
		Points:  pts,
		Loaded:  loaded,
		Skipped: choice.Skipped,
	}, nil
}
