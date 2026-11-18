package recipe

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func buildSource(name string, paths ...string) Source {
	dest := "app/"
	return Source{Build: &BuildSource{Name: name, Paths: paths}, Dest: &dest}
}

func buildRecipe() Recipe {
	return Recipe{
		ID: "app", Version: "1", Entrypoint: Entrypoint{"app/server.exe"},
		Builds: []BuildUnit{{
			Name:   "builder",
			Stages: []Stage{{Name: "compile", Build: []string{"compile"}}},
		}},
		Stages: []Stage{{Name: "package", Sources: []Source{buildSource("builder", "out/server.exe")}}},
	}
}

func parseBuildRecipe(r Recipe) (*Recipe, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return Parse("builds.json", data)
}

func TestBuildUnitRecipeFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/build-units.json")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse("build-units.json", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Builds) != 1 || r.Builds[0].Name != "builder" || r.Builds[0].SDK["go"] == "" || len(r.Builds[0].Stages) != 1 {
		t.Fatalf("build unit was not decoded: %+v", r.Builds)
	}
	source := r.Stages[0].Sources[0]
	if source.Build == nil || source.Build.Name != "builder" || !reflect.DeepEqual(source.Build.Paths, []string{"out/server.exe"}) || *source.Dest != "app/server.exe" {
		t.Fatalf("build output selection was not decoded: %+v", source)
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse("roundtrip.json", encoded)
	if err != nil || !reflect.DeepEqual(r, again) {
		t.Fatalf("recipe did not round trip: %v", err)
	}
	cfg, err := r.AppConfig(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.From) != 0 || cfg.Build != nil || cfg.Entrypoint[0] != "app/server.exe" {
		t.Fatalf("unit tools leaked into runtime config: %+v", cfg)
	}
	if r.HasBuildCommands() {
		t.Fatal("unit commands must not be mistaken for commands in the final output scope")
	}
}

func TestBuildUnitScopesAndDeclarationOrder(t *testing.T) {
	r := buildRecipe()
	firstPin := "node:22@sha256:" + strings.Repeat("a", 64)
	secondPin := "node:24@sha256:" + strings.Repeat("b", 64)
	r.Builds[0].SDK = map[string]string{"node": firstPin}
	r.Builds[0].Stages = []Stage{{Name: "package", Sources: []Source{buildSource("BASE", ".")}}}
	r.Builds = append(r.Builds, BuildUnit{
		Name: "base", From: map[string]string{"node": secondPin},
		Stages: []Stage{{Name: "package", Build: []string{"node --version"}}},
	})
	parsed, err := parseBuildRecipe(r)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Builds[0].SDK["node"] != firstPin || parsed.Builds[1].From["node"] != secondPin || len(parsed.SDK) != 0 || len(parsed.From) != 0 {
		t.Fatal("tool scopes were not kept separate")
	}
	// Unit names, stage names and tool aliases occupy separate scopes. Native
	// units may reference a later declaration, unlike the initial Docker subset.
	if parsed.Builds[0].Stages[0].Name != parsed.Stages[0].Name {
		t.Fatal("stage names should remain local to each filesystem")
	}
}

