package dockerfile

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/quasidock/quasidock/internal/recipe"
)

type stageInstruction struct {
	command any
	node    *parser.Node
}

// dockerStage describes a Dockerfile filesystem, not a recipe build step.
type dockerStage struct {
	index        int
	name, base   string
	parent       int // -1 means an external base, including scratch.
	from         stageInstruction
	commands     []stageInstruction
	dependencies []int
}

func (s *dockerStage) label() string {
	if s.name == "" {
		return strconv.Itoa(s.index)
	}
	return fmt.Sprintf("%s (%d)", s.name, s.index)
}

func (s *dockerStage) unitName() string { return fmt.Sprintf("docker-stage-%d", s.index) }

type stageGraph struct {
	stages        []*dockerStage
	names         map[string]int
	globals       map[string]string
	escape        rune
	toolOverrides map[int]map[string]string
}

type stageSelection struct {
	target           *dockerStage
	ordered, skipped []*dockerStage
}

// parseStageGraph checks syntax and references across the whole document.
// Native base and command compatibility are checked only during translation.
func parseStageGraph(parsed *parser.Result, opts Options) (*stageGraph, []Diagnostic) {
	g := &stageGraph{names: map[string]int{}, escape: parsed.EscapeToken}
	global := newState(opts, parsed.EscapeToken, map[string]string{})
	var diagnostics []Diagnostic
	var current *dockerStage
	for _, node := range parsed.AST.Children {
		command, err := instructions.ParseInstruction(node)
		if err != nil {
			diagnostics = append(diagnostics, g.diagnostic(opts, current, node, err.Error()))
			continue
		}
		instruction := stageInstruction{command: command, node: node}
		if from, ok := command.(*instructions.Stage); ok {
			current = &dockerStage{index: len(g.stages), name: from.Name, parent: -1, from: instruction}
			g.stages = append(g.stages, current)
			if from.Name != "" {
				if _, exists := g.names[from.Name]; exists {
					diagnostics = append(diagnostics, g.diagnostic(opts, current, node, fmt.Sprintf("duplicate stage name %q", from.Name)))
				} else {
					g.names[from.Name] = current.index
				}
			}
			continue
		}
		if current == nil {
			if err := global.instruction(command, node); err != nil {
				diagnostics = append(diagnostics, g.diagnostic(opts, nil, node, err.Error()))
			}
			continue
		}
		current.commands = append(current.commands, instruction)
	}
	g.globals = global.globals
	if len(g.stages) == 0 {
		diagnostics = append(diagnostics, Diagnostic{File: opts.Dockerfile, Line: 1, Instruction: "FROM", Message: "a supported base image is required"})
	}
	if len(diagnostics) != 0 {
		return g, diagnostics
	}
	for _, stage := range g.stages {
		from := stage.from.command.(*instructions.Stage)
		base, err := global.expand(from.BaseName, g.globals)
		if err != nil {
			diagnostics = append(diagnostics, g.diagnostic(opts, stage, stage.from.node, err.Error()))
			continue
		}
		stage.base = base
		if parent, exists := g.names[strings.ToLower(base)]; exists {
			// Official bases may share a name with the stage being declared
			// or a later stage, e.g. FROM node AS node. Earlier aliases win.
			if parent < stage.index {
				stage.parent = parent
				stage.dependencies = append(stage.dependencies, parent)
			} else if !externalBaseName(base) {
				diagnostics = append(diagnostics, g.diagnostic(opts, stage, stage.from.node, g.referenceError(stage, parent).Error()))
			}
		}
		for _, instruction := range stage.commands {
			copy, ok := instruction.command.(*instructions.CopyCommand)
			if !ok || copy.From == "" {
				continue
			}
			source, err := g.copySource(stage, copy.From)
			if err != nil {
				diagnostics = append(diagnostics, g.diagnostic(opts, stage, instruction.node, err.Error()))
				continue
			}
			stage.dependencies = append(stage.dependencies, source)
		}
	}
	return g, diagnostics
}

func externalBaseName(base string) bool {
	switch base {
	case "node", "golang", "python", "scratch", "ubuntu", "debian":
		return true
	}
	return false
}

