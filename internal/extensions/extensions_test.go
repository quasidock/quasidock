package extensions

import (
	"reflect"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/oci"
)

func TestChoose(t *testing.T) {
	pts := []Point{{Name: "php", Version: "API1", Join: []string{"SCAN"}}}
	candidates := []Candidate{
		{ID: "php.Redis", Extends: oci.Extends{Point: "php", Version: "API1", Needs: []string{"php.Igbinary"}}},
		{ID: "php.Xdebug", Extends: oci.Extends{Point: "php", Version: "API1"}},
		{ID: "php.Old", Extends: oci.Extends{Point: "php", Version: "API0"}},
		{ID: "php.Igbinary", Extends: oci.Extends{Point: "php", Version: "API1"}},
		{ID: "gtk.Theme", Extends: oci.Extends{Point: "gtk", Version: "4"}},
		{ID: "php.APCu", Extends: oci.Extends{Point: "php", Version: "API1"}},
		{ID: "php.Msgpack", Extends: oci.Extends{Point: "php", Version: "API1", Needs: []string{"php.Missing"}}},
	}

	// Id order, Redis once Igbinary, which it needs, has joined. Msgpack needs one that isn't there; Old is for another version.
	c := Choose(pts, candidates, []string{"app"}, nil)
	wantChosen := []int{5, 3, 0, 1}
	if !reflect.DeepEqual(c.Chosen, wantChosen) {
		t.Errorf("c.Chosen = %v; want %v", c.Chosen, wantChosen)
	}
	if len(c.Skipped) != 2 {
		t.Fatalf("len(c.Skipped) = %d; want 2", len(c.Skipped))
	}
	if c.Skipped[0].ID != "php.Old" || c.Skipped[0].Reason != "it was built for php API0, and this is API1" {
		t.Errorf("c.Skipped[0] = %+v", c.Skipped[0])
	}
	if c.Skipped[1].ID != "php.Msgpack" || !c.Skipped[1].Warn {
		t.Errorf("c.Skipped[1] = %+v", c.Skipped[1])
	}

	// Left out: Igbinary, so Redis too. The app's own copy of Xdebug.
	c = Choose(pts, candidates, []string{"app", "PHP.xdebug"}, []string{"php.igbinary"})
	wantChosen = []int{5}
	if !reflect.DeepEqual(c.Chosen, wantChosen) {
		t.Errorf("c.Chosen = %v; want %v", c.Chosen, wantChosen)
	}
	if c.Skipped[1].ID != "php.Igbinary" || c.Skipped[1].Warn {
		t.Errorf("c.Skipped[1] = %+v", c.Skipped[1])
	}
	if c.Skipped[2].ID != "php.Msgpack" {
		t.Errorf("c.Skipped[2] = %+v", c.Skipped[2])
	}
	if c.Skipped[3].ID != "php.Redis" || c.Skipped[3].Reason != "it needs php.Igbinary, which isn't installed or doesn't join" {
		t.Errorf("c.Skipped[3] = %+v", c.Skipped[3])
	}

	// A runtime the app lists meets a need.
	c = Choose(pts, candidates[:1], []string{"php.Igbinary"}, nil)
	if !reflect.DeepEqual(c.Chosen, []int{0}) {
		t.Errorf("c.Chosen = %v; want [0]", c.Chosen)
	}

	// No point, no extensions.
	c = Choose(nil, candidates, nil, nil)
	if len(c.Chosen)+len(c.Skipped) != 0 {
		t.Errorf("expected 0, got %d", len(c.Chosen)+len(c.Skipped))
	}
}

func TestPoints(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	own := map[string]oci.ExtensionPoint{
		"p": {Version: "app's"},
	}
	rt := map[string]oci.ExtensionPoint{
		"p": {Version: "runtime's"},
		"q": {Version: "1", Join: []string{"A", "B"}},
	}
	cfg := oci.AppConfig{
		ID:         "app",
		Version:    "1",
		Command:    "x.exe",
		Extensions: own,
		Runtimes: map[string]oci.Runtime{
			"r": {
				ID:         "r",
				Version:    "1",
				Image:      digest,
				Layer:      digest,
				Extensions: rt,
			},
		},
	}

	pts := Points(cfg)
	if len(pts) != 2 {
		t.Fatalf("len(pts) = %d; want 2", len(pts))
	}
	if pts[0].Version != "app's" {
		t.Errorf("pts[0].Version = %q; want app's", pts[0].Version)
	}
	joined := Joined(pts)
	wantJoined := []string{"A", "B"}
	if !reflect.DeepEqual(joined, wantJoined) {
		t.Errorf("joined = %v; want %v", joined, wantJoined)
	}
}
