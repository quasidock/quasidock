package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/store"
)

const volumePruneUsage = "usage: quasidock volume prune [-a|--all] [-f|--force] [--dry-run] [--filter FILTER]..."

const volumePruneHelp = volumePruneUsage + `
Remove unused anonymous volumes. Use -a or --all to include unused named volumes.
Saved container references and live mount grants protect volumes.
Use --filter label=KEY[=VALUE] to require a label and --filter label!=KEY[=VALUE]
to protect volumes with a label. Every inclusion must match, and any matching
exclusion protects a volume. Label keys and values are case-sensitive.
Use --dry-run to preview candidates and reclaimable bytes. This command runs
without confirmation. The -f and --force flags are accepted for compatibility.`

type volumePruneOptions struct {
	selection store.VolumePruneOptions
	dry       bool
}

func parseVolumePruneOptions(args []string) (volumePruneOptions, error) {
	var opts volumePruneOptions
	var force bool
	flags := flag.NewFlagSet("volume prune", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.selection.All, "all", false, "")
	flags.BoolVar(&opts.selection.All, "a", false, "")
	flags.BoolVar(&force, "force", false, "")
	flags.BoolVar(&force, "f", false, "")
	flags.BoolVar(&opts.dry, "dry-run", false, "")
	flags.Func("filter", "", func(raw string) error {
		key, value, ok := strings.Cut(raw, "=")
		if !ok || value == "" {
			return fmt.Errorf("invalid filter %q: expected label=KEY[=VALUE] or label!=KEY[=VALUE]", raw)
		}
		if key != "label" && key != "label!" {
			return fmt.Errorf("unsupported filter %q: use label or label!", key)
		}
		if !validMetadataKeyValue(value) {
			return fmt.Errorf("invalid label filter %q: label key must be nonempty and contain no whitespace, and NUL characters are invalid", raw)
		}
		name, label, hasValue := strings.Cut(value, "=")
		filter := store.LabelFilter{Key: name, Value: label, HasValue: hasValue}
		if key == "label" {
			opts.selection.Labels = append(opts.selection.Labels, filter)
		} else {
			opts.selection.ExcludeLabels = append(opts.selection.ExcludeLabels, filter)
		}
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return opts, nil
}

func volumePrune(ctx *context.Context, args []string) (uint32, error) {
	opts, err := parseVolumePruneOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		_, err := fmt.Fprintln(ctx.Stdout, volumePruneHelp)
		return 0, err
	}
	if err != nil {
		return 1, fmt.Errorf("volume prune: %w\n%s", err, volumePruneUsage)
	}
	if !opts.dry {
		if err := hkcu.SettleLeftovers(ctx.Store, ""); err != nil {
			return 1, err
		}
		if err := grants.RepairMounts(ctx.Store); err != nil {
			return 1, err
		}
	}
	guard, err := grants.LockMountSources(ctx.Store)
	if err != nil {
		return 1, err
	}
	defer guard.Close()
	volumes, size, err := ctx.Store.PruneVolumes(opts.dry, opts.selection, guard.Protected)
	if err != nil {
		return 1, err
	}
	for _, v := range volumes {
		fmt.Fprintln(ctx.Stdout, v.Name)
	}
	verb := "Reclaimed"
	if opts.dry {
		verb = "Would reclaim"
	}
	fmt.Fprintf(ctx.Stdout, "%s %s in %d volume(s)\n", verb, lifecycle.FormatBytes(size), len(volumes))
	return 0, nil
}
