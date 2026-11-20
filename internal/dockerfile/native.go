package dockerfile

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/quasidock/quasidock/internal/tree"
)

type nativeCommand struct{ tool, path, buildName string }

var nativeCommands = map[string]nativeCommand{
	"python": {"python", "python.exe", "python"},
	"node":   {"node", "node.exe", "node"},
	"npm":    {"node", "npm.cmd", "npm"},
	"go":     {"go", "bin/go.exe", "go"},
	"git":    {"git", "cmd/git.exe", "git.exe"},
	"make":   {"make", "bin/make.exe", "make.exe"},
	"cmake":  {"cmake", "bin/cmake.exe", "cmake.exe"},
	"ninja":  {"cmake", "bin/ninja.exe", "ninja.exe"},
	"cc":     {"compiler", "sdk-bin/cc.exe", "cc.exe"},
	"c++":    {"compiler", "sdk-bin/c++.exe", "c++.exe"},
}

func canonicalCommand(name string) string {
	if path.Dir(name) == "/usr/bin" || path.Dir(name) == "/usr/local/bin" || name == "/usr/local/go/bin/go" {
		name = path.Base(name)
	} else if strings.ContainsAny(name, "/\\") {
		return name
	}
	candidate := strings.TrimSuffix(strings.TrimSuffix(name, ".exe"), ".cmd")
	switch candidate {
	case "python3":
		candidate = "python"
	case "nodejs":
		candidate = "node"
	case "gcc", "clang":
		candidate = "cc"
	case "g++", "clang++":
		candidate = "c++"
	}
	if _, exists := nativeCommands[candidate]; exists {
		return candidate
	}
	return name
}

func compilerCommand(name string) bool { return name == "cc" || name == "c++" }

func (s *state) compilerOutput(args []commandWord, wd string) ([]commandWord, error) {
	link, input, output := true, false, -1
	for i := 0; i < len(args); i++ {
		a := args[i].value
		switch a {
		case "-c", "-S", "-E", "--version", "-dumpmachine", "-dumpversion":
			link = false
		case "-shared", "-r":
			return nil, errors.New("direct compiler commands support objects and Windows executables, not shared or relocatable linking")
		case "-target", "--target", "--sysroot", "-isysroot":
			return nil, errors.New("compiler target and sysroot overrides are unsupported; native tools target windows/amd64")
		case "-o":
			i++
			if i >= len(args) || output >= 0 {
				return nil, errors.New("compiler needs one output after -o")
			}
			output = i
		default:
			if strings.HasPrefix(a, "--target=") || strings.HasPrefix(a, "--sysroot=") || strings.HasPrefix(a, "-target=") || strings.HasPrefix(a, "-isysroot") || strings.HasPrefix(a, "-m32") {
				return nil, errors.New("compiler target and sysroot overrides are unsupported; native tools target windows/amd64")
			}
			if strings.HasPrefix(a, "-o") && len(a) > 2 {
				if output >= 0 {
					return nil, errors.New("compiler needs one -o output")
				}
				word := args[i]
				if word.dynamic || word.glob || !word.resolved {
					return nil, errors.New("compiler output must be a literal app path")
				}
				args = append(args[:i:i], append([]commandWord{literalWord("-o"), literalWord(strings.TrimPrefix(word.value, "-o"))}, args[i+1:]...)...)
				i++
				output = i
			} else if !strings.HasPrefix(a, "-") {
				input = true
			}
		}
	}
	if !link || !input {
		return args, nil
	}
	if output < 0 {
		return nil, errors.New("compiler executable links need an explicit -o output")
	}
	p, err := logicalPath(wd, args[output], false)
	if err != nil || p == "." || strings.HasSuffix(args[output].value, "/") {
		return nil, errors.New("compiler output must name one app executable")
	}
	if strings.EqualFold(path.Ext(p), ".dll") || strings.EqualFold(path.Ext(p), ".so") {
		return nil, errors.New("direct compiler linking must produce a Windows executable")
	}
	mapped := tree.NativeExecutable(p)
	if err := s.registerOutput(p, mapped); err != nil {
		return nil, err
	}
	args[output] = literalWord("/" + mapped)
	return args, nil
}

func nativeArgumentPaths(name string, args []commandWord) (map[int]string, error) {
	paths := map[int]string{}
	var options []string
	switch name {
	case "git":
		options = []string{"-C", "--git-dir", "--work-tree"}
	case "make":
		options = []string{"-C", "-f", "-I", "--directory", "--file", "--include-dir"}
	case "cmake":
		options = []string{"-S", "-B", "--build", "--install", "--prefix"}
	case "ninja":
		options = []string{"-C", "-f"}
	case "cc", "c++":
		options = []string{"-o", "-I", "-L", "-isystem", "-iquote", "-include"}
	}
	for i := 0; i < len(args); i++ {
		a := args[i].value
		matched := false
		for _, option := range options {
			if a == option {
				i++
				if i >= len(args) {
					return nil, fmt.Errorf("%s %s needs a path", name, option)
				}
				paths[i], matched = "", true
				break
			}
			prefix := option
			if strings.HasPrefix(option, "--") {
				prefix += "="
			}
			if strings.HasPrefix(a, prefix) && len(a) > len(prefix) {
				paths[i], matched = prefix, true
				break
			}
		}
		if matched {
			continue
		}
		if compilerCommand(name) {
			if a == "-Xlinker" || a == "-Xclang" {
				return nil, fmt.Errorf("%s compiler escape options are unsupported", a)
			}
			if a == "-x" || a == "-D" || a == "-U" || a == "-l" {
				i++
				if i >= len(args) {
					return nil, fmt.Errorf("%s needs a value", a)
				}
				continue
			}
			if !strings.HasPrefix(a, "-") && a != "" {
				paths[i] = ""
				continue
			}
			if !compilerFlag(a) {
				return nil, fmt.Errorf("compiler option %q has no supported operand translation; use a native recipe", a)
			}
		}
		if strings.HasPrefix(a, "/") || (strings.HasPrefix(a, "-") && strings.Contains(a, "/") && !strings.Contains(a, "://")) {
			return nil, fmt.Errorf("%s argument %q has no supported absolute Linux path mapping; use a relative app path", name, a)
		}
	}
	return paths, nil
}

func compilerFlag(option string) bool {
	switch option {
	case "-c", "-S", "-E", "-v", "--version", "-dumpmachine", "-dumpversion", "-m64", "-static", "-static-libgcc", "-static-libstdc++", "-pthread", "-pipe", "-pedantic", "-pedantic-errors", "-s", "-g", "-O":
		return true
	}
	for _, prefix := range []string{"-O", "-g", "-W", "-f", "-std=", "-D", "-U", "-l"} {
		if strings.HasPrefix(option, prefix) && len(option) > len(prefix) {
			return true
		}
	}
	return false
}
