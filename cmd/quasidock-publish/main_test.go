package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/publishing"
)

func TestPublicationPatchApplies(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "recipes"), 0755); err != nil {
		t.Fatal(err)
	}
	old, newRecipe := []byte("{\n  \"id\": \"go\",\n  \"version\": \"1\"\n}\n"), []byte("{\n  \"id\": \"go\",\n  \"version\": \"2\"\n}\n")
	path := filepath.Join(dir, "recipes", "go.json")
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(dir, "pins.patch")
	if err := os.WriteFile(patch, []byte(fullPatch("recipes/go.json", old, newRecipe)), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-c", "core.autocrlf=false", "apply", patch)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("patch does not apply: %s %v", out, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newRecipe) {
		t.Fatal("patch changed unexpected bytes")
	}
}

func TestPrepareAndCollectRefreshPinsInOneRun(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	commit, tag := strings.Repeat("c", 40), "ci-123"
	t.Setenv("GITHUB_SHA", commit)
	t.Setenv("GITHUB_RUN_ID", "123")
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	old, fresh := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	depBytes := []byte(`{"id":"go","version":"2"}`)
	appBytes := []byte(`{"id":"app","version":"1","sdk":{"go":"go:1@` + old + `"},"source":"https://example.test/archive.zip","sha256":"unchanged"}`)
	write("scripts/published-recipes.txt", []byte("go.json\napp.json\n"))
	write("recipes/go.json", depBytes)
	write("recipes/app.json", appBytes)
	dep, err := publishing.Parse("go.json", depBytes)
	if err != nil {
		t.Fatal(err)
	}
	app, err := publishing.Parse("app.json", appBytes)
	if err != nil {
		t.Fatal(err)
	}
	depReport := publishing.Report{Recipe: dep.File, ID: dep.ID, Version: dep.Version, Digest: fresh, Commit: commit, Tag: tag, Registry: publishing.Registry, RecipeJSON: depBytes}
	encoded, _ := json.Marshal(depReport)
	write("reports/go.report.json", encoded)
	if err := run([]string{"prepare", "app.json", "reports", "prepared", "prepared.txt"}); err != nil {
		t.Fatal(err)
	}
	prepared, err := os.ReadFile("prepared/recipes/app.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prepared), "go:2@"+fresh) || !strings.Contains(string(prepared), `"sha256":"unchanged"`) {
		t.Fatalf("unexpected prepared recipe: %s", prepared)
	}
	preparedApp, err := publishing.Parse(app.File, prepared)
	if err != nil {
		t.Fatal(err)
	}
	appReport := publishing.Report{Recipe: app.File, ID: app.ID, Version: app.Version, Digest: old, Commit: commit, Tag: tag, Registry: publishing.Registry, RecipeJSON: prepared, Pins: preparedApp.Pins}
	encoded, _ = json.Marshal(appReport)
	write("reports/app.report.json", encoded)
	if err := run([]string{"collect", "reports", "result"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile("result/recipes/app.json")
	if err != nil || string(snapshot) != string(prepared) {
		t.Fatalf("missing exact prepared recipe: %s %v", snapshot, err)
	}
	patch, err := os.ReadFile("result/pins.patch")
	if err != nil || !strings.Contains(string(patch), "go:2@"+fresh) {
		t.Fatalf("missing pin patch: %s %v", patch, err)
	}
	unchanged, err := os.ReadFile("recipes/app.json")
	if err != nil || string(unchanged) != string(appBytes) {
		t.Fatal("publication changed the checked-out recipe")
	}
	if err := run([]string{"collect", "bootstrap", "reports", "result"}); err == nil {
		t.Fatal("accepted a separate bootstrap mode")
	}
	if err := os.Remove("reports/app.report.json"); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"collect", "reports", "incomplete"}); err == nil {
		t.Fatal("accepted incomplete publication")
	}
	if _, err := os.Stat("incomplete"); !os.IsNotExist(err) {
		t.Fatal("incomplete publication wrote release inputs")
	}
}