func TestBuildSourceSelectors(t *testing.T) {
	for _, tc := range []struct {
		name, dest string
		paths      []string
	}{
		{"rename", "app/server.exe", []string{"out/server.exe"}},
		{"directory", "app/", []string{"dist"}},
		{"whole output", ".", []string{"."}},
		{"whole output directory", "./", []string{"."}},
		{"glob", "app/", []string{"out/*.exe"}},
		{"character class", "app/", []string{"out/server[12].exe"}},
		{"multiple selectors", "app/", []string{"out/*.exe", "assets"}},
		{"multiple selectors at root", "./", []string{"out/server.exe", "assets"}},
		{"literal brackets in destination", "app/[v1]/", []string{"out/server.exe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := buildRecipe()
			source := buildSource("BUILDER", tc.paths...)
			source.Dest = &tc.dest
			r.Stages[0].Sources[0] = source
			if _, err := parseBuildRecipe(r); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBuildSourceValidation(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		change        func(*Source)
	}{
		{"missing unit name", "valid build unit name", func(s *Source) { s.Build.Name = "" }},
		{"invalid unit name", "valid build unit name", func(s *Source) { s.Build.Name = "../builder" }},
		{"missing selectors", "at least one path selector", func(s *Source) { s.Build.Paths = nil }},
		{"missing dest", "normalized relative dest", func(s *Source) { s.Dest = nil }},
		{"multiple to file", "dest ending in '/'", func(s *Source) { dest := "file.exe"; s.Dest = &dest; s.Build.Paths = []string{"a", "b"} }},
		{"sha256", "do not apply", func(s *Source) { hash := strings.Repeat("a", 64); s.Sha256 = &hash }},
		{"type", "do not apply", func(s *Source) { kind := KindFile; s.Type = &kind }},
		{"strip", "do not apply", func(s *Source) { s.Strip = 1 }},
		{"skip", "do not apply", func(s *Source) { s.Skip = []string{"*.pdb"} }},
		{"url", "exactly one", func(s *Source) { url := "https://example.com/app.exe"; s.URL = &url }},
		{"path", "exactly one", func(s *Source) { path := "app.exe"; s.Path = &path }},
		{"image", "exactly one", func(s *Source) { image := "app@sha256:" + strings.Repeat("a", 64); s.Image = &image }},
		{"context", "exactly one", func(s *Source) { s.Context = &ContextSource{Root: ".", Dockerfile: "Dockerfile", Paths: []string{"."}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := buildRecipe()
			tc.change(&r.Stages[0].Sources[0])
			_, err := parseBuildRecipe(r)
			if err == nil || !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), "stage package source 1") {
				t.Fatalf("want scoped error containing %q, got %v", tc.message, err)
			}
		})
	}
	for _, selector := range []string{"", "/out/app.exe", "../app.exe", "out/../app.exe", "out//app.exe", "./out/app.exe", "out/", `out\app.exe`, "C:/app.exe", "bad\x00path", "bad\npath", "bad<name", "out/["} {
		t.Run("selector="+selector, func(t *testing.T) {
			r := buildRecipe()
			r.Stages[0].Sources[0].Build.Paths = []string{selector}
			if _, err := parseBuildRecipe(r); err == nil || !strings.Contains(err.Error(), "build selector") {
				t.Fatalf("accepted selector %q: %v", selector, err)
			}
		})
	}
	for _, dest := range []string{"", "/app.exe", "../app.exe", "app/../app.exe", "app//server.exe", "app//", "./app/server.exe", `app\server.exe`, "C:/app.exe", "app/*.exe", "app/?.exe", "bad\x00path", "bad\npath", "bad|name"} {
		t.Run("dest="+dest, func(t *testing.T) {
			r := buildRecipe()
			r.Stages[0].Sources[0].Dest = &dest
			if _, err := parseBuildRecipe(r); err == nil || !strings.Contains(err.Error(), "relative dest") {
				t.Fatalf("accepted destination %q: %v", dest, err)
			}
		})
	}
}

func TestBuildUnitValidation(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		change        func(*Recipe)
	}{
		{"empty name", "build unit name", func(r *Recipe) { r.Builds[0].Name = "" }},
		{"long name", "build unit name", func(r *Recipe) { r.Builds[0].Name = strings.Repeat("a", 58) }},
		{"leading dot", "build unit name", func(r *Recipe) { r.Builds[0].Name = ".hidden" }},
		{"duplicate names", "duplicate build unit name", func(r *Recipe) { unit := r.Builds[0]; unit.Name = "BUILDER"; r.Builds = append(r.Builds, unit) }},
		{"empty unit", "at least one stage", func(r *Recipe) { r.Builds[0].Stages = nil }},
		{"empty stage", "neither sources nor build commands", func(r *Recipe) { r.Builds[0].Stages[0].Build = nil }},
		{"duplicate stages", "two stages named", func(r *Recipe) {
			stage := r.Builds[0].Stages[0]
			stage.Name = "COMPILE"
			r.Builds[0].Stages = append(r.Builds[0].Stages, stage)
		}},
		{"reserved alias", "sdk alias", func(r *Recipe) { r.Builds[0].SDK = map[string]string{"app": "any"} }},
		{"invalid from alias", "from alias", func(r *Recipe) { r.Builds[0].From = map[string]string{"Node": "any"} }},
		{"conflicting aliases", "both an sdk and a base image alias", func(r *Recipe) {
			r.Builds[0].SDK = map[string]string{"tool": "any"}
			r.Builds[0].From = map[string]string{"tool": "any"}
		}},
		{"unknown root source", "references unknown build unit", func(r *Recipe) { r.Stages[0].Sources[0].Build.Name = "missing" }},
		{"no units", "references unknown build unit", func(r *Recipe) { r.Builds = nil }},
		{"unknown unit source", "references unknown build unit", func(r *Recipe) { r.Builds[0].Stages[0].Sources = []Source{buildSource("missing", ".")} }},
		{"invalid unit source", "unsafe context path", func(r *Recipe) {
			r.Builds[0].Stages[0].Sources = []Source{{Context: &ContextSource{Root: ".", Dockerfile: "Dockerfile", Paths: []string{"../x"}}}}
		}},
		{"source error in unit", "exactly one", func(r *Recipe) { r.Builds[0].Stages[0].Sources = []Source{{}} }},
		{"invalid vendor", "vendor dir", func(r *Recipe) { r.Builds[0].Stages[0].Vendor = &Vendor{Commands: []string{"x"}, Dir: "../out"} }},
		{"environment without command", "only apply to build commands", func(r *Recipe) {
			r.Builds[0].Stages[0] = Stage{Name: "compile", Env: map[string]string{"MODE": "test"}, Sources: []Source{buildSource("missing", ".")}}
		}},
		{"self reference", "cannot reference itself", func(r *Recipe) { r.Builds[0].Stages[0].Sources = []Source{buildSource("BUILDER", ".")} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := buildRecipe()
			tc.change(&r)
			_, err := parseBuildRecipe(r)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want %q, got %v", tc.message, err)
			}
		})
	}
}

