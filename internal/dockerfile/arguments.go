package dockerfile

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/quasidock/quasidock/internal/buildcontext"
	"github.com/quasidock/quasidock/internal/oci"
)

// A path index maps to the attached option prefix, or "" for a whole operand.
// Option values, patterns and code are consumed before classifying operands.
func argumentPaths(name string, args []commandWord) (map[int]string, error) {
	paths := map[int]string{}
	switch name {
	case "echo", "printf", "pwd", "true", "false", "tr", "npm":
		return paths, nil
	case "mkdir", "cp", "mv", "rm", "touch", "cat", "head", "tail", "grep", "sort", "uniq", "wc", "tee":
		options, pattern, count := true, name == "grep", 0
		for i := 0; i < len(args); i++ {
			a := args[i].value
			if options && a == "--" {
				options = false
				continue
			}
			if options && strings.HasPrefix(a, "-") && a != "-" {
				if strings.HasPrefix(a, "--") {
					return nil, fmt.Errorf("%s option %s is unsupported; use portable short options", name, a)
				}
				valueOption, valuePath := utilityValueOption(name, a)
				if valueOption != "" {
					if name == "grep" && (valueOption == "-e" || valueOption == "-f") {
						pattern = false
					}
					if len(a) > len(valueOption) {
						if valuePath {
							paths[i] = valueOption
						}
					} else {
						i++
						if i == len(args) {
							return nil, fmt.Errorf("%s %s needs a value", name, a)
						}
						if valuePath {
							paths[i] = ""
						}
					}
				} else if !utilityFlags(name, strings.TrimPrefix(a, "-")) {
					return nil, fmt.Errorf("%s option %s has no supported path translation", name, a)
				}
				continue
			}
			if pattern {
				pattern = false
				continue
			}
			if a != "-" || name == "mkdir" || name == "cp" || name == "mv" || name == "rm" || name == "touch" {
				paths[i] = ""
			}
			count++
		}
		if (name == "cp" || name == "mv") && count < 2 {
			return nil, fmt.Errorf("%s needs a source and destination", name)
		}
		return paths, nil
	case "python":
		for i := 0; i < len(args); i++ {
			a := args[i].value
			if strings.HasPrefix(a, "-c") {
				return paths, nil // code and script arguments are opaque
			}
			if a == "-m" {
				if i+1 < len(args) && args[i+1].value == "pip" {
					return pipPaths(args, i+2), nil
				}
				return paths, nil
			}
			if a == "-W" || a == "-X" {
				i++
				continue
			}
			if a == "--" && i+1 < len(args) {
				paths[i+1] = ""
				return paths, nil
			}
			if !strings.HasPrefix(a, "-") {
				paths[i] = ""
				return paths, nil
			}
		}
	case "node":
		for i := 0; i < len(args); i++ {
			a := args[i].value
			if a == "-e" || a == "--eval" || a == "-p" || a == "--print" || strings.HasPrefix(a, "--eval=") || strings.HasPrefix(a, "--print=") {
				break
			}
			if a == "-r" || a == "--require" || a == "--import" {
				i++
				continue
			}
			if !strings.HasPrefix(a, "-") {
				paths[i] = ""
				break
			}
		}
	case "go":
		for i := 1; i < len(args); i++ {
			arg := args[i]
			if arg.value == "-o" && i+1 < len(args) {
				i++
				paths[i] = ""
			} else if strings.HasPrefix(arg.value, "-o=") {
				paths[i] = "-o="
			} else if arg.value == "-ldflags" || arg.value == "-gcflags" || arg.value == "-asmflags" || arg.value == "-tags" || arg.value == "-mod" {
				i++
			} else if arg.value == "-modfile" || arg.value == "-overlay" || arg.value == "-pkgdir" {
				if i+1 < len(args) {
					i++
					paths[i] = ""
				}
			} else if strings.HasPrefix(arg.value, "/") {
				paths[i] = ""
			}
		}
	}
	return paths, nil
}

func utilityValueOption(name, arg string) (string, bool) {
	var data, files string
	switch name {
	case "mkdir":
		data = "m"
	case "touch":
		data, files = "dt", "r"
	case "head", "tail":
		data = "nc"
	case "grep":
		data, files = "eABCm", "f"
	case "sort":
		data, files = "kt", "oT"
	case "uniq":
		data = "fsw"
	}
	if len(arg) >= 2 {
		if strings.ContainsRune(data, rune(arg[1])) {
			return arg[:2], false
		}
		if strings.ContainsRune(files, rune(arg[1])) {
			return arg[:2], true
		}
	}
	return "", false
}

func utilityFlags(name, flags string) bool {
	allowed := ""
	switch name {
	case "mkdir":
		allowed = "pv"
	case "cp":
		allowed = "aRrpfivnHL"
	case "mv":
		allowed = "finv"
	case "rm":
		allowed = "frRiv"
	case "touch":
		allowed = "achm"
	case "cat":
		allowed = "AbeEnstTuv"
	case "head":
		allowed = "qv"
	case "tail":
		allowed = "qvfF"
	case "grep":
		allowed = "EFHhIiLlnqRrsvwxoc"
	case "sort":
		allowed = "bcdfgimMnrRsuzV"
	case "uniq":
		allowed = "cdiu"
	case "wc":
		allowed = "clmwL"
	case "tee":
		allowed = "ai"
	}
	return flags != "" && strings.Trim(flags, allowed) == ""
}

