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
	return Report{Recipe: r.File, ID: r.ID, Version: r.Version, Digest: digest, Commit: strings.Repeat("c", 40), Tag: "ci-1", Registry: Registry, RecipeJSON: r.Bytes}
}

func TestBootstrapAndPublishPins(t *testing.T) {
	old, newDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	dep := sampleRecipe(t, "go")
	app := sampleRecipe(t, "fzf", "go:1@"+old)
	reports := map[string]Report{"go": sampleReport(dep, newDigest)}
	before := string(app.Bytes)
	prepared, err := Prepare(app, reports, "bootstrap", strings.Repeat("c", 40), "ci-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prepared), newDigest) || string(app.Bytes) != before {
		t.Fatal("bootstrap did not update a copy of the pin")
	}
	if _, err := Prepare(app, reports, "publish", strings.Repeat("c", 40), "ci-1"); err == nil {
		t.Fatal("publish silently changed a committed pin")
	}
	app, err = Parse(app.File, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Prepare(app, reports, "publish", strings.Repeat("c", 40), "ci-1"); err != nil || string(got) != string(prepared) {
		t.Fatalf("publish changed prepared recipe: %v", err)
	}
	for _, change := range []func(*Report){func(r *Report) { r.Commit = "other" }, func(r *Report) { r.Tag = "ci-2" }, func(r *Report) { r.Registry = "ghcr.io/other" }, func(r *Report) { r.Digest = "invalid" }} {
		rp := reports["go"]
		change(&rp)
		if _, err := Prepare(app, map[string]Report{"go": rp}, "bootstrap", strings.Repeat("c", 40), "ci-1"); err == nil {
			t.Fatal("accepted foreign or malformed dependency result")
		}
	}
	if _, err := Prepare(app, nil, "bootstrap", strings.Repeat("c", 40), "ci-1"); err == nil {
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
	if err := Validate([]Recipe{r}, reports, "publish", rp.Commit, rp.Tag); err != nil {
		t.Fatal(err)
	}
	if string(reports["go"].RecipeJSON) != string(r.Bytes) {
		t.Fatal("report serialization changed recipe bytes")
	}
	if err := Validate([]Recipe{r}, nil, "publish", rp.Commit, rp.Tag); err == nil {
		t.Fatal("accepted incomplete reports")
	}
	if err := os.WriteFile(filepath.Join(dir, "duplicate.report.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReports(dir); err == nil {
		t.Fatal("accepted duplicate reports")
	}
}
func TestAllowlistAndGeneratedWorkflow(t *testing.T) {
	root := filepath.Join("..", "..")
	recipes, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "publish.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(got), "\r\n", "\n") != Workflow(recipes) {
		t.Fatal("run go run ./cmd/quasidock-publish generate")
	}
	for _, r := range recipes {
		if strings.Contains(r.File, "no-publish") {
			t.Fatal("excluded recipe in publication")
		}
	}
	workspace := t.TempDir()
	os.MkdirAll(filepath.Join(workspace, "scripts"), 0755)
	os.WriteFile(filepath.Join(workspace, "scripts", "published-recipes.txt"), []byte("../no-publish/private.json\n"), 0600)
	if _, err := Load(workspace); err == nil {
		t.Fatal("accepted a recipe outside the allowlist directory")
	}
}
