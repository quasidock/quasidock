package dockerfile

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/quasidock/quasidock/internal/recipe"
)

// Only the app's installed distributions participate. SDK packages and pip's
// generated Windows launchers must not decide how an application is launched.
const consoleLookup = `import os, sys
from importlib.metadata import distributions
name = sys.argv.pop(1)
entries = [e for d in distributions(path=[os.environ['PYTHONPATH']]) for e in d.entry_points if e.group == 'console_scripts' and e.name == name]
if len(entries) != 1:
    sys.exit('Python console command %r: expected one installed entry point, found %d' % (name, len(entries)))
sys.argv[0] = name
`

const consoleDispatch = consoleLookup + "sys.exit(entries[0].load()())\n"

var consoleName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

func pythonPip(args []commandWord) int {
	for i := 0; i < len(args); i++ {
		a := args[i].value
		if a == "-m" {
			if i+1 < len(args) && args[i+1].value == "pip" {
				return i + 1
			}
			return -1
		}
		if strings.HasPrefix(a, "-c") || !strings.HasPrefix(a, "-") {
			return -1
		}
		if a == "-W" || a == "-X" {
			i++
		}
	}
	return -1
}

func (s *state) invocationAt(command, wd string, runtime bool) ([]string, error) {
	exe, err := s.executableAt(command, wd)
	if err == nil {
		return []string{exe}, nil
	}
	if s.family != "python" || !consoleName.MatchString(command) || linuxCommand(command) || busyboxCommand(command) {
		return nil, err
	}
	if runtime {
		if s.consoleStartup == nil {
			s.consoleStartup = map[string]bool{}
		}
		s.consoleStartup[command] = true
	}
	return []string{"${python}/python.exe", "-c", consoleDispatch, command}, nil
}

func renderInvocation(invocation []string, runtime bool) string {
	parts := make([]string, len(invocation))
	for i, arg := range invocation {
		parts[i] = quote(arg)
		if i == 0 && !runtime {
			switch {
			case strings.HasPrefix(arg, "${app}/"):
				parts[i] = buildPath(strings.TrimPrefix(arg, "${app}/"))
			case arg == "${python}/python.exe":
				parts[i] = "python"
			case arg == "${node}/node.exe":
				parts[i] = "node"
			case arg == "${node}/npm.cmd":
				parts[i] = "npm"
			case arg == "${go}/bin/go.exe":
				parts[i] = "go"
			}
		}
	}
	return strings.Join(parts, " ")
}

func (s *state) validateConsoleStartup() {
	names := make([]string, 0, len(s.consoleStartup))
	for name := range s.consoleStartup {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		s.needShell = true
		s.addStage(recipe.Stage{
			Build: []string{fmt.Sprintf("cd %s && python -c %s %s", buildPath(s.workdir), quote(consoleLookup), quote(name))},
			Shell: recipe.ShellSh,
			Env:   s.buildEnv(),
		})
	}
}
