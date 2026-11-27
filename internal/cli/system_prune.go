package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/store"
)

const systemPruneUsage = "usage: quasidock system prune [-a|--all] [-f|--force] [--dry-run] [--volumes] [--networks] [--filter until=TIME]..."

const systemPruneHelp = systemPruneUsage + `
Remove stopped containers, unused unnamed images with no app default, and unused build cache.
Build cache includes build results, downloaded sources, tool caches, incremental work, and build-only images.
  -a, --all          also remove unused named images and app defaults
  -f, --force        skip confirmation
      --dry-run      preview removals and estimated reclaimable bytes
      --volumes      remove unused named and anonymous volumes
      --networks     remove unused tracked private networks (requires elevation)
      --filter until=TIME
                     remove items strictly older than any supplied cutoff
Age uses container and volume creation, image cache registration, network ownership marker
modification, and build-cache entry modification (newest modification in a directory tree).
Items with unknown ages are kept when filtering.`

type systemPruneOptions struct {
	dry, all, force, volumes, networks bool
	until                              []time.Time
}

func parseSystemPruneOptions(args []string, now time.Time) (systemPruneOptions, error) {
	var opts systemPruneOptions
	flags := flag.NewFlagSet("system prune", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.all, "all", false, "")
	flags.BoolVar(&opts.all, "a", false, "")
	flags.BoolVar(&opts.force, "force", false, "")
	flags.BoolVar(&opts.force, "f", false, "")
	flags.BoolVar(&opts.dry, "dry-run", false, "")
	flags.BoolVar(&opts.volumes, "volumes", false, "")
	flags.BoolVar(&opts.networks, "networks", false, "")
	flags.Func("filter", "", func(raw string) error {
		key, value, ok := strings.Cut(raw, "=")
		if !ok || value == "" {
			return fmt.Errorf("invalid filter %q: expected until=TIME", raw)
		}
		if key != "until" {
			return fmt.Errorf("unsupported filter %q: use until=TIME", key)
		}
		cutoff, err := parseCommandTime(value, now)
		if err != nil {
			return err
		}
		opts.until = append(opts.until, cutoff)
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

func systemPrune(ctx *context.Context, args []string, input io.Reader, backend networkBackend) (uint32, error) {
	opts, err := parseSystemPruneOptions(args, ctx.Now())
	if errors.Is(err, flag.ErrHelp) {
		return writeSystemHelp(ctx.Stdout, systemPruneHelp)
	}
	if err != nil {
		return 1, fmt.Errorf("system prune: %w\n%s", err, systemPruneUsage)
	}
	if !opts.dry && !opts.force {
		out := ctx.Stderr
		if out == nil {
			out = os.Stderr
		}
		confirmed, err := confirmSystemPrune(out, input, opts)
		if err != nil || !confirmed {
			if err != nil {
				return 1, err
			}
			return 0, nil
		}
	}
	if opts.networks {
		elevated, err := backend.elevated()
		if err != nil {
			return 1, fmt.Errorf("check network prune elevation: %w", err)
		}
		if !elevated {
			return 1, errors.New("run system prune --networks from an elevated PowerShell session")
		}
		if _, err := backend.list(); err != nil {
			return 1, networkBackendError(err)
		}
	}
	if err := openCommandStore(ctx); err != nil {
		return 1, err
	}
	lock, err := ctx.Store.Lock(true)
	if err != nil {
		return 1, err
	}
	defer lock.Release()
	var report strings.Builder
	freed, err := pruneSystemResources(ctx, opts, backend, &report)
	total := "Total reclaimed space:"
	if opts.dry {
		total = "Would reclaim:"
	}
	fmt.Fprintln(&report, total, lifecycle.FormatBytes(freed))
	_, writeErr := io.WriteString(ctx.Stdout, report.String())
	if err := errors.Join(err, writeErr); err != nil {
		return 1, err
	}
	return 0, nil
}

func confirmSystemPrune(out io.Writer, input io.Reader, opts systemPruneOptions) (bool, error) {
	images := "unused unnamed images with no app default"
	if opts.all {
		images = "all unused images, including named images and app defaults"
	}
	items := []string{"stopped containers", images, "unused build cache, including downloaded sources, tool caches, and incremental work"}
	if opts.volumes {
		items = append(items, "unused named volumes and anonymous volumes")
	}
	if opts.networks {
		items = append(items, "unused tracked private networks")
	}
	if _, err := fmt.Fprintf(out, "This will remove eligible resources:\n  %s\nContinue? [y/N] ", strings.Join(items, "\n  ")); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	response := strings.ToLower(strings.TrimSpace(line))
	return response == "y" || response == "yes", nil
}

func pruneSystemResources(ctx *context.Context, opts systemPruneOptions, backend networkBackend, report *strings.Builder) (uint64, error) {
	st := ctx.Store
	containers, freed, err := st.PruneSystemContainers(opts.dry, opts.until)
	var ids []string
	var excluded map[string]bool
	if opts.dry {
		excluded = make(map[string]bool, len(containers))
	}
	for _, c := range containers {
		ids = append(ids, c.ID)
		if opts.dry {
			excluded[c.ID] = true
		}
	}
	reportSystemPruned(report, "Containers", ids, opts.dry)
	if err != nil {
		return freed, err
	}
	if !opts.dry && (len(containers) > 0 || opts.volumes) {
		if err := errors.Join(hkcu.SettleLeftovers(st, ""), grants.RepairMounts(st)); err != nil {
			return freed, err
		}
	}
	imagesAndCache, err := lifecycle.PruneImagesAndBuildCache(ctx, store.ImagePruneOptions{
		All: opts.all, Until: opts.until, ExcludedContainers: excluded,
	}, opts.dry)
	lifecycle.ReportPrunedImages(report, imagesAndCache.Images, opts.dry)
	cache := make([]string, 0, len(imagesAndCache.Cache))
	for _, entry := range imagesAndCache.Cache {
		cache = append(cache, entry.Area+"/"+entry.Name)
	}
	reportSystemPruned(report, "Build Cache", cache, opts.dry)
	freed += imagesAndCache.Bytes
	if err != nil {
		return freed, err
	}
	if opts.volumes {
		guard, err := grants.LockMountSources(st)
		if err != nil {
			return freed, err
		}
		volumes, bytes, err := st.PruneSystemVolumes(opts.dry, opts.until, excluded, guard.Protected)
		guard.Close()
		names := make([]string, 0, len(volumes))
		for _, v := range volumes {
			names = append(names, v.Name)
		}
		reportSystemPruned(report, "Volumes", names, opts.dry)
		freed += bytes
		if err != nil {
			return freed, err
		}
	}
	if opts.networks {
		ids, err := cleanupNetworks(st, backend, nil, opts.dry, opts.until, excluded)
		reportSystemPruned(report, "Networks", ids, opts.dry)
		return freed, err
	}
	return freed, nil
}

func reportSystemPruned(out *strings.Builder, category string, names []string, dry bool) {
	if len(names) == 0 {
		return
	}
	if dry {
		fmt.Fprintf(out, "Would remove %s:\n", strings.ToLower(category))
	} else {
		fmt.Fprintf(out, "Deleted %s:\n", category)
	}
	for _, name := range names {
		fmt.Fprintln(out, escapeTableText(name))
	}
	fmt.Fprintln(out)
}
