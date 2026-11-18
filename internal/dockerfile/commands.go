package dockerfile

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/tree"
	"mvdan.cc/sh/v3/syntax"
)

func (s *state) run(line instructions.ShellDependantCmdLine, runtime bool) error {
	if len(line.CmdLine) == 0 {
		return errors.New("empty command")
	}
	var compiled string
	var err error
	if line.PrependShell {
		compiled, err = s.compileShell(line.CmdLine[0], runtime)
	} else {
		words := make([]commandWord, len(line.CmdLine))
		for i, arg := range line.CmdLine {
			if nativePlaceholder(arg) {
				return errors.New("literal Quasidock placeholders cannot be preserved in an exec command")
			}
			words[i] = literalWord(arg)
		}
		compiled, _, err = s.compileCall(words, nil, s.workdir, runtime)
	}
	if err != nil {
		return err
	}
	s.needShell = true
	s.addStage(recipe.Stage{Build: []string{"cd " + buildPath(s.workdir) + " && { " + compiled + "; }"}, Env: s.buildEnv(), Shell: recipe.ShellSh, Network: true})
	return nil
}

func (s *state) buildEnv() map[string]string {
	env := s.translatedEnv(true)
	if s.family == "python" {
		env["PYTHONPATH"] = "${app}/.python"
		env["PYTHONDONTWRITEBYTECODE"] = "1"
	}
	return env
}

func (s *state) commandName(name string) string {
	canonical := name
	switch name {
	case "/usr/local/bin/node", "node.exe":
		canonical = "node"
	case "/usr/local/bin/python", "/usr/local/bin/python3", "python3", "python.exe", "python3.exe":
		canonical = "python"
	case "/usr/local/go/bin/go", "go.exe":
		canonical = "go"
	case "npm.cmd":
		canonical = "npm"
	}
	if canonical == s.family || (canonical == "npm" && s.family == "node") {
		return canonical
	}
	return name
}

func busyboxCommand(name string) bool {
	switch name {
	case "cd", "mkdir", "cp", "rm", "echo", "printf", "pwd", "cat", "touch", "mv", "head", "tail", "grep", "sort", "uniq", "wc", "tr", "tee", "true", "false":
		return true
	}
	return false
}

func linuxCommand(name string) bool {
	switch name {
	case "apt", "apt-get", "apt-cache", "apk", "yum", "dnf", "dpkg", "rpm", "pacman", "sudo", "su":
		return true
	}
	return false
}

