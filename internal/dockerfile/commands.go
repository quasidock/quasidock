package dockerfile

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/quasidock/quasidock/internal/recipe"
	"mvdan.cc/sh/v3/syntax"
)

func (s *state) run(line instructions.ShellDependantCmdLine, runtime bool) error {
	text := ""
	if len(line.CmdLine) == 0 {
		return errors.New("empty command")
	}
	if line.PrependShell {
		text = line.CmdLine[0]
	} else {
		for _, arg := range line.CmdLine {
			if text != "" {
				text += " "
			}
			text += quote(arg)
		}
	}
	compiled, err := s.compileShell(text, runtime)
	if err != nil {
		return err
	}
	s.needShell = true
	env := s.translatedEnv(true)
	if s.family == "python" {
		env["PYTHONPATH"] = "${app}/.python"
	}
	s.addStage(recipe.Stage{Build: []string{"cd " + buildPath(s.workdir) + " && " + compiled}, Env: env, Shell: recipe.ShellSh, Network: true})
	return nil
}

func rawWord(word *syntax.Word) string {
	if word == nil {
		return ""
	}
	var b bytes.Buffer
	_ = syntax.NewPrinter().Print(&b, word)
	return b.String()
}

func (s *state) compileShell(text string, runtime bool) (string, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(text), "command")
	if err != nil {
		return "", err
	}
	if len(f.Stmts) != 1 {
		return "", errors.New("only a simple command or && chain is supported")
	}
	wd := s.workdir
	return s.compileStmt(f.Stmts[0], &wd, s.variables(), runtime)
}

