package builder

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/quasidock/quasidock/internal/buildcontext"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/tree"
)

func unitRecipe(r *recipe.Recipe, unit recipe.BuildUnit) *recipe.Recipe {
	return &recipe.Recipe{ID: r.ID, Version: r.Version, From: unit.From, SDK: unit.SDK, Stages: unit.Stages, SourceMirrors: r.SourceMirrors, Executables: unit.Executables}
}

type unitResults struct {
	dir  string
	info *oci.Build
}

func (u *unitResults) close() {
	if u.dir != "" {
		_ = os.RemoveAll(u.dir)
	}
}

func executeBuildUnits(ctx *context.Context, r *recipe.Recipe, recipePath, source string, projects *Snapshots, opts Options) (*unitResults, error) {
	result := &unitResults{info: &oci.Build{SDK: map[string]string{}, Vendor: map[string]string{}}}
	order, err := r.ReachableBuilds()
	if err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return result, nil
	}
	result.dir, err = ctx.Store.MakeTmpDir("build-unit-outputs")
	if err != nil {
		return nil, err
	}
	complete := false
	defer func() {
		if !complete {
			result.close()
		}
	}()
	definition, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	for index, unit := range order {
		name := strings.ToLower(unit.Name)
		ctx.Note("building unit %s", unit.Name)
		local := unitRecipe(r, unit)
		inputs := projects.units[name]
		var work *Work
		if len(r.Host) == 0 && wantedWork(local, inputs) {
			work, err = initWork(ctx, local, definition, recipePath, source+"\x00build:"+name, inputs)
			if err != nil {
				return nil, err
			}
			if opts.Clean {
				if err := work.drop(); err != nil {
					return nil, err
				}
			}
		}
		scope, err := executeScope(ctx, local, recipePath, inputs, work, opts)
		if err != nil {
			return nil, fmt.Errorf("%s: build %s: %w", recipePath, unit.Name, err)
		}
		if _, err := scope.tree.ExecutableView(unit.Executables); err != nil {
			scope.close(ctx, opts.KeepBuildDir)
			return nil, fmt.Errorf("%s: build %s: %w", recipePath, unit.Name, err)
		}
		// Capture bytes while source archives, dependency leases and the producing
		// drive are still alive. Each later unit receives ordinary file copies.
		output, captureErr := captureUnitOutput(scope.tree, filepath.Join(result.dir, fmt.Sprintf("unit-%03d", index)))
		scope.close(ctx, opts.KeepBuildDir)
		if captureErr != nil {
			return nil, fmt.Errorf("%s: build %s: %w", recipePath, unit.Name, captureErr)
		}
		projects.outputs[name] = output
		for alias, digest := range scope.info.SDK {
			result.info.SDK[name+"/sdk/"+alias] = digest
		}
		for alias, runtime := range scope.from {
			result.info.SDK[name+"/from/"+alias] = runtime.Image
		}
		for step, digest := range scope.info.Vendor {
			result.info.Vendor[name+"/"+step] = digest
		}
		result.info.Sources = append(result.info.Sources, scope.info.Sources...)
		result.info.Images = append(result.info.Images, scope.info.Images...)
		result.info.Network = result.info.Network || scope.info.Network
	}
	if err := projects.recheckGraph(ctx); err != nil {
		return nil, err
	}
	complete = true
	return result, nil
}

func captureUnitOutput(output *tree.Tree, directory string) (*tree.Tree, error) {
	if err := buildcontext.ValidateOutput(output); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0755); err != nil {
		return nil, err
	}
	if err := output.WriteFiles(directory); err != nil {
		return nil, err
	}
	return buildcontext.ReadOutput(directory)
}

func mergeUnitBuildInfo(target, units *oci.Build) {
	maps.Copy(target.SDK, units.SDK)
	maps.Copy(target.Vendor, units.Vendor)
	target.Network = target.Network || units.Network
	if len(units.Sources) > 0 {
		target.Sources = uniqueSorted(append(target.Sources, units.Sources...))
	}
	if len(units.Images) > 0 {
		target.Images = uniqueSorted(append(target.Images, units.Images...))
	}
}

func uniqueSorted(values []string) []string {
	slices.Sort(values)
	return slices.Compact(values)
}
