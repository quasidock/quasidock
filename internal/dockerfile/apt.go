package dockerfile

import (
	"errors"
	"fmt"
	"strings"
)

var aptPackages = map[string][]string{
	"python3": {"python"}, "python3-pip": {"python"}, "python3-dev": {"python"},
	"nodejs": {"node"}, "npm": {"node"}, "golang": {"go"}, "golang-go": {"go"},
	"php": {"php"}, "php-cli": {"php"}, "composer": {"composer"},
	"git": {"git"}, "make": {"make"}, "cmake": {"cmake"}, "ninja-build": {"cmake"},
	"clang": {"compiler"}, "gcc": {"compiler"}, "g++": {"compiler"},
	"build-essential": {"compiler", "make"},
}

func (s *state) compileApt(args []commandWord, runtime bool) (string, error) {
	if runtime || !s.apt {
		return "", errors.New("apt translation requires an Ubuntu/Debian-based build stage")
	}
	var words []string
	for _, arg := range args {
		if !arg.resolved || arg.glob || arg.split {
			return "", errors.New("apt arguments must resolve to individual literal options and package names")
		}
		if strings.HasPrefix(arg.value, "-") {
			switch arg.value {
			case "-y", "--yes", "--assume-yes", "-q", "-qq", "--no-install-recommends":
				continue
			default:
				return "", fmt.Errorf("unsupported apt option %q", arg.value)
			}
		}
		words = append(words, arg.value)
	}
	if len(words) == 0 {
		return "", errors.New("apt needs update, install, clean or autoclean")
	}
	switch words[0] {
	case "update", "clean", "autoclean":
		if len(words) != 1 {
			return "", fmt.Errorf("apt %s does not accept operands", words[0])
		}
		s.diagnostic(s.currentLine, "RUN", "apt "+words[0]+" omitted: native images do not use Linux package metadata", true)
	case "install":
		if s.aptForbidden != "" {
			return "", errors.New(s.aptForbidden)
		}
		if len(words) < 2 {
			return "", errors.New("apt install needs at least one supported package")
		}
		// Check the whole request before adding any tools.
		for _, pkg := range words[1:] {
			if _, ok := aptPackages[pkg]; !ok {
				return "", fmt.Errorf("apt package %q has no supported native mapping; version constraints and .deb files are unsupported; select native versions with --tool or --stage-tool", pkg)
			}
		}
		for _, pkg := range words[1:] {
			for _, tool := range aptPackages[pkg] {
				if err := s.addTool(tool, "", "", false); err != nil {
					return "", err
				}
			}
			message := fmt.Sprintf("apt package %s uses native %s", pkg, strings.Join(aptPackages[pkg], ", "))
			if pkg == "gcc" || pkg == "g++" || pkg == "build-essential" {
				message += "; GCC is replaced by Clang targeting Windows; Debian packaging tools and Linux headers are not reproduced"
			}
			s.diagnostic(s.currentLine, "RUN", message, true)
		}
	default:
		return "", fmt.Errorf("apt operation %q is unsupported; use update, install, clean or autoclean", words[0])
	}
	return "true", nil
}

func (s *state) aptCleanup(name string, args []commandWord, runtime bool) bool {
	if runtime || !s.apt || name != "rm" || len(args) != 2 {
		return false
	}
	if args[0].value != "-rf" || !args[0].resolved || args[0].dynamic {
		return false
	}
	if args[1].value != "/var/lib/apt/lists/*" || !args[1].resolved || args[1].dynamic || !args[1].glob {
		return false
	}
	s.diagnostic(s.currentLine, "RUN", "apt list cleanup omitted: no Linux package metadata is created", true)
	return true
}
