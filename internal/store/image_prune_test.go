package store

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestImageCacheDates(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ref := catalogFixture(t, st, "1")
	ref.CachedAt = "1900-01-01T00:00:00Z"
	start := time.Now().UTC()
	if err := st.RegisterImage(ref, ref.Source, true); err != nil {
		t.Fatal(err)
	}
	get := func() Ref {
		t.Helper()
		idx, err := st.ImageCatalog()
		if err != nil {
			t.Fatal(err)
		}
		return idx.Images[ref.Manifest]
	}
	first := get().CachedAt
	if date, err := time.Parse(time.RFC3339Nano, first); err != nil || date.Before(start) {
		t.Fatalf("cache date did not use local registration: %q %v", first, err)
	}
	if err := st.TagImage("", ref.Manifest, "example.test/app:alias"); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterImage(ref, ref.Source, true); err != nil {
		t.Fatal(err)
	}
	if err := st.ImportImages(ImageImport{Images: map[string]Ref{ref.Manifest: ref}}, false); err != nil {
		t.Fatal(err)
	}
	if get().CachedAt != first {
		t.Fatal("tag, registration or import reset the original cache date")
	}
	// Old catalogs stay readable and undated; tagging alone is not recaching.
	if err := st.changeImages(func(idx *ImageIndex) error {
		r := idx.Images[ref.Manifest]
		r.CachedAt = ""
		idx.Images[ref.Manifest] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(st.Path("images", "index.json"))
	plan, err := st.PlanImagePrune(ImagePruneOptions{All: true, Until: []time.Time{time.Now().Add(time.Hour)}})
	if err != nil || len(plan.Images) != 0 || get().CachedAt != "" {
		t.Fatalf("legacy unknown age: %+v %v", plan, err)
	}
	after, _ := os.ReadFile(st.Path("images", "index.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("reading an old catalog rewrote it")
	}
	if err := st.TagImage("", ref.Manifest, "example.test/app:another"); err != nil || get().CachedAt != "" {
		t.Fatal("tag assigned a cache date", err)
	}
	if err := st.ImportImages(ImageImport{Images: map[string]Ref{ref.Manifest: ref}}, false); err != nil || get().CachedAt == "" {
		t.Fatal("recaching an old image did not assign a date", err)
	}
	if err := st.changeImages(func(idx *ImageIndex) error {
		r := idx.Images[ref.Manifest]
		r.CachedAt = ref.CachedAt
		idx.Images[ref.Manifest] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RemoveImage("", ref.Manifest); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterImage(ref, ref.Source, true); err != nil || get().CachedAt == ref.CachedAt {
		t.Fatal("removed image reused its prior cache date", err)
	}
	imported := catalogFixture(t, st, "2")
	imported.CachedAt = ref.CachedAt
	if err := st.ImportImages(ImageImport{Images: map[string]Ref{imported.Manifest: imported}}, false); err != nil {
		t.Fatal(err)
	}
	idx, _ := st.ImageCatalog()
	if date := idx.Images[imported.Manifest].CachedAt; date == "" || date == imported.CachedAt {
		t.Fatalf("new import trusted a foreign cache date: %q", date)
	}
}

func TestImagePruneProtectsRawContainersAndLeases(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	var refs []Ref
	for _, version := range []string{"old", "default", "named", "leased", "created", "exited", "paused", "running", "stale-autorm"} {
		ref := catalogFixture(t, st, version)
		name := ""
		if version == "named" {
			name = ref.Source
		}
		if err := st.RegisterImage(ref, name, version == "default"); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	if err := st.TagImage("", refs[2].Manifest, "example.test/app:second"); err != nil {
		t.Fatal(err)
	}
	var containers []*ContainerRecord
	for _, ref := range refs[4:] {
		c, err := st.CreateContainer("pin-"+ref.Version, ContainerRecord{Image: ref.Manifest, AppID: ref.ID})
		if err != nil {
			t.Fatal(err)
		}
		containers = append(containers, c)
	}
	states := []ContainerState{
		{Status: "created"}, {Status: "exited"},
		{Status: "running", Running: true, Paused: true, Launched: true},
		{Status: "running", Running: true, Launched: true},
		{Status: "running", Running: true, Launched: true},
	}
	before := map[string][]byte{}
	for i, c := range containers {
		c.State = states[i]
		c.HostConfig.AutoRemove = i == len(containers)-1
		if err := st.writeContainerStateNoLock(c); err != nil {
			t.Fatal(err)
		}
		before[c.ID], _ = os.ReadFile(st.ContainerStatePath(c.ID))
	}
	events, _ := os.ReadFile(st.Path("events.json"))
	app, image, err := st.BeginImageRun(refs[3])
	if err != nil {
		t.Fatal(err)
	}
	defer app.Release()
	defer image.Release()
	plain, err := st.PlanImagePrune(ImagePruneOptions{})
	if err != nil || len(plain.Images) != 1 || plain.Images[0].Manifest != refs[0].Manifest {
		t.Fatalf("ordinary prune: %+v %v", plain, err)
	}
	all, err := st.PlanImagePrune(ImagePruneOptions{All: true})
	if err != nil || len(all.Images) != 3 || len(all.Names) != 2 {
		t.Fatalf("all prune: %+v %v", all, err)
	}
	if err := st.ApplyImagePrune(all); err != nil {
		t.Fatal(err)
	}
	idx, _ := st.ImageCatalog()
	if len(idx.Images) != len(refs)-3 || len(idx.Names) != 0 || len(idx.Defaults) != 0 {
		t.Fatalf("prune left aliases/defaults or removed a pin: %+v", idx)
	}
	for id, data := range before {
		after, err := os.ReadFile(st.ContainerStatePath(id))
		if err != nil || !bytes.Equal(data, after) {
			t.Fatalf("prune reconciled container %s: %v", id, err)
		}
	}
	afterEvents, _ := os.ReadFile(st.Path("events.json"))
	if !bytes.Equal(events, afterEvents) {
		t.Fatal("prune changed container events")
	}
}

func TestImagePruneFilters(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts ImagePruneOptions
		want bool
	}{
		{"none", ImagePruneOptions{}, true},
		{"presence", ImagePruneOptions{Labels: []ImageLabelFilter{{Key: "empty"}}}, true},
		{"empty value", ImagePruneOptions{Labels: []ImageLabelFilter{{Key: "empty", HasValue: true}}}, true},
		{"missing empty", ImagePruneOptions{Labels: []ImageLabelFilter{{Key: "missing", HasValue: true}}}, false},
		{"or", ImagePruneOptions{Labels: []ImageLabelFilter{{Key: "missing"}, {Key: "team", Value: "a=b", HasValue: true}}}, true},
		{"negative presence", ImagePruneOptions{NotLabels: []ImageLabelFilter{{Key: "team"}}}, false},
		{"negative value", ImagePruneOptions{NotLabels: []ImageLabelFilter{{Key: "team", Value: "other", HasValue: true}}}, true},
		{"negative or", ImagePruneOptions{NotLabels: []ImageLabelFilter{{Key: "team"}, {Key: "missing"}}}, true},
		{"and", ImagePruneOptions{Labels: []ImageLabelFilter{{Key: "team"}}, NotLabels: []ImageLabelFilter{{Key: "empty"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.matchesLabels(map[string]string{"team": "a=b", "empty": ""}); got != tc.want {
				t.Fatalf("got %t, want %t", got, tc.want)
			}
		})
	}
	cutoff := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	opts := ImagePruneOptions{Until: []time.Time{cutoff.Add(-time.Hour), cutoff}}
	for _, tc := range []struct {
		cached string
		want   bool
	}{{"", false}, {"invalid", false}, {cutoff.Format(time.RFC3339Nano), false},
		{cutoff.Add(-time.Nanosecond).Format(time.RFC3339Nano), true}, {cutoff.Add(time.Nanosecond).Format(time.RFC3339Nano), false}} {
		if got := opts.matchesAge(Ref{CachedAt: tc.cached}); got != tc.want {
			t.Errorf("cache date %q: %t, want %t", tc.cached, got, tc.want)
		}
	}
}

func TestImagePruneRejectsChangedPlan(t *testing.T) {
	for _, change := range []string{"catalog", "container", "lease", "corrupt container"} {
		t.Run(change, func(t *testing.T) {
			st, _ := Open(t.TempDir(), false, "")
			ref := catalogFixture(t, st, "1")
			if err := st.RegisterImage(ref, ref.Source, true); err != nil {
				t.Fatal(err)
			}
			plan, err := st.PlanImagePrune(ImagePruneOptions{All: true})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "catalog":
				err = st.TagImage("", ref.Manifest, "example.test/app:new")
			case "container", "corrupt container":
				var c *ContainerRecord
				c, err = st.CreateContainer("new", ContainerRecord{Image: ref.Manifest, AppID: ref.ID})
				if err == nil && change == "corrupt container" {
					err = os.WriteFile(st.ContainerStatePath(c.ID), []byte("{"), 0o644)
				}
			case "lease":
				var a, b *Lock
				a, b, err = st.BeginImageRun(ref)
				if err == nil {
					defer a.Release()
					defer b.Release()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(st.Path("images", "index.json"))
			if err := st.ApplyImagePrune(plan); err == nil {
				t.Fatal("applied invalidated prune plan")
			}
			after, _ := os.ReadFile(st.Path("images", "index.json"))
			if !reflect.DeepEqual(before, after) || !strings.Contains(string(after), ref.Manifest) {
				t.Fatal("failed apply changed catalog")
			}
		})
	}
}
