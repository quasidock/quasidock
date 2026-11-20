// Package dockerfile translates a deliberately bounded Dockerfile subset into recipes.
package dockerfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/quasidock/quasidock/internal/buildcontext"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/recipe"
)

type Options struct {
	Context, Dockerfile, Output, ID, Version, Base, Target string
	BuildArgs, StageBases                                  map[string]string
	Tools                                                  map[string]string
	StageTools                                             map[string]map[string]string
}

type Dependency struct {
	Reference string
	Config    oci.AppConfig
}

// Resolver returns a pinned native image satisfying the requested numeric prefix.
// Override explicitly permits a version substitution, but must supply the same tools.
type Resolver interface {
	Resolve(image, version, override string) (Dependency, error)
}

type Diagnostic struct {
	File                 string
	Line                 int
	Instruction, Message string
	Warning              bool
	Stage                string
}

func (d Diagnostic) String() string {
	if d.Stage != "" {
		return fmt.Sprintf("%s:%d: %s: stage %s: %s", d.File, d.Line, d.Instruction, d.Stage, d.Message)
	}
	return fmt.Sprintf("%s:%d: %s: %s", d.File, d.Line, d.Instruction, d.Message)
}

type Result struct {
	Recipe       *recipe.Recipe
	Diagnostics  []Diagnostic
	SourceLines  map[string]int
	SourceStages map[string]string
}

