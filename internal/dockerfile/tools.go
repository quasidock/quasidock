package dockerfile

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/quasidock/quasidock/internal/oci"
)

var toolImages = map[string]string{
	"python": "python-sdk", "node": "node", "go": "go", "git": "mingit",
	"php": "php", "composer": "composer",
	"make": "make", "cmake": "cmake", "compiler": "llvm-mingw",
}

type toolRequest struct {
	image, version, override, instruction string
	origin, line                          int
	base                                  bool
}

func (s *state) stageIndex() int {
	if s.input != nil {
		return s.input.index
	}
	return 0
}

func (s *state) hasTool(alias string) bool {
	_, ok := s.tools[alias]
	return ok
}

func (s *state) toolOverride(alias string) string {
	override := s.opts.Tools[alias]
	if s.graph != nil {
		if local := s.graph.toolOverrides[s.stageIndex()][alias]; local != "" {
			override = local
		}
	}
	return override
}

func (s *state) addTool(alias, version, legacyOverride string, base bool) error {
	if s.hasTool(alias) {
		return nil
	}
	override := s.toolOverride(alias)
	if legacyOverride != "" {
		if override != "" {
			return fmt.Errorf("native tool %s has conflicting base and tool overrides", alias)
		}
		override = legacyOverride
	}
	instruction := "RUN"
	if base {
		instruction = "FROM"
	}
	s.tools[alias] = toolRequest{toolImages[alias], version, override, instruction, s.stageIndex(), s.currentLine, base}
	if alias == "composer" {
		return s.addTool("php", "", "", base)
	}
	return nil
}

func (s *state) validateStageTools() {
	if s.graph == nil {
		return
	}
	for _, alias := range slices.Sorted(maps.Keys(s.graph.toolOverrides[s.stageIndex()])) {
		request, exists := s.tools[alias]
		if !exists || request.origin != s.stageIndex() {
			s.diagnostic(s.fromLine, "--stage-tool", fmt.Sprintf("tool %s is not introduced in this stage; override the stage that first supplies it", alias), false)
		}
	}
}

func (s *state) resolveTools(resolve func(string, string, string) (Dependency, error)) {
	for _, alias := range slices.Sorted(maps.Keys(s.tools)) {
		request := s.tools[alias]
		if request.origin != s.stageIndex() {
			continue
		}
		dep, err := resolve(request.image, request.version, request.override)
		if err != nil {
			s.diagnostic(request.line, request.instruction, fmt.Sprintf("resolving %s: %v", alias, err), false)
			continue
		}
		if alias == "composer" {
			// Import only Composer's own layer. Its bundled PHP must not compete
			// with the stage's selected PHP, and composite runtimes are unsupported.
			s.installComposer(dep.Reference)
		} else {
			s.r.From[alias] = dep.Reference
		}
		if dep.Config.Permissions.Sandbox == oci.SandboxSoft || (request.base && s.r.Permissions.Sandbox != oci.SandboxSoft) {
			s.r.Permissions.Sandbox = dep.Config.Permissions.Sandbox
		}
		s.diagnostic(request.line, request.instruction, fmt.Sprintf("mapped to %s (%s)", dep.Reference, dep.Config.Version), true)
	}
}

// Validate selectors even for skipped stages without inspecting their commands
// or resolving their images. Stage name/index aliases must not hide duplicates.
func (g *stageGraph) validateToolOverrides(opts Options) []Diagnostic {
	g.toolOverrides = map[int]map[string]string{}
	var diagnostics []Diagnostic
	check := func(alias, image string) string {
		if _, ok := toolImages[alias]; !ok {
			return fmt.Sprintf("unknown native tool %q", alias)
		}
		if strings.TrimSpace(image) == "" {
			return fmt.Sprintf("tool %s needs a nonempty native image", alias)
		}
		return ""
	}
	for _, alias := range slices.Sorted(maps.Keys(opts.Tools)) {
		if message := check(alias, opts.Tools[alias]); message != "" {
			diagnostics = append(diagnostics, Diagnostic{File: opts.Dockerfile, Line: 1, Instruction: "--tool", Message: message})
		}
	}
	for _, selector := range slices.Sorted(maps.Keys(opts.StageTools)) {
		stage, err := g.overrideStage(selector)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{File: opts.Dockerfile, Line: 1, Instruction: "--stage-tool", Message: strings.ReplaceAll(err.Error(), "--stage-base", "--stage-tool")})
			continue
		}
		if g.toolOverrides[stage.index] == nil {
			g.toolOverrides[stage.index] = map[string]string{}
		}
		for _, alias := range slices.Sorted(maps.Keys(opts.StageTools[selector])) {
			image := opts.StageTools[selector][alias]
			message := check(alias, image)
			if _, exists := g.toolOverrides[stage.index][alias]; exists {
				message = fmt.Sprintf("duplicate --stage-tool overrides for %s in the same stage", alias)
			}
			if message != "" {
				diagnostics = append(diagnostics, g.diagnostic(opts, stage, stage.from.node, message))
				continue
			}
			g.toolOverrides[stage.index][alias] = image
		}
	}
	return diagnostics
}

func (g *stageGraph) unusedToolDiagnostics(opts Options, translated map[int]*state) []Diagnostic {
	used := map[string]bool{}
	for _, state := range translated {
		for alias := range state.tools {
			used[alias] = true
		}
	}
	var diagnostics []Diagnostic
	for _, alias := range slices.Sorted(maps.Keys(opts.Tools)) {
		if !used[alias] {
			diagnostics = append(diagnostics, Diagnostic{File: opts.Dockerfile, Line: 1, Instruction: "--tool", Message: "unused override for tool " + alias + " skipped", Warning: true})
		}
	}
	return diagnostics
}
