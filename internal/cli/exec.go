package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/run"
)

func parseExecOptions(ctx *context.Context, args []string) (run.ExecOptions, error) {
	var opts run.ExecOptions
	args = append([]string(nil), args...)
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
		if strings.HasPrefix(arg, "-e") && !strings.HasPrefix(arg, "--") {
			value := strings.TrimPrefix(arg[2:], "=")
			args[i] = "--env"
			if len(arg) > 2 {
				args[i] += "=" + value
			}
		} else if strings.HasPrefix(arg, "-w") && !strings.HasPrefix(arg, "--") {
			value := strings.TrimPrefix(arg[2:], "=")
			args[i] = "--workdir"
			if len(arg) > 2 {
				args[i] += "=" + value
			}
		} else if !strings.HasPrefix(arg, "--") {
			if len(arg) < 2 {
				return opts, fmt.Errorf("unknown exec option %q", arg)
			}
			for _, flag := range arg[1:] {
				switch flag {
				case 'd':
					opts.Detach = true
				case 'i':
					opts.Interactive = true
				case 't':
					opts.TTY = true
				default:
					return opts, fmt.Errorf("unknown exec option %q", arg)
				}
			}
			i++
			continue
		}
		opt, err := readOption(args, &i, []string{"detach", "interactive", "tty", "verbose"})
		if err != nil {
			return opts, err
		}
		switch opt.Name {
		case "detach", "interactive", "tty", "verbose":
			if opt.Value != nil {
				return opts, fmt.Errorf("--%s takes no value", opt.Name)
			}
			switch opt.Name {
			case "detach":
				opts.Detach = true
			case "interactive":
				opts.Interactive = true
			case "tty":
				opts.TTY = true
			case "verbose":
				ctx.Verbose = true
				if ctx.Store != nil {
					ctx.Store.Verbose = true
				}
			}
		case "env":
			opts.Env = append(opts.Env, *opt.Value)
		case "workdir":
			if *opt.Value == "" {
				return opts, errors.New("--workdir needs a nonempty directory")
			}
			opts.Workdir = *opt.Value
		default:
			return opts, fmt.Errorf("unknown exec option --%s", opt.Name)
		}
		i++
	}
	if len(args)-i < 2 || args[i] == "" || args[i+1] == "" {
		return opts, errors.New("usage: quasidock exec [OPTIONS] CONTAINER COMMAND [ARG...]")
	}
	opts.Container, opts.Args = args[i], args[i+1:]
	return opts, nil
}

func execCommand(ctx *context.Context, args []string) (uint32, error) {
	opts, err := parseExecOptions(ctx, args)
	if err != nil {
		return 1, err
	}
	return run.Exec(ctx, opts)
}