func (s *state) compileCall(words []commandWord, assigns []*syntax.Assign, wd string, runtime bool) (string, string, error) {
	if len(words) == 0 {
		return "", wd, errors.New("standalone assignments are unsupported; use ENV")
	}
	if !words[0].resolved || words[0].dynamic || words[0].glob {
		return "", wd, errors.New("command names must be literal native tool or app executable names")
	}
	name := s.commandName(words[0].value)
	args := append([]commandWord(nil), words[1:]...)
	prefix, err := s.assignments(assigns, runtime)
	if err != nil {
		return "", wd, err
	}
	if name == "pip" || name == "pip3" {
		name = "python"
		args = append([]commandWord{literalWord("-m"), literalWord("pip")}, args...)
	}
	if name == "python" {
		// Normalize the attached module spelling before applying pip's rules.
		for i := 0; i < len(args); i++ {
			a := args[i].value
			if strings.HasPrefix(a, "-m") {
				if len(a) > 2 {
					args = append(args[:i:i], append([]commandWord{literalWord("-m"), literalWord(a[2:])}, args[i+1:]...)...)
				}
				break
			}
			if strings.HasPrefix(a, "-c") || !strings.HasPrefix(a, "-") {
				break
			}
			if a == "-W" || a == "-X" {
				i++
			}
		}
	}
	allowed := false
	switch name {
	case "npm":
		allowed = s.family == "node" && len(args) > 0 && (args[0].value == "ci" || args[0].value == "install" || args[0].value == "run" || args[0].value == "prune" || (runtime && args[0].value == "start"))
		for _, arg := range args {
			a := arg.value
			if strings.HasPrefix(a, "-g") || a == "--global" || strings.HasPrefix(a, "--global=") || strings.HasPrefix(a, "--prefix") || a == "-C" || a == "--location=global" {
				return "", wd, errors.New("global npm installs and custom prefixes are unsupported")
			}
		}
	case "node":
		allowed = s.family == "node"
	case "python":
		allowed = s.family == "python"
	case "go":
		allowed = s.family == "go" && len(args) > 0 && (args[0].value == "build" || (len(args) > 1 && args[0].value == "mod" && args[1].value == "download") || (runtime && (args[0].value == "run" || args[0].value == "version")))
	default:
		allowed = !runtime && busyboxCommand(name)
	}
	if linuxCommand(name) {
		return "", wd, fmt.Errorf("command %q has no supported Windows translation; use a native recipe", name)
	}
	var invocation []string
	if !allowed || runtime {
		invocation, err = s.invocationAt(name, wd, runtime)
		if err != nil {
			return "", wd, fmt.Errorf("command %q has no supported Windows translation: %w", name, err)
		}
		if !allowed && (name == "npm" || name == "go" || name == "node" || name == "python") {
			return "", wd, fmt.Errorf("command %q has no supported Windows translation; use a native recipe", name)
		}
	}
	pip := pythonPip(args)
	if name == "python" && !runtime && pip >= 0 {
		if pip+1 >= len(args) || args[pip+1].value != "install" {
			return "", wd, errors.New("pip build commands support install only")
		}
		for _, arg := range args[pip+2:] {
			a := arg.value
			if a == "--user" || (strings.HasPrefix(a, "-e") && !strings.HasPrefix(a, "--")) || (strings.HasPrefix(a, "-t") && !strings.HasPrefix(a, "--")) || strings.HasPrefix(a, "--target") || strings.HasPrefix(a, "--prefix") || strings.HasPrefix(a, "--root") || strings.HasPrefix(a, "--editable") {
				return "", wd, errors.New("custom pip destinations and editable installs are unsupported")
			}
		}
		args = append(args, literalWord("--only-binary=:all:"), literalWord("--no-compile"), literalWord("--target"), literalWord("/.python"))
	}
	if name == "go" && !runtime && len(args) > 0 && args[0].value == "build" {
		found := false
		for i := 1; i < len(args); i++ {
			arg := args[i].value
			joined := strings.HasPrefix(arg, "-o=")
			if arg == "-o" && i+1 < len(args) {
				i++
			} else if !joined {
				continue
			}
			output := args[i]
			output.value = strings.TrimPrefix(output.value, "-o=")
			p, err := logicalPath(wd, output, false)
			if err != nil || strings.HasSuffix(output.value, "/") {
				return "", wd, fmt.Errorf("Go output must name one app executable: %s", output.value)
			}
			mapped := tree.NativeExecutable(p)
			if err := s.registerOutput(p, mapped); err != nil {
				return "", wd, err
			}
			value := "/" + mapped
			if joined {
				value = "-o=" + value
			}
			args[i] = literalWord(value)
			found = true
		}
		if !found {
			return "", wd, errors.New("Go builds need an explicit -o executable name")
		}
	}
	if name == "cd" && !runtime {
		if len(args) != 1 {
			return "", wd, errors.New("cd needs one directory")
		}
		p, err := logicalPath(wd, args[0], false)
		if err != nil {
			return "", wd, err
		}
		return prefix + "cd " + buildPath(p), p, nil
	}
	paths, err := argumentPaths(name, args)
	if err != nil {
		return "", wd, err
	}
	// Opaque native executable and console-command arguments are data.
	if !allowed {
		paths = map[int]string{}
	}
	before := maps.Clone(s.outputs)
	overrides, err := s.fileOperation(name, args, paths, wd, runtime)
	if err != nil {
		return "", wd, err
	}
	out := prefix + name
	if invocation != nil {
		out = prefix + renderInvocation(invocation, runtime)
	}
	for i, arg := range args {
		value := arg.render()
		if replacement, ok := overrides[i]; ok {
			value = nativePath(replacement, runtime)
		} else if flag, ok := paths[i]; ok {
			file := arg
			file.value = strings.TrimPrefix(file.value, flag)
			file.pattern = strings.TrimPrefix(file.pattern, flag)
			p, err := logicalPath(wd, file, true)
			if err != nil {
				return "", wd, err
			}
			if mapped, ok := before[p]; ok {
				p = mapped
			}
			if name == "mkdir" && !file.glob && !runtime {
				s.rememberDirectory(p)
			}
			if (name == "cp" || name == "mv") && (file.value == "." || strings.HasSuffix(file.value, "/.")) {
				p += "/."
			}
			value = flag + nativePathWord(p, file.glob, runtime)
		}
		out += " " + value
	}
	return out, wd, nil
}

