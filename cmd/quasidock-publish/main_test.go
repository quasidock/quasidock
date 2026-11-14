package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBootstrapPatchApplies(t *testing.T) {
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
