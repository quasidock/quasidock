package dockerfile

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

func (g *stageGraph) overrideStage(selector string) (*dockerStage, error) {
	if index, err := strconv.Atoi(selector); err == nil {
		if index < 0 || index >= len(g.stages) {
			return nil, fmt.Errorf("--stage-base stage index %q is out of range", selector)
		}
		return g.stages[index], nil
	} else if number, ok := err.(*strconv.NumError); ok && number.Err == strconv.ErrRange {
		return nil, fmt.Errorf("--stage-base stage index %q is out of range", selector)
	}
	if index, exists := g.names[strings.ToLower(selector)]; exists {
		return g.stages[index], nil
	}
	return nil, fmt.Errorf("unknown --stage-base stage %q", selector)
}

// Validate every supplied selector, including unused branches, before resolving
// any images. Resolve aliases to indexes without changing the caller's options.
func (g *stageGraph) baseOverrides(opts Options, selection stageSelection) (map[int]string, []Diagnostic) {
	overrides := map[int]string{}
	var diagnostics []Diagnostic
	selectors := make([]string, 0, len(opts.StageBases))
	for selector := range opts.StageBases {
		selectors = append(selectors, selector)
	}
	slices.Sort(selectors)
	for _, selector := range selectors {
		stage, err := g.overrideStage(selector)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{File: opts.Dockerfile, Line: 1, Instruction: "--stage-base", Message: err.Error()})
			continue
		}
		message := ""
		switch {
		case strings.TrimSpace(opts.StageBases[selector]) == "":
			message = "--stage-base needs NAME_OR_INDEX=IMAGE with a nonempty native image"
		case overrides[stage.index] != "":
			message = "duplicate --stage-base overrides for the same stage"
		case stage.parent >= 0 || stage.base == "scratch":
			message = "--stage-base requires a concrete external base; override the external ancestor of an inherited stage, and keep scratch empty"
		default:
			overrides[stage.index] = opts.StageBases[selector]
		}
		if message != "" {
			diagnostics = append(diagnostics, g.diagnostic(opts, stage, stage.from.node, message))
		}
	}
	if opts.Base != "" {
		target := selection.target
		message := ""
		switch {
		case target.parent >= 0 || target.base == "scratch":
			message = "--base requires a concrete external base on the selected target; use --stage-base on its external ancestor for an inherited target, and keep scratch empty"
		case overrides[target.index] != "":
			message = "--base conflicts with --stage-base for the selected target"
		default:
			overrides[target.index] = opts.Base
		}
		if message != "" {
			diagnostics = append(diagnostics, g.diagnostic(opts, target, target.from.node, message))
		}
	}
	return overrides, diagnostics
}

func (g *stageGraph) targetDiagnostic(opts Options, selection stageSelection) Diagnostic {
	d := g.diagnostic(opts, selection.target, selection.target.from.node, "selected conversion target")
	d.Warning = true
	return d
}
