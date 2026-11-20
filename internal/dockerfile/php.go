package dockerfile

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/quasidock/quasidock/internal/recipe"
)

var phpTag = regexp.MustCompile(`^(?:([0-9]+(?:\.[0-9]+){0,2})(?:-cli)?|cli)(?:-(?:alpine[0-9.]*|bookworm|bullseye|trixie))?$`)
var phpServerTag = regexp.MustCompile(`(?:^|-)(?:apache|fpm|zts)(?:-|$)`)
var phpNativeTag = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+){0,2})(?:-([0-9]+))?$`)

// PHP's Windows packages may add a packaging revision to the PHP version.
func imageVersion(image, version string) string {
	if image == "php" {
		if match := phpNativeTag.FindStringSubmatch(version); match != nil {
			return match[1]
		}
	}
	return version
}

func (s *state) installComposer(reference string) {
	dest := ".composer"
	s.r.Stages = append([]recipe.Stage{{
		Name:    "composer",
		Sources: []recipe.Source{{Image: &reference, Dest: &dest}},
	}}, s.r.Stages...)
}

// CLI code, INI definitions, addresses, and script arguments are opaque. Only
// PHP's file/configuration options and the script/router itself name app paths.
func phpArgumentPaths(args []commandWord) (map[int]string, error) {
	paths := map[int]string{}
	lint, script := false, false
	for i := 0; i < len(args); i++ {
		a := args[i].value
		if a == "--" {
			return paths, nil // Everything after -- is passed to the script.
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			if script && !lint {
				return paths, nil
			}
			if a != "-" {
				paths[i] = ""
			}
			script = true
			if !lint {
				return paths, nil
			}
			continue
		}
		if a == "-l" || a == "--syntax-check" {
			lint = true
			continue
		}
		if strings.HasPrefix(a, "--") {
			option, _, attached := strings.Cut(a, "=")
			switch option {
			case "--php-ini", "--file", "--process-file", "--docroot", "--define", "--run", "--process-begin", "--process-code", "--process-end", "--server", "--rf", "--rc", "--re", "--rz", "--ri":
				file := option == "--php-ini" || option == "--file" || option == "--process-file" || option == "--docroot"
				if attached {
					if file {
						paths[i] = option + "="
					}
				} else {
					i++
					if i == len(args) {
						return nil, fmt.Errorf("php %s needs a value", option)
					}
					if file {
						paths[i] = ""
					}
				}
				if option == "--file" || option == "--run" {
					script = true
				}
			case "--interactive", "--no-php-ini", "--profile-info", "--help", "--usage", "--info", "--modules", "--syntax-highlight", "--syntax-highlighting", "--version", "--ini":
				if attached {
					return nil, fmt.Errorf("php option %s does not accept a value", option)
				}
			default:
				return nil, fmt.Errorf("php option %s has no supported path translation", a)
			}
			continue
		}
		option := a[:2]
		switch option {
		case "-c", "-f", "-F", "-t", "-d", "-r", "-B", "-R", "-E", "-S":
			file := strings.Contains("cfFt", option[1:])
			if len(a) > 2 {
				if file {
					paths[i] = option
				}
			} else {
				i++
				if i == len(args) {
					return nil, fmt.Errorf("php %s needs a value", option)
				}
				if file {
					paths[i] = ""
				}
			}
			if option == "-f" || option == "-r" {
				script = true
			}
		default:
			if strings.Trim(a[1:], "aneh?imsvwH") != "" {
				return nil, fmt.Errorf("php option %s has no supported path translation", a)
			}
		}
	}
	return paths, nil
}

func composerArgumentPaths(args []commandWord) (map[int]string, error) {
	paths := map[int]string{}
	command := false
	for i := 0; i < len(args); i++ {
		a := args[i].value
		if a == "--" {
			break
		}
		if a == "-g" || a == "--global" || strings.HasPrefix(a, "--global=") || (!command && (a == "global" || a == "self-update" || a == "selfupdate")) {
			return nil, fmt.Errorf("global Composer commands and self-update are unsupported; select versions with --tool or --stage-tool")
		}
		if a == "-d" || a == "--working-dir" {
			i++
			if i == len(args) {
				return nil, fmt.Errorf("composer %s needs a directory", a)
			}
			paths[i] = ""
		} else if strings.HasPrefix(a, "--working-dir=") {
			paths[i] = "--working-dir="
		} else if strings.HasPrefix(a, "-d") && !strings.HasPrefix(a, "--") && len(a) > 2 {
			paths[i] = "-d"
		} else if !strings.HasPrefix(a, "-") {
			command = true
		}
	}
	return paths, nil
}
