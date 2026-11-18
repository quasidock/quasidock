package recipe

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/tree"
)

func validateBuildSource(fileName, stage string, n int, source Source) error {
	where := fmt.Sprintf("%s: stage %s source %d", fileName, stage, n)
	if !oci.IsValidID(source.Build.Name) {
		return fmt.Errorf("%s: build source needs a valid build unit name", where)
	}
	if len(source.Build.Paths) == 0 {
		return fmt.Errorf("%s: build source needs at least one path selector", where)
	}
	for _, selector := range source.Build.Paths {
		if !validBuildPath(selector, true) {
			return fmt.Errorf("%s: build selector %q must be a normalized '/'-separated path inside the unit output", where, selector)
		}
		if _, err := path.Match(selector, ""); err != nil {
			return fmt.Errorf("%s: invalid build selector %q: %w", where, selector, err)
		}
	}
	if source.Dest == nil || !validBuildPath(strings.TrimSuffix(*source.Dest, "/"), false) {
		return fmt.Errorf("%s: build source needs a normalized relative dest inside the receiving output", where)
	}
	if len(source.Build.Paths) > 1 && !strings.HasSuffix(*source.Dest, "/") {
		return fmt.Errorf("%s: build source with multiple selectors requires a dest ending in '/'", where)
	}
	if source.Sha256 != nil || source.Type != nil || source.Strip != 0 || len(source.Skip) != 0 {
		return fmt.Errorf("%s: sha256, type, strip and skip do not apply to build sources", where)
	}
	return nil
}

func validBuildPath(value string, selector bool) bool {
	if !oci.IsSafeRelPath(value) || strings.ContainsAny(value, "\\<>:\"|\x00") || path.Clean(value) != value {
		return false
	}
	if !selector && strings.ContainsAny(value, "*?") {
		return false
	}
	for _, ch := range value {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	return true
}

func validateBuildUnits(fileName string, r Recipe) error {
	names := make(map[string]int, len(r.Builds))
	for index, unit := range r.Builds {
		if !oci.IsValidID(unit.Name) {
			return fmt.Errorf("%s: build unit name %q must be 1-57 characters of letters, digits, '.', '-', '_', not starting with '.'", fileName, unit.Name)
		}
		key := strings.ToLower(unit.Name)
		if _, exists := names[key]; exists {
			return fmt.Errorf("%s: duplicate build unit name %q", fileName, unit.Name)
		}
		names[key] = index
		where := fmt.Sprintf("%s: build %s", fileName, unit.Name)
		if err := validateExecutables(where, unit.Executables); err != nil {
			return err
		}
		if err := validateDependencies(where, unit.SDK, unit.From); err != nil {
			return err
		}
		for _, group := range []struct {
			name string
			refs map[string]string
		}{{"sdk", unit.SDK}, {"from", unit.From}} {
			for _, alias := range slices.Sorted(maps.Keys(group.refs)) {
				if !pinnedBuildReference(group.refs[alias]) {
					return fmt.Errorf("%s: %s %s must be an image reference pinned with @sha256:...", where, group.name, alias)
				}
			}
		}
		if err := validateStages(where, unit.Stages); err != nil {
			return err
		}
	}
	// Validate every definition, including unused units. Declaration order does
	// not constrain native recipe references; the dependency graph must be a DAG.
	dependencies := make([][]int, len(r.Builds))
	references := func(where string, stages []Stage) ([]int, error) {
		var result []int
		for _, stage := range stages {
			for index, source := range stage.Sources {
				if source.Build == nil {
					continue
				}
				unit, exists := names[strings.ToLower(source.Build.Name)]
				if !exists {
					return nil, fmt.Errorf("%s: stage %s source %d references unknown build unit %q", where, stage.Name, index+1, source.Build.Name)
				}
				result = append(result, unit)
			}
		}
		return result, nil
	}
	if _, err := references(fileName, r.Stages); err != nil {
		return err
	}
	for index, unit := range r.Builds {
		where := fmt.Sprintf("%s: build %s", fileName, unit.Name)
		deps, err := references(where, unit.Stages)
		if err != nil {
			return err
		}
		for _, dep := range deps {
			if dep == index {
				return fmt.Errorf("%s: build unit cannot reference itself", where)
			}
		}
		dependencies[index] = deps
	}
	visited := make([]uint8, len(r.Builds))
	var visit func(int) error
	visit = func(index int) error {
		if visited[index] == 1 {
			return fmt.Errorf("%s: circular dependency on build unit %q", fileName, r.Builds[index].Name)
		}
		if visited[index] == 2 {
			return nil
		}
		visited[index] = 1
		for _, dep := range dependencies[index] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visited[index] = 2
		return nil
	}
	for index := range r.Builds {
		if err := visit(index); err != nil {
			return err
		}
	}
	return nil
}

func validateExecutables(where string, executables []string) error {
	physical := map[string]string{}
	logical := map[string]bool{}
	for _, executable := range executables {
		if executable == "." || !validBuildPath(executable, false) {
			return fmt.Errorf("%s: executable %q must name a normalized file path inside the output", where, executable)
		}
		key := strings.ToLower(tree.NativeExecutable(executable))
		if previous, exists := physical[key]; exists {
			return fmt.Errorf("%s: ambiguous executable mappings %q and %q", where, previous, executable)
		}
		physical[key] = executable
		logical[strings.ToLower(executable)] = true
	}
	for name := range logical {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if logical[parent] {
				return fmt.Errorf("%s: executable %q conflicts with directory %q", where, parent, name)
			}
		}
	}
	for name := range physical {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, exists := physical[parent]; exists {
				return fmt.Errorf("%s: native executable %q conflicts with directory %q", where, parent, name)
			}
		}
	}
	return nil
}

// Validate both short app references and fully qualified references without
// consulting the registry or the user's default registry configuration.
func pinnedBuildReference(text string) bool {
	if !strings.ContainsRune(text, '/') {
		end := strings.IndexAny(text, ":@")
		if end == -1 {
			return false
		}
		if !oci.IsValidID(text[:end]) {
			return false
		}
		text = "localhost/" + strings.ToLower(text[:end]) + text[end:]
	}
	ref, err := registry.ParseReference(text)
	return err == nil && ref.Digest != ""
}

// ReachableBuilds follows source order and emits each reachable unit once.
// Recipes are validated by Parse; guards here also protect direct snapshot use.
func (r Recipe) ReachableBuilds() ([]BuildUnit, error) {
	units := make(map[string]BuildUnit, len(r.Builds))
	for _, unit := range r.Builds {
		units[strings.ToLower(unit.Name)] = unit
	}
	visited := map[string]uint8{}
	var result []BuildUnit
	var visit func([]Stage) error
	visit = func(stages []Stage) error {
		for _, step := range stages {
			for _, source := range step.Sources {
				if source.Build == nil {
					continue
				}
				name := strings.ToLower(source.Build.Name)
				unit, exists := units[name]
				if !exists {
					return fmt.Errorf("stage %s references unknown build unit %q", step.Name, source.Build.Name)
				}
				if visited[name] == 1 {
					return fmt.Errorf("circular dependency on build unit %q", unit.Name)
				}
				if visited[name] == 2 {
					continue
				}
				visited[name] = 1
				if err := visit(unit.Stages); err != nil {
					return err
				}
				visited[name] = 2
				result = append(result, unit)
			}
		}
		return nil
	}
	if err := visit(r.Stages); err != nil {
		return nil, err
	}
	return result, nil
}
