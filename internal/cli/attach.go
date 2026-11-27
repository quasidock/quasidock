package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/run"
)

type startOptions struct {
	targets             []string
	attach, interactive bool
	stream              run.AttachOptions
}

func parseStreamOptions(ctx *context.Context, args []string, start bool) (startOptions, error) {
	opts := startOptions{stream: run.AttachOptions{SigProxy: true}}
	streamFlag := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			opts.targets = append(opts.targets, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			opts.targets = append(opts.targets, arg)
			continue
		}
		if start && !strings.HasPrefix(arg, "--") && len(arg) > 1 {
			for _, c := range arg[1:] {
				switch c {
				case 'a':
					opts.attach = true
				case 'i':
					opts.interactive = true
				default:
					return opts, fmt.Errorf("unknown start option %q", arg)
				}
			}
			continue
		}
		if !strings.HasPrefix(arg, "--") {
			return opts, fmt.Errorf("unknown stream option %q", arg)
		}
		opt, err := readOption(args, &i, []string{"attach", "interactive", "no-stdin", "sig-proxy", "verbose"})
		if err != nil {
			return opts, err
		}
		switch opt.Name {
		case "sig-proxy":
			streamFlag = true
			if opt.Value != nil {
				if *opt.Value != "true" && *opt.Value != "false" {
					return opts, errors.New("--sig-proxy expects true or false")
				}
				opts.stream.SigProxy, _ = strconv.ParseBool(*opt.Value)
			}
		case "detach-keys":
			streamFlag = true
			if err := run.ValidateDetachKeys(*opt.Value); err != nil {
				return opts, err
			}
			opts.stream.DetachKeys = opt.Value
		case "attach", "interactive", "no-stdin", "verbose":
			if opt.Value != nil {
				return opts, fmt.Errorf("--%s takes no value", opt.Name)
			}
			switch opt.Name {
			case "attach":
				if !start {
					return opts, errors.New("attach does not support --attach")
				}
				opts.attach = true
			case "interactive":
				if !start {
					return opts, errors.New("attach uses the saved stdin preference; use --no-stdin to disable input")
				}
				opts.interactive = true
			case "no-stdin":
				if start {
					return opts, errors.New("start uses -i to attach stdin")
				}
				opts.stream.NoStdin = true
			case "verbose":
				ctx.Verbose = true
				if ctx.Store != nil {
					ctx.Store.Verbose = true
				}
			}
		default:
			return opts, fmt.Errorf("unknown stream option --%s", opt.Name)
		}
	}
	if len(opts.targets) == 0 {
		return opts, errors.New("a container name or ID is required")
	}
	for _, target := range opts.targets {
		if target == "" {
			return opts, errors.New("container name cannot be empty")
		}
	}
	if !start || opts.attach || opts.interactive {
		if len(opts.targets) != 1 {
			return opts, errors.New("attachment requires exactly one container")
		}
		opts.stream.Container = opts.targets[0]
		if start {
			opts.stream.NoStdin = !opts.interactive
		}
	} else if streamFlag {
		return opts, errors.New("--detach-keys and --sig-proxy require start -a or -i")
	}
	return opts, nil
}

func attachCommand(ctx *context.Context, args []string) (uint32, error) {
	opts, err := parseStreamOptions(ctx, args, false)
	if err != nil {
		return 1, err
	}
	return run.Attach(ctx, opts.stream)
}

func startCommand(ctx *context.Context, args []string) (uint32, error) {
	opts, err := parseStreamOptions(ctx, args, true)
	if err != nil {
		return 1, err
	}
	if opts.attach || opts.interactive {
		return run.StartAttached(ctx, opts.stream)
	}
	return containerLifecycle(ctx, "start", opts.targets)
}