func pipPaths(args []commandWord, start int) map[int]string {
	paths := map[int]string{}
	for i := start; i < len(args); i++ {
		a := args[i].value
		if a == "-r" || a == "--requirement" || a == "-c" || a == "--constraint" || a == "--target" {
			if i+1 < len(args) {
				i++
				paths[i] = ""
			}
			continue
		}
		for _, flag := range []string{"--requirement=", "--constraint=", "--target=", "-r", "-c"} {
			if strings.HasPrefix(a, flag) && len(a) > len(flag) {
				paths[i] = flag
			}
		}
		if strings.HasPrefix(a, "/") || strings.HasPrefix(a, "./") || strings.HasPrefix(a, "../") {
			paths[i] = ""
		}
		if a == "-f" || a == "--find-links" {
			if i+1 < len(args) {
				i++
				if !strings.Contains(args[i].value, "://") {
					paths[i] = ""
				}
			}
		} else if strings.HasPrefix(a, "--find-links=") && !strings.Contains(a, "://") {
			paths[i] = "--find-links="
		}
	}
	return paths
}

func logicalPath(wd string, word commandWord, allowGlob bool) (string, error) {
	if !word.resolved {
		return "", fmt.Errorf("path %q needs a declared ARG/ENV value", word.raw)
	}
	value := word.value
	if value == "" {
		return "", errors.New("path operand must not be empty")
	}
	if word.split {
		return "", errors.New("unquoted variable can split a path into multiple operands; quote the variable")
	}
	if wd == "" && !strings.HasPrefix(value, "/") {
		return "", errors.New("relative path has an ambiguous working directory; use && after cd or an absolute app path")
	}
	if !word.glob {
		return appPath(wd, value)
	}
	if !allowGlob {
		return "", errors.New("this path must be literal, without shell globbing")
	}
	if strings.ContainsAny(value, "\\:$\x00") {
		return "", fmt.Errorf("path %q has no supported native mapping", value)
	}
	if word.pattern != "" {
		value = word.pattern
	}
	if strings.HasPrefix(value, "/") {
		wd = "."
		value = strings.TrimLeft(value, "/")
	}
	p := path.Join(escapePattern(wd), value)
	if !oci.IsSafeRelPath(p) {
		return "", fmt.Errorf("path %q escapes the app", value)
	}
	if _, err := shellMatch(p, ""); err != nil {
		return "", fmt.Errorf("invalid shell glob %q: %w", value, err)
	}
	for _, part := range strings.Split(p, "/") {
		// Validate the non-pattern spelling as a Windows filename too.
		clean := strings.Map(func(r rune) rune {
			if strings.ContainsRune("\\*?[]", r) {
				return 'x'
			}
			return r
		}, part)
		if part != "." && !buildcontext.ValidName(clean) {
			return "", fmt.Errorf("path %q is not Windows-compatible", value)
		}
	}
	return p, nil
}

func nativePath(p string, runtime bool) string {
	return nativePathWord(p, false, runtime)
}

func nativePathWord(p string, glob, runtime bool) string {
	if runtime {
		return quote("${app}/" + p)
	}
	if !glob {
		return buildPath(p)
	}
	return `"$PREFIX"/` + globQuote(p)
}

// Keep glob metacharacters active without allowing path text to become shell
// syntax. Bracket contents are individually quoted except for safe ranges.
func globQuote(p string) string {
	var out strings.Builder
	for len(p) > 0 {
		i := strings.IndexAny(p, "\\*?[")
		if i < 0 {
			out.WriteString(quote(p))
			break
		}
		if i > 0 {
			out.WriteString(quote(p[:i]))
		}
		if p[i] == '\\' && i+1 < len(p) {
			out.WriteString(quote(p[i+1 : i+2]))
			p = p[i+2:]
		} else if p[i] == '[' {
			end := i + 1
			for end < len(p) && p[end] != ']' {
				if p[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(p) { // logicalPath normally rejects this first.
				out.WriteString(quote(p[i:]))
				break
			}
			out.WriteByte('[')
			for j := i + 1; j < end; {
				escaped := p[j] == '\\'
				if escaped {
					j++
				}
				r, size := utf8.DecodeRuneInString(p[j:end])
				value := p[j : j+size]
				j += size
				if !escaped && strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-!^", r) {
					out.WriteRune(r)
				} else {
					out.WriteString(quote(value))
				}
			}
			out.WriteByte(']')
			p = p[end+1:]
		} else {
			out.WriteByte(p[i])
			p = p[i+1:]
		}
	}
	return out.String()
}

// Go's path matcher spells bracket negation with '^'; POSIX shells also use '!'.
func shellMatch(pattern, name string) (bool, error) {
	var normalized strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		normalized.WriteByte(c)
		if c == '\\' && i+1 < len(pattern) {
			i++
			normalized.WriteByte(pattern[i])
		} else if c == '[' && i+1 < len(pattern) && pattern[i+1] == '!' {
			i++
			normalized.WriteByte('^')
		}
	}
	return path.Match(normalized.String(), name)
}