func (s *state) compileStmt(stmt *syntax.Stmt, wd *string, env map[string]string, runtime bool) (string, error) {
	if stmt.Background || stmt.Negated || len(stmt.Redirs) != 0 {
		return "", errors.New("background commands, negation and redirection are unsupported")
	}
	switch command := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		if command.Op != syntax.AndStmt {
			return "", errors.New("only && command chains are supported; pipes and conditionals need a native recipe")
		}
		left, err := s.compileStmt(command.X, wd, env, runtime)
		if err != nil {
			return "", err
		}
		right, err := s.compileStmt(command.Y, wd, env, runtime)
		if err != nil {
			return "", err
		}
		return left + " && " + right, nil
	case *syntax.CallExpr:
		if len(command.Args) == 0 {
			return "", errors.New("standalone assignments are unsupported; use ENV")
		}
		prefix := ""
		for _, assign := range command.Assigns {
			if assign.Append || assign.Index != nil || assign.Array != nil || reserved(assign.Name.Value) {
				return "", errors.New("unsupported shell assignment")
			}
			value, err := s.expand(rawWord(assign.Value), env)
			if err != nil {
				return "", err
			}
			if (assign.Name.Value == "GOOS" && value != "windows") || (assign.Name.Value == "GOARCH" && value != "amd64") {
				return "", errors.New("Go builds must target windows/amd64")
			}
			assigned := quote(value)
			if strings.HasPrefix(value, "/") {
				p, err := appPath(".", value)
				if err != nil {
					return "", err
				}
				if runtime {
					assigned = quote("${app}/" + p)
				} else {
					assigned = buildPath(p)
				}
			}
			prefix += assign.Name.Value + "=" + assigned + " "
		}
		var args []string
		for _, word := range command.Args {
			for _, part := range word.Parts {
				if lit, ok := part.(*syntax.Lit); ok && (strings.ContainsAny(lit.Value, "*?[") || strings.HasPrefix(lit.Value, "~")) {
					return "", errors.New("shell globbing and home expansion are unsupported; use COPY selectors or a native recipe")
				}
			}
			valid := true
			syntax.Walk(word, func(n syntax.Node) bool {
				switch n.(type) {
				case *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ProcSubst:
					valid = false
				}
				return valid
			})
			if !valid {
				return "", errors.New("command substitution and arithmetic are unsupported")
			}
			if runtime && nativePlaceholder(rawWord(word)) {
				return "", errors.New("runtime shell variables collide with Quasidock placeholders; use unbraced variables or move the script into the context")
			}
			value, err := s.expand(rawWord(word), env)
			if err != nil && !runtime {
				return "", err
			}
			if err != nil {
				value = rawWord(word)
			}
			args = append(args, value)
		}
		name := args[0]
		args = args[1:]
		originalName := name
		switch name {
		case "/usr/local/bin/node":
			name = "node"
		case "/usr/local/bin/python", "/usr/local/bin/python3":
			name = "python"
		case "/usr/local/go/bin/go":
			name = "go"
		}
		if name == "python3" {
			name = "python"
		}
		if name == "pip" || name == "pip3" {
			if runtime {
				return "", errors.New("use python -m for Python startup commands")
			}
			name = "python"
			args = append([]string{"-m", "pip"}, args...)
		}
		allowed := false
		switch name {
		case "npm":
			allowed = s.family == "node" && len(args) > 0 && (args[0] == "ci" || args[0] == "install" || args[0] == "run" || args[0] == "prune" || (runtime && args[0] == "start"))
			for _, a := range args {
				if strings.HasPrefix(a, "-g") || a == "--global" || strings.HasPrefix(a, "--global=") || strings.HasPrefix(a, "--prefix") || a == "-C" || a == "--location=global" {
					return "", errors.New("global npm installs and custom prefixes are unsupported")
				}
			}
		case "node":
			allowed = s.family == "node"
		case "go":
			allowed = s.family == "go" && len(args) > 0 && (args[0] == "build" || (len(args) > 1 && args[0] == "mod" && args[1] == "download") || (runtime && (args[0] == "run" || args[0] == "version")))
		case "python":
			allowed = s.family == "python"
		case "cd", "mkdir", "cp", "rm":
			allowed = !runtime
		}
		if !allowed && runtime && (strings.Contains(name, "/") || strings.HasSuffix(strings.ToLower(name), ".exe")) {
			_, err := s.executable(name)
			allowed = err == nil
		}
		if !allowed {
			return "", fmt.Errorf("command %q has no supported Windows translation; use a native recipe", name)
		}
		if name == "python" && !runtime {
			if len(args) < 3 || args[0] != "-m" || args[1] != "pip" || args[2] != "install" {
				return "", errors.New("Python build commands currently support pip install only")
			}
			for _, a := range args {
				if a == "--user" || a == "-e" || a == "-t" || strings.HasPrefix(a, "-t=") || strings.HasPrefix(a, "--target") || strings.HasPrefix(a, "--prefix") || strings.HasPrefix(a, "--root") || strings.HasPrefix(a, "--editable") {
					return "", errors.New("custom pip destinations and editable installs are unsupported")
				}
			}
			args = append(args, "--only-binary=:all:", "--no-compile", "--target", "/.python")
		}
		originalArgs := append([]string(nil), args...)
		if name == "go" && !runtime && args[0] == "build" {
			found := false
			for i := 1; i < len(args); i++ {
				var output string
				if args[i] == "-o" && i+1 < len(args) {
					output = args[i+1]
					i++
				} else if strings.HasPrefix(args[i], "-o=") {
					output = strings.TrimPrefix(args[i], "-o=")
				} else {
					continue
				}
				p, err := appPath(*wd, output)
				if err != nil {
					return "", err
				}
				if strings.HasSuffix(output, "/") {
					return "", errors.New("Go output must name one executable")
				}
				mapped := p
				if !strings.HasSuffix(strings.ToLower(mapped), ".exe") {
					mapped += ".exe"
				}
				s.outputs[p] = mapped
				if strings.HasPrefix(args[i], "-o=") {
					args[i] = "-o=/" + mapped
				} else {
					args[i] = "/" + mapped
				}
				found = true
			}
			if !found {
				return "", errors.New("Go builds need an explicit -o executable name")
			}
		}
		if name == "cd" {
			if len(args) != 1 {
				return "", errors.New("cd needs one directory")
			}
			p, err := appPath(*wd, args[0])
			if err != nil {
				return "", err
			}
			*wd = p
			return prefix + "cd " + buildPath(p), nil
		}
		out := prefix + name
		if runtime {
			exe, err := s.executable(name)
			if err != nil {
				return "", err
			}
			out = prefix + quote(exe)
		}
		for i, arg := range args {
			value := quote(arg)
			sourceIndex := i + 1
			if originalName == "pip" || originalName == "pip3" {
				sourceIndex -= 2
			}
			raw := ""
			if sourceIndex > 0 && sourceIndex < len(command.Args) {
				raw = rawWord(command.Args[sourceIndex])
			}
			if i < len(originalArgs) && arg == originalArgs[i] && strings.Contains(raw, "$") && name != "mkdir" && name != "cp" && name != "rm" {
				value = raw
			} else if strings.HasPrefix(arg, "/") {
				p, err := appPath(".", arg)
				if err != nil {
					return "", err
				}
				if runtime {
					value = quote("${app}/" + p)
				} else {
					value = buildPath(p)
				}
			} else if strings.HasPrefix(arg, "-o=/") {
				value = "-o=" + buildPath(strings.TrimPrefix(arg, "-o=/"))
			} else if name == "mkdir" || name == "cp" || name == "rm" {
				if !strings.HasPrefix(arg, "-") {
					p, err := appPath(*wd, arg)
					if err != nil {
						return "", err
					}
					value = buildPath(p)
				}
			}
			out += " " + value
		}
		// Native paths use the same app-relative tree during build and launch.
		return out, nil
	default:
		return "", errors.New("loops, functions and compound shell commands are unsupported")
	}
}
