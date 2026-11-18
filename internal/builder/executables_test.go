package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/buildcontext"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/tree"
)

func TestBuildGraphPreservesExecutableMappingsThroughCopies(t *testing.T) {
	ctx, _ := unitFixture(t)
	root := t.TempDir()
	unitFile(t, root, "server.exe", "native executable bytes")
	file := filepath.Join(root, "quasidock.json")
	r := recipe.Recipe{ID: "mapped-artifacts", Version: "1", Entrypoint: recipe.Entrypoint{"app/server.exe"}, Executables: []string{"app/server"}, Builds: []recipe.BuildUnit{
		{Name: "producer", Executables: []string{"out/server.exe"}, Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{contextSource("out/server.exe", "server.exe")}}}},
		{Name: "middle", Executables: []string{"pkg/server"}, Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{unitSource("producer", "pkg/server", "out/server.exe")}}}},
	}, Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{unitSource("middle", "app/server", "pkg/server")}}}}
	writeUnitRecipe(t, file, r)
	image, err := Build(ctx, file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	files := unitLayerFiles(t, ctx, image)
	if files["app/server.exe"] != "native executable bytes" {
		t.Fatal(files)
	}
	if _, exists := files["app/server"]; exists {
		t.Fatal("logical executable filename escaped into the native layer")
	}
}

func TestBuildSourceTranslatesCompletedNativeExecutables(t *testing.T) {
	ctx, _ := unitFixture(t)
	producer := t.TempDir()
	unitFile(t, producer, "out/server.exe", "native executable bytes")
	output, err := buildcontext.ReadOutput(producer)
	if err != nil {
		t.Fatal(err)
	}
	projects := &Snapshots{
		outputs:           map[string]*tree.Tree{"producer": output},
		outputExecutables: map[string][]string{"producer": {"out/server"}},
		executables:       []string{"bin/renamed"},
	}
	sources := NewSources(ctx, ".", "fixture", projects)
	defer sources.Close()
	result := tree.New()
	stage := recipe.Stage{Name: "copy", Sources: []recipe.Source{unitSource("producer", "bin/renamed", "out/server")}}
	if err := sources.Add(result, stage, "fixture.json"); err != nil {
		t.Fatal(err)
	}
	if !result.IsFile("bin/renamed.exe") || result.IsFile("bin/renamed") {
		t.Fatal(result.Sorted())
	}
	dest := t.TempDir()
	if err := result.WriteFiles(dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "bin", "renamed.exe"))
	if err != nil || string(data) != "native executable bytes" {
		t.Fatalf("mapped source bytes lost: %q %v", data, err)
	}
}

func TestExecutableCollisionChecksReadLiveContext(t *testing.T) {
	for _, filename := range []string{"server", "server.exe"} {
		t.Run(filename, func(t *testing.T) {
			ctx, _ := unitFixture(t)
			root := t.TempDir()
			file := filepath.Join(root, "quasidock.json")
			r := recipe.Recipe{ID: "artifact-collision", Version: "1", Entrypoint: recipe.Entrypoint{"bin/server.exe"}, Executables: []string{"bin/server"}, Stages: []recipe.Stage{{Name: "copy", Sources: []recipe.Source{contextSource("bin/", "*")}}}}
			writeUnitRecipe(t, file, r)
			// The file arrives after conversion/schema validation. Build-time
			// source selection must reject it before it can replace an artifact.
			unitFile(t, root, filename, "literal collision")
			if _, err := Build(ctx, file, Options{}); err == nil || !strings.Contains(err.Error(), "collides with mapped executable") {
				t.Fatalf("new context file occupied an executable alias: %v", err)
			}
		})
	}
}
