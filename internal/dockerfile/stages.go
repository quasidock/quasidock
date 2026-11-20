package dockerfile

import (
	"encoding/json"
	"maps"

	"github.com/quasidock/quasidock/internal/recipe"
)

func (g *stageGraph) convertStages(opts Options, resolver Resolver, selection stageSelection, overrides map[int]string) (Result, error) {
	result := Result{SourceLines: map[string]int{}, SourceStages: map[string]string{}}
	result.Diagnostics = append(result.Diagnostics, g.targetDiagnostic(opts, selection))
	translated := map[int]*state{}
	failed := false
	for _, stage := range selection.ordered {
		local := opts
		local.Base = overrides[stage.index]
		s := g.translateStage(stage, translated[stage.parent], local, translated)
		if len(s.r.Stages) == 0 {
			s.mkdir(".")
		}
		translated[stage.index] = s
		failed = failed || s.failed()
	}
	final := translated[selection.target.index]
	if !failed {
		if err := final.startup(); err != nil {
			final.diagnostic(final.startupLine, "CMD/ENTRYPOINT", err.Error(), false)
			failed = true
		}
	}
	// Validate all reachable commands before any metadata resolution. The cache
	// avoids resolving identical native toolchains repeatedly across stages.
	cache := map[string]Dependency{}
	resolve := func(image, version, override string) (Dependency, error) {
		key := image + "\x00" + version + "\x00" + override
		if dependency, exists := cache[key]; exists {
			return dependency, nil
		}
		dependency, err := resolver.Resolve(image, version, override)
		if err == nil {
			cache[key] = dependency
		}
		return dependency, err
	}
	if !failed {
		for _, stage := range selection.ordered {
			s := translated[stage.index]
			if stage.parent >= 0 {
				parent := translated[stage.parent]
				s.r.From = maps.Clone(parent.r.From)
				// BusyBox used by a parent's startup is not inherited into a
				// builder; startup is compiled only for the selected final stage.
				s.r.Permissions.Sandbox = parent.r.Permissions.Sandbox
			}
			s.resolveTools(resolve)
			if !s.failed() && s.needShell {
				dep, err := resolve("busybox", "", "")
				if err != nil {
					s.diagnostic(s.fromLine, "RUN", "resolving BusyBox: "+err.Error(), false)
				} else if s.runtimeShell {
					s.r.From["busybox"] = dep.Reference
				} else {
					s.r.SDK["busybox"] = dep.Reference
				}
			}
		}
	}
	for _, stage := range selection.ordered {
		s := translated[stage.index]
		result.Diagnostics = append(result.Diagnostics, s.result.Diagnostics...)
		failed = failed || s.failed()
		s.r.Executables = s.executablePaths()
		prefix := ""
		if stage != selection.target {
			prefix = stage.unitName() + "/"
			final.r.Builds = append(final.r.Builds, recipe.BuildUnit{Name: stage.unitName(), From: s.r.From, SDK: s.r.SDK, Stages: s.r.Stages, Executables: s.r.Executables})
		}
		for name, line := range s.result.SourceLines {
			result.SourceLines[prefix+name] = line
			result.SourceStages[prefix+name] = stage.label()
		}
	}
	for _, stage := range selection.skipped {
		d := g.diagnostic(opts, stage, stage.from.node, "unused stage skipped")
		if overrides[stage.index] != "" {
			d.Message = "unused stage and its native base override skipped"
		}
		if len(g.toolOverrides[stage.index]) > 0 {
			d.Message = "unused stage and its native tool overrides skipped"
		}
		d.Warning = true
		result.Diagnostics = append(result.Diagnostics, d)
	}
	result.Diagnostics = append(result.Diagnostics, g.unusedToolDiagnostics(opts, translated)...)
	if failed {
		return result, conversionError()
	}
	final.r.Workdir = final.workdir
	if final.r.Workdir == "" {
		final.r.Workdir = "."
	}
	final.r.Env = final.translatedEnv(false)
	if final.hasTool("python") {
		final.r.Env["PYTHONPATH"] = "${app}/.python"
		final.r.Env["PYTHONDONTWRITEBYTECODE"] = "1"
	}
	encoded, err := json.Marshal(final.r)
	if err == nil {
		_, err = recipe.Parse(opts.Output, encoded)
	}
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{File: opts.Dockerfile, Line: 1, Instruction: "recipe", Message: err.Error(), Stage: selection.target.label()})
		return result, conversionError()
	}
	result.Recipe = &final.r
	return result, nil
}
