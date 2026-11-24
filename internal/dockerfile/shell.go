package dockerfile

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func rawWord(word *syntax.Word) string {
	if word == nil {
		return "''"
	}
	var b bytes.Buffer
	_ = syntax.NewPrinter().Print(&b, word)
	return b.String()
}

type commandWord struct {
	value, raw, pattern            string
	resolved, dynamic, glob, split bool
}

func literalWord(value string) commandWord {
	return commandWord{value: value, raw: quote(value), resolved: true}
}

func (w commandWord) render() string {
	if w.dynamic || w.glob {
		return w.raw
	}
	return quote(w.value)
}

func (s *state) shellWord(word *syntax.Word, runtime bool) (commandWord, error) {
	raw := rawWord(word)
	if nativePlaceholder(raw) {
		return commandWord{}, errors.New("shell variables collide with Quasidock placeholders; use unbraced variables or move the script into the context")
	}
	out := commandWord{raw: raw}
	var invalid bool
	if word != nil {
		syntax.Walk(word, func(n syntax.Node) bool {
			switch n.(type) {
			case *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ProcSubst:
				invalid = true
			case *syntax.ParamExp:
				out.dynamic = true
			}
			return !invalid
		})
		for _, part := range word.Parts {
			if lit, ok := part.(*syntax.Lit); ok {
				if strings.HasPrefix(lit.Value, "~") {
					return out, errors.New("home expansion is unsupported; use an app-local path")
				}
				for i := 0; i < len(lit.Value); i++ {
					if lit.Value[i] == '\\' {
						i++
					} else if strings.ContainsRune("*?[", rune(lit.Value[i])) {
						out.glob = true
					}
				}
			}
		}
	}
	if invalid {
		return out, errors.New("command substitution and arithmetic are unsupported")
	}
	value, err := s.expand(raw, s.variables())
	out.value, out.resolved = value, err == nil
	if err != nil {
		out.value = raw
	} else if word != nil {
		var pattern strings.Builder
		for _, part := range word.Parts {
			switch p := part.(type) {
			case *syntax.Lit:
				for i := 0; i < len(p.Value); i++ {
					if p.Value[i] == '\\' && i+1 < len(p.Value) {
						i++
						if p.Value[i] != '\n' {
							pattern.WriteString(escapePattern(p.Value[i : i+1]))
						}
					} else {
						pattern.WriteByte(p.Value[i])
					}
				}
			default:
				value, err := s.expand(rawWord(&syntax.Word{Parts: []syntax.WordPart{part}}), s.variables())
				if err != nil {
					return out, err
				}
				if _, unquoted := part.(*syntax.ParamExp); unquoted {
					out.split = out.split || strings.ContainsAny(value, " \t\r\n")
					out.glob = out.glob || strings.ContainsAny(value, "*?[")
					pattern.WriteString(value)
				} else {
					pattern.WriteString(escapePattern(value))
				}
			}
		}
		out.pattern = pattern.String()
	}
	if runtime && out.glob {
		return out, errors.New("startup shell globbing is unsupported; use a script in the context")
	}
	return out, nil
}

func escapePattern(value string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[", "]", "\\]", "-", "\\-", "!", "\\!", "^", "\\^").Replace(value)
}

// Each statement has separate success and failure working directories. Empty
// denotes a join of different directories, which cannot map relative paths.
type shellFlow struct{ success, failure string }

func mergeDirectory(a, b string) string {
	if a == b {
		return a
	}
	return ""
}

func (s *state) compileShell(text string, runtime bool) (string, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(text), "command")
	if err != nil {
		return "", err
	}
	if len(f.Stmts) == 0 {
		return "", errors.New("empty command")
	}
	if runtime && len(f.Stmts) != 1 {
		return "", errors.New("startup supports only a simple command or && chain")
	}
	wd := s.workdir
	var out []string
	for _, stmt := range f.Stmts {
		compiled, flow, err := s.compileStmt(stmt, wd, runtime)
		if err != nil {
			return "", err
		}
		out = append(out, compiled)
		wd = mergeDirectory(flow.success, flow.failure)
	}
	return strings.Join(out, "; "), nil
}

