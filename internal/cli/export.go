package cli

import (
	stdcontext "context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/quasidock/quasidock/internal/containerexport"
	"github.com/quasidock/quasidock/internal/context"
)

const exportUsage = `usage: quasidock export [-o FILE|--output FILE] [-p|--pause[=true|false]] CONTAINER

Export app/, data/, and runtimes/runtime-<alias>/ as an uncompressed filesystem tar.
Options precede CONTAINER. Output defaults to stdout; -o - also selects stdout.
Mounted storage is excluded. Running containers pause only during data capture
(default true); --pause=false captures best effort. All app/runtime blobs must
be local. Archives have canonical permissions and zero timestamps; they contain
no launch configuration and cannot be restored with load.`

type exportOptions struct {
	containerexport.Options
	output string
}

func parseExportOptions(args []string) (exportOptions, error) {
	var opts exportOptions
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.output, "o", "-", "output file")
	flags.StringVar(&opts.output, "output", "-", "output file")
	flags.BoolVar(&opts.Pause, "p", true, "pause during data capture")
	flags.BoolVar(&opts.Pause, "pause", true, "pause during data capture")
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if opts.output == "" || flags.NArg() != 1 || flags.Arg(0) == "" || strings.HasPrefix(flags.Arg(0), "-") {
		return opts, errors.New(exportUsage)
	}
	opts.Container = flags.Arg(0)
	return opts, nil
}

func exportCommand(ctx *context.Context, args []string) (uint32, error) {
	opts, err := parseExportOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		_, err = fmt.Fprintln(ctx.Stdout, exportUsage)
		return 0, err
	}
	if err != nil {
		return 1, err
	}
	cancel, stop := signal.NotifyContext(stdcontext.Background(), os.Interrupt)
	defer stop()
	streams := &copyStreams{ctx: cancel, out: ctx.Stdout}
	return runTransfer(cancel, streams, func() error {
		if opts.output != "-" {
			return containerexport.ExportFile(cancel, ctx, opts.Options, opts.output)
		}
		return containerexport.Export(cancel, ctx, opts.Options, streams)
	})
}
