package publishing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleRecipe(t *testing.T, id string, pins ...string) Recipe {
	t.Helper()
	sdk := map[string]string{}
	for _, pin := range pins {
		sdk[strings.Split(pin, ":")[0]] = pin
	}
	data, _ := json.Marshal(map[string]any{"id": id, "version": "1", "sdk": sdk})
	r, err := Parse(id+".json", data)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func sampleReport(r Recipe, digest string) Report {
	return Report{Recipe: r.File, ID: r.ID, Version: r.Version, Digest: digest, Commit: strings.Repeat("c", 40), Tag: "ci-1", Registry: Registry, RecipeJSON: r.Bytes, Pins: r.Pins}
}

func TestPublishRefreshesPinsFromSameRun(t *testing.T) {
	old, newDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	dep := sampleRecipe(t, "go")
	app := sampleRecipe(t, "fzf", "go:1@"+old)
	reports := map[string]Report{"go": sampleReport(dep, newDigest)}
	before := string(app.Bytes)
	prepared, err := Prepare(app, reports, strings.Repeat("c", 40), "ci-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prepared), newDigest) || string(app.Bytes) != before {
		t.Fatal("publication did not update a copy of the pin")
	}
	preparedApp, err := Parse(app.File, prepared)
	if err != nil {
		t.Fatal(err)
	}
	complete := map[string]Report{"go": reports["go"], "fzf": sampleReport(preparedApp, old)}
	if err := Validate([]Recipe{dep, app}, complete, strings.Repeat("c", 40), "ci-1"); err != nil {
		t.Fatalf("freshly prepared pins cannot be promoted in the same run: %v", err)
	}
	app, err = Parse(app.File, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Prepare(app, reports, strings.Repeat("c", 40), "ci-1"); err != nil || string(got) != string(prepared) {
		t.Fatalf("publish changed prepared recipe: %v", err)
	}
	for _, change := range []func(*Report){func(r *Report) { r.ID = "other" }, func(r *Report) { r.Commit = "other" }, func(r *Report) { r.Tag = "ci-2" }, func(r *Report) { r.Registry = "ghcr.io/other" }, func(r *Report) { r.Digest = "invalid" }} {
		rp := reports["go"]
		change(&rp)
		if _, err := Prepare(app, map[string]Report{"go": rp}, strings.Repeat("c", 40), "ci-1"); err == nil {
			t.Fatal("accepted foreign or malformed dependency result")
		}
	}
	if _, err := Prepare(app, nil, strings.Repeat("c", 40), "ci-1"); err == nil {
		t.Fatal("accepted missing dependency")
	}
}
func TestOrderAndCycle(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	root := sampleRecipe(t, "go")
	app := sampleRecipe(t, "fzf", "go:1@"+digest)
	ordered, err := Order([]Recipe{app, root})
	if err != nil || ordered[0].ID != "go" {
		t.Fatalf("dependency order: %v %v", ordered, err)
	}
	root = sampleRecipe(t, "go", "fzf:1@"+digest)
	if _, err := Order([]Recipe{root, app}); err == nil {
		t.Fatal("accepted cyclic dependencies")
	}
}
func TestReportRoundTripAndCompleteness(t *testing.T) {
	r := sampleRecipe(t, "go")
	r.Bytes = append(r.Bytes, '\n')
	rp := sampleReport(r, "sha256:"+strings.Repeat("a", 64))
	dir := t.TempDir()
	data, _ := json.MarshalIndent(rp, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "go.report.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	reports, err := ReadReports(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate([]Recipe{r}, reports, rp.Commit, rp.Tag); err != nil {
		t.Fatal(err)
	}
	if string(reports["go"].RecipeJSON) != string(r.Bytes) {
		t.Fatal("report serialization changed recipe bytes")
	}
	if err := Validate([]Recipe{r}, nil, rp.Commit, rp.Tag); err == nil {
		t.Fatal("accepted incomplete reports")
	}
	if err := os.WriteFile(filepath.Join(dir, "duplicate.report.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReports(dir); err == nil {
		t.Fatal("accepted duplicate reports")
	}
}

func TestWorkflowAlwaysVerifiesAndPromotes(t *testing.T) {
	workflow := Workflow([]Recipe{sampleRecipe(t, "go")})
	for _, removed := range []string{"inputs.mode", "PUBLISH_MODE", "default: bootstrap"} {
		if strings.Contains(workflow, removed) {
			t.Fatalf("publication still depends on a mode: %s", removed)
		}
	}
	previous := -1
	for _, step := range []string{"Collect verified results and pin patch", "Publish candidate catalog", "Verify anonymous access before promotion", "Promote exact verified digests; catalog last", "Verify final tags, pins, and searchable catalog anonymously"} {
		at := strings.Index(workflow, "      - name: "+step+"\n        shell: bash\n")
		if at <= previous {
			t.Fatalf("step %q is missing, conditional, or out of order", step)
		}
		previous = at
	}
	if !strings.Contains(workflow, "scripts/check-pins.sh out/publishing/recipes/*.json") {
		t.Fatal("release checks do not use prepared recipes")
	}
	reusable, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "publish-image.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(reusable), "inputs.mode") || strings.Contains(string(reusable), "PUBLISH_MODE") {
		t.Fatal("image workers still need a separate mode")
	}
}

func TestValidationRejectsStalePreparedRecipesAndPins(t *testing.T) {
	old, updated := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	dep := sampleRecipe(t, "go")
	app := sampleRecipe(t, "app", "go:1@"+old)
	depReport := sampleReport(dep, updated)
	prepared, err := Prepare(app, map[string]Report{"go": depReport}, depReport.Commit, depReport.Tag)
	if err != nil {
		t.Fatal(err)
	}
	preparedApp, err := Parse(app.File, prepared)
	if err != nil {
		t.Fatal(err)
	}
	valid := sampleReport(preparedApp, old)
	for _, change := range []func(*Report){
		func(r *Report) { r.RecipeJSON = app.Bytes },
		func(r *Report) { r.Pins = app.Pins },
		func(r *Report) { r.ID = "wrong" },
		func(r *Report) {
			r.RecipeJSON = append([]byte(nil), r.RecipeJSON...)
			r.RecipeJSON = append(r.RecipeJSON, ' ')
		},
	} {
		altered := valid
		change(&altered)
		if err := Validate([]Recipe{dep, app}, map[string]Report{"go": depReport, "app": altered}, depReport.Commit, depReport.Tag); err == nil {
			t.Fatal("accepted a stale or inconsistent publication report")
		}
	}
}