func (s *state) assignments(assigns []*syntax.Assign, runtime bool) (string, error) {
	var prefix string
	for _, assign := range assigns {
		if assign.Append || assign.Index != nil || assign.Array != nil || assign.Naked || reserved(assign.Name.Value) {
			return "", errors.New("unsupported shell assignment")
		}
		word, err := s.shellWord(assign.Value, runtime)
		if err != nil {
			return "", err
		}
		if (assign.Name.Value == "GOOS" && word.value != "windows") || (assign.Name.Value == "GOARCH" && word.value != "amd64") {
			return "", errors.New("Go builds must target windows/amd64")
		}
		value := word.render()
		if strings.HasPrefix(word.value, "/") {
			p, err := logicalPath(".", word, false)
			if err != nil {
				return "", err
			}
			value = nativePath(p, runtime)
		}
		prefix += assign.Name.Value + "=" + value + " "
	}
	return prefix, nil
}

// File operations share COPY's logical executable namespace. Overrides rename
// individual operands when the native filename needs an .exe suffix.
func (s *state) fileOperation(name string, args []commandWord, paths map[int]string, wd string, runtime bool) (map[int]string, error) {
	overrides := map[int]string{}
	if runtime || (name != "cp" && name != "mv" && name != "rm") {
		return overrides, nil
	}
	var indexes []int
	var operands []string
	for i, arg := range args {
		if _, ok := paths[i]; !ok {
			continue
		}
		p, err := logicalPath(wd, arg, true)
		if err != nil {
			return nil, err
		}
		indexes, operands = append(indexes, i), append(operands, p)
	}
	if name != "rm" && len(operands) < 2 {
		return nil, fmt.Errorf("%s needs a source and destination", name)
	}
	previous := maps.Clone(s.outputs)
	previousDirectories := maps.Clone(s.directories)
	if name == "rm" {
		for _, operand := range operands {
			for logical, physical := range previous {
				if selectsPath(operand, logical) {
					if strings.ContainsAny(operand, "*?[") {
						if matches, _ := shellMatch(operand, physical); !matches {
							return nil, errors.New("glob cannot select a substituted executable; use its explicit logical path")
						}
					}
					delete(s.outputs, logical)
				}
			}
			for directory := range s.directories {
				if selectsPath(operand, directory) && directory != "." {
					delete(s.directories, directory)
				}
			}
		}
		return overrides, nil
	}
	dest := operands[len(operands)-1]
	if args[indexes[len(indexes)-1]].glob {
		return nil, fmt.Errorf("%s destination must be literal, without shell globbing", name)
	}
	directory := s.directories[dest] || strings.HasSuffix(args[indexes[len(indexes)-1]].value, "/")
	if len(operands) > 2 && !directory {
		return nil, fmt.Errorf("%s with multiple sources requires a known directory destination", name)
	}
	for i, source := range operands[:len(operands)-1] {
		contents := args[indexes[i]].value == "." || strings.HasSuffix(args[indexes[i]].value, "/.")
		for logical, physical := range previous {
			if !selectsPath(source, logical) {
				continue
			}
			if strings.ContainsAny(source, "*?[") {
				if matches, _ := shellMatch(source, physical); !matches {
					return nil, errors.New("glob cannot select a substituted executable; use its explicit logical path")
				}
			}
			target := dest
			if logical != source && !strings.ContainsAny(source, "*?[") {
				if directory && !contents {
					target = path.Join(target, path.Base(source))
				}
				target = path.Join(target, strings.TrimPrefix(logical, source+"/"))
			} else if directory {
				target = path.Join(target, path.Base(logical))
			}
			if name == "mv" {
				delete(s.outputs, logical)
			}
			if err := s.registerOutput(target, tree.NativeExecutable(target)); err != nil {
				return nil, err
			}
			if logical == source && !directory {
				overrides[indexes[len(indexes)-1]] = tree.NativeExecutable(target)
			}
		}
		if previousDirectories[source] {
			for old := range previousDirectories {
				if old == source || strings.HasPrefix(old, source+"/") {
					if name == "mv" && old != "." {
						delete(s.directories, old)
					}
					base := dest
					if directory && !contents {
						base = path.Join(base, path.Base(source))
					}
					s.rememberDirectory(path.Join(base, strings.TrimPrefix(strings.TrimPrefix(old, source), "/")))
				}
			}
		}
	}
	return overrides, nil
}

func selectsPath(selector, name string) bool {
	if selector == name || selector == "." || strings.HasPrefix(name, selector+"/") {
		return true
	}
	matched, _ := shellMatch(selector, name)
	return matched
}
