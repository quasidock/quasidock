package dockerfile

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/recipe"
)

// stageConfig is instruction-time state, separate from emitted recipe steps.
// Inheriting a Dockerfile stage copies this configuration, not its build steps.
type stageConfig struct {
	args, env, outputs                    map[string]string
	directories                           map[string]bool
	workdir, family, requested, baseImage string
	entryLine, cmdLine, startupLine       int
	entry, cmd                            *instructions.ShellDependantCmdLine
}

type state struct {
	stageConfig
	opts                              Options
	result                            Result
	r                                 recipe.Recipe
	globals                           map[string]string
	lex                               *shell.Lex
	stage                             string
	fromLine                          int
	seenFrom, needShell, runtimeShell bool
	graph                             *stageGraph
	input                             *dockerStage
	previous                          map[int]*state
	consoleStartup                    map[string]bool
}

func newState(opts Options, escape rune, globals map[string]string) *state {
	return &state{
		stageConfig: stageConfig{args: map[string]string{}, env: map[string]string{}, outputs: map[string]string{}, directories: map[string]bool{".": true}, workdir: "."},
		opts:        opts,
		result:      Result{SourceLines: map[string]int{}},
		r: recipe.Recipe{
			ID: opts.ID, Version: opts.Version, From: map[string]string{},
			SDK: map[string]string{}, Env: map[string]string{}, Workdir: ".",
			Path: []string{"."}, Permissions: oci.Permissions{Network: true},
		},
		globals: maps.Clone(globals),
		lex:     shell.NewLex(escape),
	}
}

func (s *state) inherit(parent *state) {
	s.stageConfig = parent.stageConfig
	s.args = maps.Clone(parent.args)
	s.env = maps.Clone(parent.env)
	s.outputs = maps.Clone(parent.outputs)
	s.directories = maps.Clone(parent.directories)
	s.entry = cloneCommand(parent.entry)
	s.cmd = cloneCommand(parent.cmd)
	s.r.Name = cloneString(parent.r.Name)
	s.r.Summary = cloneString(parent.r.Summary)
	s.r.Homepage = cloneString(parent.r.Homepage)
	s.r.License = cloneString(parent.r.License)
}

func (s *state) rememberDirectory(directory string) {
	for {
		s.directories[directory] = true
		if directory == "." {
			return
		}
		directory = path.Dir(directory)
	}
}

func (s *state) executablePaths() []string {
	return slices.Sorted(maps.Keys(s.outputs))
}

func (s *state) registerOutput(logical, native string) error {
	if logical == "." {
		return fmt.Errorf("an executable output must name a file")
	}
	for previous, physical := range s.outputs {
		if previous != logical && strings.EqualFold(physical, native) {
			return fmt.Errorf("ambiguous executable outputs %q and %q both map to %q", previous, logical, native)
		}
		first, second := strings.ToLower(previous), strings.ToLower(logical)
		if strings.HasPrefix(first, second+"/") || strings.HasPrefix(second, first+"/") {
			return fmt.Errorf("executable output %q conflicts with %q", logical, previous)
		}
	}
	s.outputs[logical] = native
	s.rememberDirectory(path.Dir(logical))
	return nil
}

func cloneCommand(command *instructions.ShellDependantCmdLine) *instructions.ShellDependantCmdLine {
	if command == nil {
		return nil
	}
	cloned := *command
	cloned.CmdLine = slices.Clone(command.CmdLine)
	cloned.Files = slices.Clone(command.Files)
	return &cloned
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
