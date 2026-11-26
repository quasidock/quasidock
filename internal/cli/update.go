package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/run"
)

const renameUsage = "usage: quasidock rename CONTAINER NEW_NAME"
const updateUsage = "usage: quasidock update [--cpus NUMBER] [-m|--memory SIZE] [--pids-limit N] [--restart POLICY] CONTAINER [CONTAINER...]"

func parseUpdateSetting(o *run.UpdateOptions, name, value string) error {
	switch name {
	case "cpus":
		n, err := run.ParseCPUs(value)
		if err != nil {
			return err
		}
		o.NanoCPUs = &n
	case "memory":
		n, err := run.ParseMemory(value)
		if err != nil {
			return err
		}
		o.Memory = &n
	case "pids-limit":
		n, err := run.ParsePidsLimit(value)
		if err != nil {
			return err
		}
		o.PidsLimit = &n
	case "restart":
		p, err := run.ParseRestartPolicy(value)
		if err != nil {
			return err
		}
		o.RestartPolicy = &p
	default:
		return fmt.Errorf("unknown update option --%s", name)
	}
	return nil
}

func memoryOption(arg string) string {
	if arg == "-m" {
		return "--memory"
	}
	if strings.HasPrefix(arg, "-m=") {
		return "--memory=" + arg[3:]
	}
	if strings.HasPrefix(arg, "-m") && !strings.HasPrefix(arg, "--") {
		return "--memory=" + arg[2:]
	}
	return arg
}

func parseUpdateOptions(args []string) (run.UpdateOptions, []string, error) {
	var opts run.UpdateOptions
	var targets []string
	args = append([]string(nil), args...)
	for i := 0; i < len(args); i++ {
		arg := memoryOption(args[i])
		args[i] = arg
		if arg == "--" {
			targets = append(targets, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			targets = append(targets, arg)
			continue
		}
		if !strings.HasPrefix(arg, "--") {
			return opts, nil, fmt.Errorf("unknown update option %q", arg)
		}
		opt, err := readOption(args, &i, nil)
		if err != nil {
			return opts, nil, err
		}
		if err := parseUpdateSetting(&opts, opt.Name, *opt.Value); err != nil {
			return opts, nil, err
		}
	}
	if opts.Empty() || len(targets) == 0 {
		return opts, nil, errors.New(updateUsage)
	}
	for _, target := range targets {
		if target == "" {
			return opts, nil, errors.New("container name cannot be empty")
		}
	}
	return opts, targets, nil
}

func updateCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(ctx.Stdout, updateUsage)
		return 0, err
	}
	opts, targets, err := parseUpdateOptions(args)
	if err != nil {
		return 1, err
	}
	var errs []error
	for _, target := range targets {
		if _, err := run.UpdateContainer(ctx, target, opts); err != nil {
			errs = append(errs, fmt.Errorf("update %s: %w", target, err))
			continue
		}
		if _, err := fmt.Fprintln(ctx.Stdout, target); err != nil {
			errs = append(errs, err)
		}
	}
	return 0, errors.Join(errs...)
}

func renameCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(ctx.Stdout, renameUsage)
		return 0, err
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) != 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return 1, errors.New(renameUsage)
	}
	_, err := run.RenameContainer(ctx, args[0], args[1])
	return 0, err
}
