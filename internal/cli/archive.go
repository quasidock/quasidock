package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/imagearchive"
)

const (
	saveUsage = "usage: quasidock save [-o FILE|--output FILE] IMAGE [IMAGE...]"
	loadUsage = "usage: quasidock load [-i FILE|--input FILE] [-q|--quiet] [--force]"
)

type saveOptions struct {
	output string
	images []string
}

type loadOptions struct {
	input string
	quiet bool
	force bool
}

func parseSaveOptions(args []string) (saveOptions, error) {
	var opts saveOptions
	flags := flag.NewFlagSet("save", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.output, "o", "-", "output file")
	flags.StringVar(&opts.output, "output", "-", "output file")
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	opts.images = flags.Args()
	if opts.output == "" || len(opts.images) == 0 {
		return opts, errors.New(saveUsage)
	}
	for _, name := range opts.images {
		if name == "" || strings.HasPrefix(name, "-") {
			return opts, errors.New(saveUsage + "; options precede IMAGE")
		}
	}
	return opts, nil
}

func parseLoadOptions(args []string) (loadOptions, error) {
	var opts loadOptions
	flags := flag.NewFlagSet("load", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.input, "i", "-", "input file")
	flags.StringVar(&opts.input, "input", "-", "input file")
	flags.BoolVar(&opts.quiet, "q", false, "suppress loaded image output")
	flags.BoolVar(&opts.quiet, "quiet", false, "suppress loaded image output")
	flags.BoolVar(&opts.force, "force", false, "replace conflicting names")
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if opts.input == "" || flags.NArg() != 0 {
		return opts, errors.New(loadUsage)
	}
	return opts, nil
}

func imageSave(ctx *context.Context, args []string) error {
	opts, err := parseSaveOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		_, err = fmt.Fprintln(ctx.Stdout, saveUsage)
		return err
	}
	if err != nil {
		return err
	}
	if opts.output == "-" {
		return imagearchive.Save(ctx.Store, ctx.Env["QUASIDOCK_REGISTRY"], opts.images, ctx.Stdout)
	}
	// A sibling temporary file permits replacing the output only after every
	// blob and the complete tar stream have been successfully written.
	f, err := os.CreateTemp(filepath.Dir(opts.output), ".quasidock-save-*.tar")
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if err := imagearchive.Save(ctx.Store, ctx.Env["QUASIDOCK_REGISTRY"], opts.images, f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), opts.output)
}

func imageLoad(ctx *context.Context, args []string) error {
	opts, err := parseLoadOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		_, err = fmt.Fprintln(ctx.Stdout, loadUsage)
		return err
	}
	if err != nil {
		return err
	}
	var src io.Reader = os.Stdin
	if opts.input != "-" {
		f, err := os.Open(opts.input)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		src = f
	}
	images, err := imagearchive.Load(ctx.Store, src, imagearchive.LoadOptions{Force: opts.force})
	if err != nil {
		return err
	}
	if opts.quiet {
		return nil
	}
	var lines []string
	for _, image := range images {
		if len(image.Names) == 0 {
			lines = append(lines, "Loaded image ID: "+image.Digest)
		} else {
			for _, name := range image.Names {
				lines = append(lines, "Loaded image: "+name)
			}
		}
	}
	sort.Strings(lines)
	for _, line := range lines {
		if _, err := fmt.Fprintln(ctx.Stdout, line); err != nil {
			return err
		}
	}
	return nil
}
