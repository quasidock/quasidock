package cli

import (
	stdcontext "context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/imageimport"
)

const importUsage = `usage: quasidock image import --config FILE [-c INSTRUCTION...] [--force] FILE|- [REPOSITORY[:TAG]]

Import a Quasidock export tar (or gzip) containing app/, data/, and runtimes/.
--config requires app-config JSON, as emitted by image inspect's .Quasidock field.
-c/--change supports ENTRYPOINT, CMD, ENV, WORKDIR, and LABEL, in order.
Options precede input. - reads stdin; prefer a file in PowerShell for binary data.
An optional target adds a local alias; config id/version stay unchanged.
Conflicting aliases require --force. Existing app defaults remain selected.
Import works offline and prints the full manifest digest without launching an app.`

type importOptions struct {
	imageimport.Options
	input, config string
}

func parseImportOptions(args []string) (importOptions, error) {
	var opts importOptions
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.config, "config", "", "app config JSON file")
	flags.BoolVar(&opts.Force, "force", false, "replace a conflicting alias")
	change := func(value string) error {
		opts.Changes = append(opts.Changes, value)
		return nil
	}
	flags.Func("c", "configuration instruction", change)
	flags.Func("change", "configuration instruction", change)
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if opts.config == "" || opts.config == "-" || flags.NArg() < 1 || flags.NArg() > 2 {
		return opts, errors.New(importUsage)
	}
	opts.input = flags.Arg(0)
	if opts.input == "" || opts.input != "-" && strings.HasPrefix(opts.input, "-") {
		return opts, errors.New(importUsage)
	}
	if flags.NArg() == 2 {
		opts.Target = flags.Arg(1)
		if opts.Target == "" || strings.HasPrefix(opts.Target, "-") {
			return opts, errors.New(importUsage + "; options precede input")
		}
	}
	return opts, nil
}

func importCommand(ctx *context.Context, args []string) (uint32, error) {
	opts, err := parseImportOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		_, err = fmt.Fprintln(ctx.Stdout, importUsage)
		return 0, err
	}
	if err != nil {
		return 1, err
	}
	config, err := os.Open(opts.config)
	if err != nil {
		return 1, err
	}
	defer func() { _ = config.Close() }()
	var src io.Reader = os.Stdin
	if opts.input != "-" {
		f, err := os.Open(opts.input)
		if err != nil {
			return 1, err
		}
		defer func() { _ = f.Close() }()
		src = f
		opts.Source, err = filepath.Abs(opts.input)
		if err != nil {
			return 1, err
		}
	}
	signals, stop := signal.NotifyContext(stdcontext.Background(), os.Interrupt)
	defer stop()
	cancel, interrupt := stdcontext.WithCancel(signals)
	defer interrupt()
	streams := &copyStreams{ctx: cancel, interrupt: interrupt, in: src, out: ctx.Stdout}
	if opts.input == "-" {
		restore, err := streams.prepareConsoleInput()
		if err != nil {
			return 1, err
		}
		defer restore()
	}
	var digest string
	code, err := runTransfer(cancel, streams, func() error {
		var err error
		digest, err = imageimport.Import(cancel, ctx, streams, config, opts.Options)
		return err
	})
	if code != 0 || err != nil {
		return code, err
	}
	_, err = fmt.Fprintln(ctx.Stdout, digest)
	return 0, err
}