func (s *state) compileStmt(stmt *syntax.Stmt, wd string, runtime bool) (string, shellFlow, error) {
	flow := shellFlow{wd, wd}
	if stmt.Background || stmt.Negated || stmt.Coprocess || stmt.Disown {
		return "", flow, errors.New("background commands and negation are unsupported")
	}
	var out string
	switch command := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		if command.Op != syntax.AndStmt && (runtime || (command.Op != syntax.OrStmt && command.Op != syntax.Pipe)) {
			return "", flow, errors.New("unsupported shell operator")
		}
		before := maps.Clone(s.outputs)
		beforeDirectories := maps.Clone(s.directories)
		previousRestriction := s.aptForbidden
		if command.Op != syntax.AndStmt {
			s.aptForbidden = "apt installs in || or pipelines are unsupported; use a separate RUN instruction"
		}
		defer func() { s.aptForbidden = previousRestriction }()
		left, x, err := s.compileStmt(command.X, wd, runtime)
		if err != nil {
			return "", flow, err
		}
		next := x.success
		switch command.Op {
		case syntax.OrStmt:
			next = x.failure
		case syntax.Pipe:
			next = wd
		}
		if command.Op == syntax.AndStmt && !s.aptSetupOnly(command.X) {
			s.aptForbidden = "apt installs gated by other commands are ambiguous; use a separate RUN instruction"
		}
		right, y, err := s.compileStmt(command.Y, next, runtime)
		if err != nil {
			return "", flow, err
		}
		switch command.Op {
		case syntax.AndStmt:
			flow = shellFlow{y.success, mergeDirectory(x.failure, y.failure)}
		case syntax.OrStmt:
			flow = shellFlow{mergeDirectory(x.success, y.success), y.failure}
		}
		if command.Op != syntax.AndStmt && (!maps.Equal(before, s.outputs) || !maps.Equal(beforeDirectories, s.directories)) {
			return "", flow, errors.New("conditional or piped path mappings are ambiguous; use separate RUN instructions")
		}
		out = left + " " + command.Op.String() + " " + right
	case *syntax.CallExpr:
		words := make([]commandWord, len(command.Args))
		for i, word := range command.Args {
			var err error
			words[i], err = s.shellWord(word, runtime)
			if err != nil {
				return "", flow, err
			}
		}
		if len(words) > 0 && (words[0].value == "apt" || words[0].value == "apt-get" || words[0].value == "/usr/bin/apt" || words[0].value == "/usr/bin/apt-get") && len(stmt.Redirs) != 0 {
			return "", flow, errors.New("apt redirection is unsupported")
		}
		var err error
		out, flow.success, err = s.compileCall(words, command.Assigns, wd, runtime)
		if err != nil {
			return "", flow, err
		}
	default:
		return "", flow, errors.New("loops, functions and compound shell commands are unsupported")
	}
	if runtime && len(stmt.Redirs) > 0 {
		return "", flow, errors.New("startup redirection is unsupported")
	}
	for _, redir := range stmt.Redirs {
		word, err := s.shellWord(redir.Word, runtime)
		if err != nil {
			return "", flow, err
		}
		fd := ""
		if redir.N != nil {
			fd = redir.N.Value
		}
		var target string
		switch redir.Op {
		case syntax.RdrIn, syntax.RdrOut, syntax.AppOut:
			if word.resolved && word.value == "/dev/null" {
				target = "NUL"
			} else {
				p, err := logicalPath(wd, word, false)
				if err != nil {
					return "", flow, err
				}
				target = nativePath(p, runtime)
			}
		case syntax.DplIn, syntax.DplOut:
			if !word.resolved || word.dynamic || (word.value != "-" && (word.value == "" || strings.Trim(word.value, "0123456789") != "")) {
				return "", flow, errors.New("descriptor redirection needs a literal descriptor number or '-'")
			}
			target = word.value
		default:
			return "", flow, fmt.Errorf("redirection %s is unsupported", redir.Op)
		}
		out += " " + fd + redir.Op.String() + target
	}
	return out, flow, nil
}

// An install may follow metadata/setup commands, but never an arbitrary command
// that could skip it and still allow a later semicolon-separated command to run.
func (s *state) aptSetupOnly(stmt *syntax.Stmt) bool {
	if len(stmt.Redirs) != 0 || stmt.Background || stmt.Negated {
		return false
	}
	switch command := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		return command.Op == syntax.AndStmt && s.aptSetupOnly(command.X) && s.aptSetupOnly(command.Y)
	case *syntax.CallExpr:
		if len(command.Args) == 0 {
			return false
		}
		word, err := s.shellWord(command.Args[0], false)
		return err == nil && word.resolved && (word.value == "apt" || word.value == "apt-get" || word.value == "/usr/bin/apt" || word.value == "/usr/bin/apt-get")
	}
	return false
}