func (g *stageGraph) copySource(stage *dockerStage, selector string) (int, error) {
	if index, err := strconv.Atoi(selector); err == nil {
		if index < 0 || index >= len(g.stages) {
			return 0, fmt.Errorf("COPY --from stage index %q is out of range", selector)
		}
		return index, g.referenceError(stage, index)
	} else if number, ok := err.(*strconv.NumError); ok && number.Err == strconv.ErrRange {
		return 0, fmt.Errorf("COPY --from stage index %q is out of range", selector)
	}
	if index, exists := g.names[strings.ToLower(selector)]; exists {
		return index, g.referenceError(stage, index)
	}
	return 0, fmt.Errorf("unknown COPY --from stage %q; external images and named contexts are unsupported", selector)
}

func (g *stageGraph) referenceError(stage *dockerStage, source int) error {
	if source == stage.index {
		return fmt.Errorf("stage %s cannot reference itself", stage.label())
	}
	if source > stage.index {
		return fmt.Errorf("forward reference to stage %s is unsupported; define it before stage %s", g.stages[source].label(), stage.label())
	}
	return nil
}

// selectTarget performs no native resolution or instruction translation, so an
// unused Linux-only branch cannot force tool downloads or compatibility errors.
func (g *stageGraph) selectTarget(name string) (stageSelection, error) {
	if len(g.stages) == 0 {
		return stageSelection{}, fmt.Errorf("a target requires at least one FROM stage")
	}
	target := len(g.stages) - 1
	if name != "" {
		var exists bool
		target, exists = g.names[strings.ToLower(name)]
		if !exists {
			return stageSelection{}, fmt.Errorf("unknown target stage %q", name)
		}
	}
	selection := stageSelection{target: g.stages[target]}
	visited := make([]uint8, len(g.stages))
	var visit func(int) error
	visit = func(index int) error {
		if visited[index] == 1 {
			return fmt.Errorf("circular dependency on stage %s", g.stages[index].label())
		}
		if visited[index] == 2 {
			return nil
		}
		visited[index] = 1
		for _, dependency := range g.stages[index].dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visited[index] = 2
		selection.ordered = append(selection.ordered, g.stages[index])
		return nil
	}
	if err := visit(target); err != nil {
		return stageSelection{}, err
	}
	for index, stage := range g.stages {
		if visited[index] == 0 {
			selection.skipped = append(selection.skipped, stage)
		}
	}
	return selection, nil
}

func (g *stageGraph) diagnostic(opts Options, stage *dockerStage, node *parser.Node, message string) Diagnostic {
	d := Diagnostic{File: opts.Dockerfile, Line: node.StartLine, Instruction: strings.ToUpper(node.Value), Message: message}
	if stage != nil {
		d.Stage = stage.label()
	}
	return d
}

// translateStage emits this stage's own instructions and an explicit filesystem
// source when it inherits another stage. Parent build commands are not repeated.
func (g *stageGraph) translateStage(stage *dockerStage, parent *state, opts Options, previous map[int]*state) *state {
	s := newState(opts, g.escape, g.globals)
	s.graph, s.input, s.previous = g, stage, previous
	if len(g.stages) > 1 || opts.Target != "" || len(opts.StageBases) > 0 || len(opts.StageTools) > 0 {
		s.stage = stage.label()
	}
	if stage.parent >= 0 {
		if parent == nil || parent.failed() {
			s.diagnostic(stage.from.node.StartLine, "FROM", "parent stage must be translated successfully before its child", false)
			return s
		}
		if stage.from.command.(*instructions.Stage).Platform != "" {
			s.diagnostic(stage.from.node.StartLine, "FROM", "FROM --platform is unsupported; converted images target windows/amd64", false)
			return s
		}
		s.inherit(parent)
		s.seenFrom = true
		s.fromLine = stage.from.node.StartLine
		dest := "./"
		s.addStage(recipe.Stage{Sources: []recipe.Source{{Build: &recipe.BuildSource{Name: g.stages[stage.parent].unitName(), Paths: []string{"."}}, Dest: &dest}}})
		s.result.SourceLines[s.r.Stages[0].Name] = stage.from.node.StartLine
	} else if err := s.instruction(stage.from.command, stage.from.node); err != nil {
		s.diagnostic(stage.from.node.StartLine, "FROM", err.Error(), false)
	}
	for _, instruction := range stage.commands {
		if err := s.instruction(instruction.command, instruction.node); err != nil {
			s.diagnostic(instruction.node.StartLine, strings.ToUpper(instruction.node.Value), err.Error(), false)
		}
	}
	s.validateStageTools()
	return s
}
