package cli

import (
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/quasidock/quasidock/internal/commit"
	"github.com/quasidock/quasidock/internal/context"
)

func parseCommitOptions(args []string) (commit.Options, error) {
	opts := commit.Options{Pause: true}
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
		if !strings.HasPrefix(arg, "--") && len(arg) >= 2 {
			name := ""
			switch arg[1] {
			case 'p':
				name = "pause"
			case 'a':
				name = "author"
			case 'm':
				name = "message"
			case 'c':
				name = "change"
			}
			if name != "" {
				args[i] = "--" + name
				if len(arg) > 2 {
					args[i] += "=" + strings.TrimPrefix(arg[2:], "=")
				}
			} else {
				return opts, fmt.Errorf("unknown commit option %q", arg)
			}
		}
		if len(args[i]) < 3 {
			return opts, fmt.Errorf("unknown commit option %q", arg)
		}
		option, err := readOption(args, &i, []string{"pause"})
		if err != nil {
			return opts, err
		}
		switch option.Name {
		case "pause":
			if option.Value == nil {
				opts.Pause = true
			} else {
				value, err := strconv.ParseBool(*option.Value)
				if err != nil {
					return opts, errors.New("--pause expects true or false")
				}
				opts.Pause = value
			}
		case "author", "message", "change":
			if option.Value == nil {
				return opts, fmt.Errorf("--%s needs a value", option.Name)
			}
			switch option.Name {
			case "author":
				opts.Author = *option.Value
			case "message":
				opts.Message = *option.Value
			case "change":
				opts.Changes = append(opts.Changes, *option.Value)
			}
		default:
			return opts, fmt.Errorf("unknown commit option --%s", option.Name)
		}
		i++
	}
	if len(args)-i < 1 || len(args)-i > 2 || args[i] == "" {
		return opts, errors.New("usage: quasidock commit [--pause=false] [-a AUTHOR] [-m MESSAGE] [-c INSTRUCTION] CONTAINER [REPOSITORY[:TAG]]")
	}
	opts.Container = args[i]
	if len(args)-i == 2 {
		opts.Target = args[i+1]
		if opts.Target == "" || strings.HasPrefix(opts.Target, "-") {
			return opts, errors.New("commit target must be a repository and optional tag; options precede CONTAINER")
		}
	}
	return opts, nil
}

func commitCommand(ctx *context.Context, args []string) (uint32, error) {
	opts, err := parseCommitOptions(args)
	if err != nil {
		return 1, err
	}
	cancel, stop := signal.NotifyContext(stdcontext.Background(), os.Interrupt)
	defer stop()
	digest, err := commit.Commit(cancel, ctx, opts)
	if err != nil {
		if cancel.Err() != nil {
			return 130, nil
		}
		return 1, err
	}
	_, err = fmt.Fprintln(ctx.Stdout, digest)
	return 0, err
}
