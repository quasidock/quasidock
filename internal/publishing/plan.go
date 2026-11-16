// Package publishing describes the allowlisted recipe graph and staged results.
package publishing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

const Registry = "ghcr.io/quasidock/quasidock"

var pinPattern = regexp.MustCompile(`"([A-Za-z0-9_.-]+):([^"@ ]+)@(sha256:[0-9a-f]{64})"`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type Pin struct{ ID, Version, Digest string }
type Recipe struct {
	File    string `json:"-"`
	ID      string `json:"id"`
	Version string `json:"version"`
	Pins    []Pin  `json:"-"`
	Bytes   []byte `json:"-"`
}
type Report struct {
	Recipe     string   `json:"recipe"`
	ID         string   `json:"id"`
	Version    string   `json:"version"`
	Digest     string   `json:"digest"`
	Commit     string   `json:"commit"`
	Tag        string   `json:"tag"`
	Registry   string   `json:"registry"`
	Pins       []Pin    `json:"pins"`
	Sources    []string `json:"sources"`
	RecipeJSON []byte   `json:"recipe_bytes"`
}

func (r Report) Ref() string { return r.Registry + "/" + r.ID + "@" + r.Digest }
func (r Recipe) Job() string {
	return "image_" + strings.ReplaceAll(strings.ReplaceAll(r.ID, "-", "_"), ".", "_")
}

func Parse(file string, data []byte) (Recipe, error) {
	r := Recipe{File: file, Bytes: data}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if r.ID == "" || r.Version == "" {
		return r, fmt.Errorf("%s: missing id or version", file)
	}
	seen := map[string]Pin{}
	for _, match := range pinPattern.FindAllSubmatch(data, -1) {
		p := Pin{string(match[1]), string(match[2]), string(match[3])}
		if prior, ok := seen[p.ID]; ok && prior != p {
			return r, fmt.Errorf("%s pins conflicting versions of %s", file, p.ID)
		}
		seen[p.ID] = p
	}
	for _, p := range seen {
		r.Pins = append(r.Pins, p)
	}
	sort.Slice(r.Pins, func(i, j int) bool { return r.Pins[i].ID < r.Pins[j].ID })
	return r, nil
}

func Load(root string) ([]Recipe, error) {
	list, err := os.ReadFile(filepath.Join(root, "scripts", "published-recipes.txt"))
	if err != nil {
		return nil, err
	}
	var recipes []Recipe
	ids, jobs := map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(string(list), "\n") {
		file := strings.TrimSpace(line)
		if file == "" || strings.HasPrefix(file, "#") {
			continue
		}
		if filepath.Base(file) != file || !strings.HasSuffix(file, ".json") {
			return nil, fmt.Errorf("recipe is outside publish allowlist directory: %s", file)
		}
		data, err := os.ReadFile(filepath.Join(root, "recipes", file))
		if err != nil {
			return nil, err
		}
		r, err := Parse(file, data)
		if err != nil {
			return nil, err
		}
		if ids[r.ID] || jobs[r.Job()] {
			return nil, fmt.Errorf("duplicate recipe id or workflow job: %s", r.ID)
		}
		ids[r.ID], jobs[r.Job()] = true, true
		recipes = append(recipes, r)
	}
	for _, r := range recipes {
		for _, p := range r.Pins {
			if !ids[p.ID] {
				return nil, fmt.Errorf("%s depends on %s outside the publish allowlist", r.File, p.ID)
			}
			if p.ID == r.ID {
				return nil, fmt.Errorf("%s pins itself; publication requires an explicit seed", r.File)
			}
		}
	}
	if _, err := Order(recipes); err != nil {
		return nil, err
	}
	return recipes, nil
}

func Order(recipes []Recipe) ([]Recipe, error) {
	var ordered []Recipe
	done := map[string]bool{}
	for len(ordered) < len(recipes) {
		progress := false
		for _, r := range recipes {
			if done[r.ID] {
				continue
			}
			ready := true
			for _, p := range r.Pins {
				if !done[p.ID] {
					ready = false
				}
			}
			if ready {
				done[r.ID] = true
				ordered = append(ordered, r)
				progress = true
			}
		}
		if !progress {
			return nil, fmt.Errorf("cycle in published recipe dependencies")
		}
	}
	return ordered, nil
}

// Prepare updates only dependency references; source URLs and hashes stay exact.
func Prepare(r Recipe, reports map[string]Report, commit, tag string) ([]byte, error) {
	data := string(r.Bytes)
	for _, p := range r.Pins {
		dep, ok := reports[p.ID]
		if !ok {
			return nil, fmt.Errorf("%s: missing result for dependency %s", r.File, p.ID)
		}
		if dep.ID != p.ID || dep.Commit != commit || dep.Tag != tag || dep.Registry != Registry {
			return nil, fmt.Errorf("%s: dependency result belongs to another publishing run", p.ID)
		}
		if !digestPattern.MatchString(dep.Digest) {
			return nil, fmt.Errorf("%s: invalid dependency digest", p.ID)
		}
		old := fmt.Sprintf("\"%s:%s@%s\"", p.ID, p.Version, p.Digest)
		updated := fmt.Sprintf("\"%s:%s@%s\"", p.ID, dep.Version, dep.Digest)
		data = strings.ReplaceAll(data, old, updated)
	}
	return []byte(data), nil
}

func ReadReports(dir string) (map[string]Report, error) {
	reports := map[string]Report{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".report.json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r Report
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if _, exists := reports[r.ID]; exists {
			return fmt.Errorf("duplicate result for %s", r.ID)
		}
		reports[r.ID] = r
		return nil
	})
	return reports, err
}

func Validate(recipes []Recipe, reports map[string]Report, commit, tag string) error {
	if len(recipes) != len(reports) {
		return fmt.Errorf("expected %d recipe reports, got %d", len(recipes), len(reports))
	}
	for _, r := range recipes {
		report, ok := reports[r.ID]
		if !ok || report.ID != r.ID || report.Recipe != r.File || report.Version != r.Version || report.Commit != commit || report.Tag != tag || report.Registry != Registry {
			return fmt.Errorf("invalid or missing report for %s", r.File)
		}
		if !digestPattern.MatchString(report.Digest) {
			return fmt.Errorf("invalid image digest for %s", r.ID)
		}
		prepared, err := Prepare(r, reports, commit, tag)
		if err != nil {
			return err
		}
		if string(prepared) != string(report.RecipeJSON) {
			return fmt.Errorf("%s report does not match prepared recipe", r.File)
		}
		preparedRecipe, err := Parse(r.File, prepared)
		if err != nil {
			return err
		}
		if !slices.Equal(report.Pins, preparedRecipe.Pins) {
			return fmt.Errorf("%s report pins do not match prepared recipe", r.File)
		}
	}
	return nil
}