func TestBuildUnitDependencyCycles(t *testing.T) {
	for _, unused := range []bool{false, true} {
		t.Run(fmt.Sprintf("unused=%t", unused), func(t *testing.T) {
			r := buildRecipe()
			r.Builds[0].Stages[0].Sources = []Source{buildSource("other", ".")}
			r.Builds = append(r.Builds, BuildUnit{Name: "other", Stages: []Stage{{Name: "compile", Sources: []Source{buildSource("BUILDER", ".")}}}})
			if unused {
				path := "app.exe"
				r.Stages[0].Sources = []Source{{Path: &path}}
			}
			if _, err := parseBuildRecipe(r); err == nil || !strings.Contains(err.Error(), "circular dependency") {
				t.Fatalf("accepted circular graph: %v", err)
			}
		})
	}
}

func TestBuildUnitDependencyPins(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, group := range []string{"sdk", "from"} {
		for _, tc := range []struct {
			ref   string
			valid bool
		}{
			{"tool" + digest, true},
			{"Tool:1.2" + digest, true},
			{"ghcr.io/example/tool:1.2" + digest, true},
			{"localhost:5000/tool" + digest, true},
			{"tool", false},
			{"tool:latest", false},
			{"ghcr.io/example/tool:1.2", false},
			{"tool@sha256:bad", false},
			{"bad name" + digest, false},
			{"tool:bad tag" + digest, false},
			{"https://example.com/tool" + digest, false},
			{"owner/tool" + digest, false},
			{"tool@@sha256:" + strings.Repeat("a", 64), false},
			{"sha256:" + strings.Repeat("a", 64), false},
		} {
			t.Run(group+"="+tc.ref, func(t *testing.T) {
				r := buildRecipe()
				if group == "sdk" {
					r.Builds[0].SDK = map[string]string{"tool": tc.ref}
				} else {
					r.Builds[0].From = map[string]string{"tool": tc.ref}
				}
				_, err := parseBuildRecipe(r)
				if (err == nil) != tc.valid {
					t.Fatalf("valid=%t: %v", tc.valid, err)
				}
				if err != nil && !strings.Contains(err.Error(), "build builder: "+group+" tool") {
					t.Fatalf("missing dependency scope: %v", err)
				}
			})
		}
	}
}

// Freeze the old source decoder and source-kind check. Previous clients ignore
// builds/build fields during JSON decoding, then reject the resulting source
// because none of the four source kinds they understand is present.
func legacySourceValidation(data []byte) error {
	type legacySource struct {
		URL, Path, Image *string
		Context          *ContextSource
	}
	var legacy struct {
		Stages []struct {
			Sources []legacySource
		}
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	for _, stage := range legacy.Stages {
		for _, source := range stage.Sources {
			kinds := 0
			for _, present := range []bool{source.URL != nil, source.Path != nil, source.Image != nil, source.Context != nil} {
				if present {
					kinds++
				}
			}
			if kinds != 1 {
				return fmt.Errorf("source needs exactly one of url, path, image or context")
			}
		}
	}
	return nil
}

func TestBuildUnitRecipeRejectedByLegacySourceValidation(t *testing.T) {
	data, err := os.ReadFile("testdata/build-units.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse("new.json", data); err != nil {
		t.Fatal(err)
	}
	if err := legacySourceValidation(data); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("old source validation silently accepted a build-dependent recipe: %v", err)
	}
	legacy := []byte(`{"id":"app","version":"1","entrypoint":"app.exe","stages":[{"name":"copy","sources":[{"path":"app.exe"}]}]}`)
	if _, err := Parse("old.json", legacy); err != nil {
		t.Fatal(err)
	}
	if err := legacySourceValidation(legacy); err != nil {
		t.Fatalf("legacy fixture was rejected: %v", err)
	}
	r, _ := Parse("old.json", legacy)
	encoded, err := json.Marshal(r)
	if err != nil || strings.Contains(string(encoded), `"builds"`) || strings.Contains(string(encoded), `"build":`) {
		t.Fatalf("legacy recipe gained build-unit fields: %s %v", encoded, err)
	}
}
