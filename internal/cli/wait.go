package cli

import (
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/run"
)

const waitUsage = "usage: quasidock wait CONTAINER [CONTAINER...]"

func parseWaitOptions(args []string) ([]string, error) {
	var targets []string
	endOptions := false
	for _, arg := range args {
		if arg == "--" && !endOptions {
			endOptions = true
			continue
		}
		if arg == "" || !endOptions && strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("invalid wait argument %q; %s", arg, waitUsage)
		}
		targets = append(targets, arg)
	}
	if len(targets) == 0 {
		return nil, errors.New(waitUsage)
	}
	return targets, nil
}

func waitContainers(cancel stdcontext.Context, ctx *context.Context, targets []string, wait func(stdcontext.Context, *context.Context, string) (uint32, error)) error {
	cancel, stop := stdcontext.WithCancel(cancel)
	var workers sync.WaitGroup
	defer func() { stop(); workers.Wait() }()
	type result struct {
		code uint32
		err  error
	}
	results := make([]chan result, len(targets))
	for i, target := range targets {
		ch := make(chan result, 1)
		results[i] = ch
		workers.Go(func() {
			code, err := wait(cancel, ctx, target)
			ch <- result{code, err}
		})
	}
	var errs []error
	for i, ch := range results {
		select {
		case <-cancel.Done():
			return cancel.Err()
		case result := <-ch:
			if err := cancel.Err(); err != nil {
				return err
			}
			if result.err != nil {
				errs = append(errs, fmt.Errorf("wait %s: %w", targets[i], result.err))
				continue
			}
			if _, err := fmt.Fprintln(ctx.Stdout, result.code); err != nil {
				return err
			}
		}
	}
	return errors.Join(errs...)
}

func waitCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(ctx.Stdout, waitUsage+"\nPrint each exit code in argument order; waiting succeeds even when a container exits nonzero.\nReturns on the first exit, including before automatic restart. Ctrl+C disconnects without stopping containers.")
		return 0, err
	}
	targets, err := parseWaitOptions(args)
	if err != nil {
		return 1, err
	}
	cancel, stop := signal.NotifyContext(stdcontext.Background(), os.Interrupt)
	defer stop()
	if err := waitContainers(cancel, ctx, targets, run.WaitContainer); err != nil {
		if cancel.Err() != nil {
			return 130, nil
		}
		return 1, err
	}
	return 0, nil
}