func Convert(data []byte, opts Options, resolver Resolver) (Result, error) {
	if resolver == nil {
		return Result{}, errors.New("conversion requires an image resolver")
	}
	if frontend, _, location, ok := parser.DetectSyntax(data); ok {
		ref, digest, pinned := strings.Cut(strings.TrimPrefix(frontend, "docker.io/"), "@")
		if !standardFrontend.MatchString(ref) || (pinned && oci.DigestHex(digest) == "") {
			line := 1
			if len(location) > 0 {
				line = location[0].Start.Line
			}
			return Result{}, fmt.Errorf("%s:%d: syntax: custom Dockerfile frontends are unsupported", opts.Dockerfile, line)
		}
	}
	parsed, err := parser.Parse(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", opts.Dockerfile, err)
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.ID == "" {
		opts.ID = filepath.Base(opts.Context)
	}
	graph, diagnostics := parseStageGraph(parsed, opts)
	if len(diagnostics) > 0 {
		return Result{Diagnostics: diagnostics}, conversionError()
	}
	selection, err := graph.selectTarget(opts.Target)
	if err != nil {
		return Result{Diagnostics: []Diagnostic{{File: opts.Dockerfile, Line: 1, Instruction: "--target", Message: err.Error()}}}, conversionError()
	}
	overrides, diagnostics := graph.baseOverrides(opts, selection)
	diagnostics = append(diagnostics, graph.validateToolOverrides(opts)...)
	if len(diagnostics) > 0 {
		return Result{Diagnostics: diagnostics}, conversionError()
	}
	if len(graph.stages) > 1 {
		return graph.convertStages(opts, resolver, selection, overrides)
	}
	opts.Base = overrides[selection.target.index]
	s := graph.translateStage(selection.target, nil, opts, nil)
	if s.stage != "" {
		s.result.SourceStages = map[string]string{}
		for name := range s.result.SourceLines {
			s.result.SourceStages[name] = s.stage
		}
	}
	if opts.Target != "" {
		s.result.Diagnostics = append(s.result.Diagnostics, graph.targetDiagnostic(opts, selection))
	}
	if len(s.r.Stages) == 0 {
		s.mkdir(".")
	}
	if !s.failed() {
		if err := s.startup(); err != nil {
			s.diagnostic(s.startupLine, "CMD/ENTRYPOINT", err.Error(), false)
		}
	}
	if !s.failed() {
		s.resolveTools(resolver.Resolve)
	}
	if !s.failed() && s.needShell {
		dep, err := resolver.Resolve("busybox", "", "")
		if err != nil {
			s.diagnostic(1, "RUN", "resolving BusyBox: "+err.Error(), false)
		} else if s.runtimeShell {
			s.r.From["busybox"] = dep.Reference
		} else {
			s.r.SDK["busybox"] = dep.Reference
		}
	}
	if !s.failed() {
		s.r.Workdir = s.workdir
		if s.r.Workdir == "" {
			s.r.Workdir = "."
		}
		s.r.Env = s.translatedEnv(false)
		if s.hasTool("python") {
			s.r.Env["PYTHONPATH"] = "${app}/.python"
			s.r.Env["PYTHONDONTWRITEBYTECODE"] = "1"
		}
		s.r.Executables = s.executablePaths()
		encoded, err := json.Marshal(s.r)
		if err == nil {
			_, err = recipe.Parse(opts.Output, encoded)
		}
		if err != nil {
			s.diagnostic(1, "recipe", err.Error(), false)
		}
	}
	s.result.Recipe = &s.r
	s.result.Diagnostics = append(s.result.Diagnostics, graph.unusedToolDiagnostics(opts, map[int]*state{selection.target.index: s})...)
	if s.failed() {
		s.result.Recipe = nil
		return s.result, conversionError()
	}
	return s.result, nil
}

func (s *state) diagnostic(line int, instruction, message string, warning bool) {
	s.result.Diagnostics = append(s.result.Diagnostics, Diagnostic{File: s.opts.Dockerfile, Line: line, Instruction: instruction, Message: message, Warning: warning, Stage: s.stage})
}

func conversionError() error {
	return errors.New("Dockerfile conversion failed; correct the reported instructions and retry")
}
func (s *state) failed() bool {
	for _, d := range s.result.Diagnostics {
		if !d.Warning {
			return true
		}
	}
	return false
}
func (s *state) variables() map[string]string {
	out := maps.Clone(s.args)
	maps.Copy(out, s.env)
	return out
}

type envMap map[string]string

func (e envMap) Get(k string) (string, bool) { v, ok := e[k]; return v, ok }
func (e envMap) Keys() []string {
	var out []string
	for k := range e {
		out = append(out, k)
	}
	return out
}
func (s *state) expand(word string, env map[string]string) (string, error) {
	r, err := s.lex.ProcessWordWithMatches(word, envMap(env))
	if err != nil {
		return "", err
	}
	if len(r.Unmatched) != 0 {
		return "", fmt.Errorf("unresolved variable in %q; declare it with ARG/ENV or supply --build-arg", word)
	}
	return r.Result, nil
}

var numericTag = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+){0,2})(?:-(?:alpine[0-9.]*|slim|(?:bookworm|bullseye|trixie)(?:-slim)?))?$`)
var standardFrontend = regexp.MustCompile(`^docker/dockerfile:1(?:\.[0-9]+){0,2}$`)
var distroTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func (s *state) instruction(command any, node *parser.Node) error {
	s.currentLine = node.StartLine
	if _, ok := command.(*instructions.ArgCommand); !ok && !s.seenFrom {
		if _, ok := command.(*instructions.Stage); !ok {
			return errors.New("only ARG is supported before FROM")
		}
	}
	if len(node.Heredocs) > 0 {
		return errors.New("here-documents are unsupported; move the content into a context file")
	}
	switch c := command.(type) {
	case *instructions.Stage:
		if s.seenFrom {
			return errors.New("FROM must begin a new stage")
		}
		s.seenFrom = true
		s.fromLine = node.StartLine
		if c.Platform != "" {
			return errors.New("FROM --platform is unsupported; converted images target windows/amd64")
		}
		base, err := s.expand(c.BaseName, s.globals)
		if err != nil {
			return err
		}
		expandedBase := base
		base = strings.SplitN(base, "@", 2)[0]
		base = strings.TrimPrefix(strings.TrimPrefix(base, "docker.io/"), "library/")
		parts := strings.SplitN(base, ":", 2)
		switch parts[0] {
		case "node":
			s.family = "node"
		case "golang":
			s.family = "go"
		case "python":
			s.family = "python"
		case "php", "composer":
			s.family = parts[0]
		case "ubuntu", "debian":
			if len(parts) == 2 && !distroTag.MatchString(parts[1]) {
				return errors.New("distro base requires a valid nonempty image tag")
			}
			if strings.Contains(expandedBase, "@") {
				return errors.New("Linux distro image digests cannot be reused; use a distro tag and pin native tools with --tool or --stage-tool")
			}
			if s.opts.Base != "" {
				return errors.New("distro bases use --tool or --stage-tool to select native tools, rather than --base or --stage-base")
			}
			s.family, s.apt = parts[0], true
			s.diagnostic(node.StartLine, "FROM", fmt.Sprintf("%s supplies source distro metadata only; apt packages select independent native Windows tool versions", expandedBase), true)
			return nil
		case "scratch":
			if expandedBase != "scratch" {
				return errors.New("scratch must be an untagged, unqualified base")
			}
			s.family = "scratch"
		default:
			return fmt.Errorf("base %q is unsupported; use an official node, golang, python, php, composer, ubuntu or debian base, or scratch", c.BaseName)
		}
		hasOverride := s.opts.Base != "" || s.toolOverride(s.family) != ""
		if strings.Contains(expandedBase, "@") && !hasOverride {
			return errors.New("Linux image digests cannot be reused; select a native image with --base, --stage-base, --tool or --stage-tool")
		}
		if len(parts) == 2 && parts[1] != "latest" {
			pattern := numericTag
			if s.family == "php" {
				if phpServerTag.MatchString(parts[1]) {
					return errors.New("PHP Apache, FPM and ZTS variants have no native translation; use a PHP CLI base and an explicit native startup command")
				}
				pattern = phpTag
			}
			m := pattern.FindStringSubmatch(parts[1])
			if m == nil && !hasOverride {
				return errors.New("base tag is not a supported numeric version; select a native image with --base or --stage-base")
			}
			if m != nil {
				s.requested = m[1]
			}
		}
		if s.family != "scratch" {
			s.apt = s.family != "composer" && !strings.Contains(base, "alpine")
			if err := s.addTool(s.family, s.requested, s.opts.Base, true); err != nil {
				return err
			}
		}
	case *instructions.ArgCommand:
		for _, arg := range c.Args {
			value, supplied := s.opts.BuildArgs[arg.Key]
			if !supplied && arg.Value != nil {
				var err error
				value, err = s.expand(*arg.Value, s.variables())
				if !s.seenFrom {
					value, err = s.expand(*arg.Value, s.globals)
				}
				if err != nil {
					return err
				}
				supplied = true
			}
			if !supplied && s.seenFrom {
				value, supplied = s.globals[arg.Key]
			}
			if supplied {
				if nativePlaceholder(value) {
					return errors.New("literal Quasidock placeholders cannot be preserved in a build argument; use a native recipe")
				}
				if s.seenFrom {
					s.args[arg.Key] = value
				} else {
					s.globals[arg.Key] = value
				}
			}
		}
	case *instructions.EnvCommand:
		env := s.variables()
		for _, pair := range c.Env {
			if reserved(pair.Key) {
				return fmt.Errorf("ENV %s controls a managed Windows build path; set it manually in a native recipe", pair.Key)
			}
			value, err := s.expand(pair.Value, env)
			if err != nil {
				return err
			}
			if nativePlaceholder(value) {
				return errors.New("literal Quasidock placeholders cannot be preserved in ENV; move this value into application configuration")
			}
			if (pair.Key == "GOOS" && value != "windows") || (pair.Key == "GOARCH" && value != "amd64") {
				return errors.New("Linux/cross-architecture Go targets are unsupported")
			}
			s.env[pair.Key] = value
		}
	case *instructions.WorkdirCommand:
		value, err := s.expand(c.Path, s.variables())
		if err != nil {
			return err
		}
		wd, err := appPath(s.workdir, value)
		if err != nil {
			return err
		}
		s.workdir = wd
		s.mkdir(wd)
	case *instructions.CopyCommand:
		return s.copy(c, node)
	case *instructions.RunCommand:
		if len(node.Flags) != 0 {
			return errors.New("RUN flags and BuildKit mounts/secrets are unsupported")
		}
		previous := maps.Clone(s.outputs)
		if err := s.run(c.ShellDependantCmdLine, false); err != nil {
			return err
		}
		for _, logical := range s.executablePaths() {
			if native := s.outputs[logical]; native != logical && previous[logical] != native {
				s.diagnostic(node.StartLine, "RUN", fmt.Sprintf("mapped executable output %s to %s", logical, native), true)
			}
		}
	case *instructions.EntrypointCommand:
		v := c.ShellDependantCmdLine
		s.entry = &v
		s.cmd = nil
		s.entryLine, s.cmdLine = node.StartLine, 0
	case *instructions.CmdCommand:
		v := c.ShellDependantCmdLine
		s.cmd = &v
		s.cmdLine = node.StartLine
	case *instructions.LabelCommand:
		for _, pair := range c.Labels {
			v, err := s.expand(pair.Value, s.variables())
			if err != nil {
				return err
			}
			switch pair.Key {
			case "org.opencontainers.image.title":
				s.r.Name = &v
			case "org.opencontainers.image.description":
				s.r.Summary = &v
			case "org.opencontainers.image.url":
				s.r.Homepage = &v
			case "org.opencontainers.image.licenses":
				s.r.License = &v
			default:
				s.diagnostic(node.StartLine, "LABEL", "advisory label omitted: "+pair.Key, true)
			}
		}
	case *instructions.ExposeCommand:
		s.diagnostic(node.StartLine, "EXPOSE", "advisory ports "+strings.Join(c.Ports, ", ")+"; configure Quasidock services manually if needed", true)
	default:
		return fmt.Errorf("unsupported instruction %s; express this behavior in a native recipe", strings.ToUpper(node.Value))
	}
	return nil
}

func reserved(name string) bool {
	switch strings.ToUpper(name) {
	case "PATH", "HOME", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "PREFIX", "PYTHONPATH":
		return true
	}
	return false
}
func appPath(wd, value string) (string, error) {
	if strings.ContainsAny(value, "\\:$\x00") {
		return "", fmt.Errorf("path %q has no supported native mapping", value)
	}
	if strings.HasPrefix(value, "/") {
		wd = "."
		value = strings.TrimLeft(value, "/")
	}
	p := path.Join(wd, value)
	if !oci.IsSafeRelPath(p) {
		return "", fmt.Errorf("path %q escapes the app", value)
	}
	if p != "." {
		for _, part := range strings.Split(p, "/") {
			if !buildcontext.ValidName(part) {
				return "", fmt.Errorf("path %q is not Windows-compatible", value)
			}
		}
	}
	return p, nil
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func nativePlaceholder(value string) bool {
	for _, name := range []string{"app}", "data}", "cache}", "roots}", "node}", "go}", "python}", "php}", "composer}", "busybox}", "git}", "compiler}", "make}", "cmake}", "listen:", "port:", "uses:"} {
		if strings.Contains(value, "${"+name) {
			return true
		}
	}
	return false
}
func buildPath(p string) string {
	if p == "." || p == "" {
		return `"$PREFIX"`
	}
	return `"$PREFIX"/` + quote(p)
}
func (s *state) addStage(stage recipe.Stage) {
	stage.Name = fmt.Sprintf("step-%03d", len(s.r.Stages)+1)
	s.r.Stages = append(s.r.Stages, stage)
}
func (s *state) mkdir(wd string) {
	s.rememberDirectory(wd)
	s.needShell = true
	s.addStage(recipe.Stage{Build: []string{"mkdir -p " + buildPath(wd)}, Shell: recipe.ShellSh})
}

func (s *state) startup() error {
	s.startupLine = s.fromLine
	entry := s.entry
	cmd := s.cmd
	cmdOnly := false
	if entry != nil && len(entry.CmdLine) == 0 {
		entry = nil
	}
	if entry == nil {
		if s.cmdLine != 0 {
			s.startupLine = s.cmdLine
		}
		if cmd == nil {
			switch s.family {
			case "node":
				cmd = &instructions.ShellDependantCmdLine{CmdLine: []string{"node"}}
			case "python":
				cmd = &instructions.ShellDependantCmdLine{CmdLine: []string{"python"}}
			case "php", "composer":
				cmd = &instructions.ShellDependantCmdLine{CmdLine: []string{s.family}}
			case "scratch":
				return errors.New("scratch stages require CMD or ENTRYPOINT naming a native executable")
			case "ubuntu", "debian":
				return errors.New("distro stages require an explicit supported CMD or ENTRYPOINT")
			default:
				return errors.New("provide CMD or ENTRYPOINT for the Go application")
			}
		}
		entry, cmd = cmd, nil
		cmdOnly = true
	} else {
		s.startupLine = s.entryLine
	}
	if entry.PrependShell {
		text, err := s.compileShell(entry.CmdLine[0], true)
		if err != nil {
			return err
		}
		s.runtimeShell, s.needShell = true, true
		s.r.Entrypoint = recipe.Entrypoint{"${busybox}/busybox.exe", "sh", "-c", text}
	} else {
		if len(entry.CmdLine) == 0 {
			return errors.New("startup command is empty")
		}
		for _, arg := range entry.CmdLine {
			if nativePlaceholder(arg) {
				return errors.New("literal Quasidock placeholders cannot be preserved in an exec command; move the script into the context")
			}
		}
		invocation, err := s.invocationAt(entry.CmdLine[0], s.workdir, true)
		if err != nil {
			return err
		}
		s.r.Entrypoint = recipe.Entrypoint(invocation)
		arguments, err := s.startupArgs(entry.CmdLine[0], entry.CmdLine[1:])
		if err != nil {
			return err
		}
		for _, arg := range arguments {
			if cmdOnly {
				s.r.Cmd = append(s.r.Cmd, arg)
			} else {
				s.r.Entrypoint = append(s.r.Entrypoint, arg)
			}
		}
	}
	if cmd != nil {
		s.startupLine = s.cmdLine
		for _, arg := range cmd.CmdLine {
			if nativePlaceholder(arg) {
				return errors.New("literal Quasidock placeholders cannot be preserved in CMD; move the script into the context")
			}
		}
		if cmd.PrependShell {
			s.r.Cmd = []string{"/bin/sh", "-c", cmd.CmdLine[0]}
			return errors.New("shell-form CMD with ENTRYPOINT is unsupported; use a JSON argument array")
		}
		all := append(append([]string(nil), entry.CmdLine[1:]...), cmd.CmdLine...)
		arguments, err := s.startupArgs(entry.CmdLine[0], all)
		if err != nil {
			return err
		}
		s.r.Cmd = append(s.r.Cmd, arguments[len(entry.CmdLine)-1:]...)
	}
	s.validateConsoleStartup()
	return nil
}

func (s *state) translatedEnv(withArgs bool) map[string]string {
	env := maps.Clone(s.env)
	if withArgs {
		env = s.variables()
	}
	for k, v := range env {
		if strings.HasPrefix(v, "/") {
			env[k] = "${app}/" + strings.TrimPrefix(v, "/")
		}
	}
	return env
}

func (s *state) executableAt(command, wd string) (string, error) {
	if cmd, ok := nativeCommands[canonicalCommand(command)]; ok {
		if s.hasTool(cmd.tool) {
			return "${" + cmd.tool + "}/" + cmd.path, nil
		}
		if ext := path.Ext(command); ext != ".exe" && ext != ".cmd" && ext != ".bat" {
			return "", fmt.Errorf("native tool %s is not installed; install a supported apt package first", cmd.tool)
		}
	}
	if wd == "" && !strings.HasPrefix(command, "/") {
		return "", errors.New("app executable has an ambiguous working directory; use an absolute app path")
	}
	p, err := appPath(wd, command)
	if err != nil {
		return "", err
	}
	if out, ok := s.outputs[p]; ok {
		p = out
	} else if ext := strings.ToLower(path.Ext(p)); ext != ".exe" && ext != ".cmd" && ext != ".bat" {
		return "", fmt.Errorf("startup executable %q is unknown; use a language interpreter or an explicit native executable path", command)
	}
	return "${app}/" + p, nil
}
func (s *state) startupArgs(command string, args []string) ([]string, error) {
	words := make([]commandWord, len(args))
	for i, arg := range args {
		words[i] = literalWord(arg)
	}
	paths, err := argumentPaths(s.commandName(command), words)
	if err != nil {
		return nil, err
	}
	out := append([]string(nil), args...)
	for i, flag := range paths {
		word := words[i]
		word.value = strings.TrimPrefix(word.value, flag)
		if !strings.HasPrefix(word.value, "/") {
			continue
		}
		p, err := logicalPath(s.workdir, word, false)
		if err != nil {
			return nil, err
		}
		if mapped, ok := s.outputs[p]; ok {
			p = mapped
		}
		out[i] = flag + "${app}/" + p
	}
	for i, arg := range args {
		if strings.HasPrefix(arg, "/") {
			if p, err := appPath(".", arg); err == nil {
				if mapped, ok := s.outputs[p]; ok {
					out[i] = "${app}/" + mapped
				}
			}
		}
	}
	return out, nil
}
