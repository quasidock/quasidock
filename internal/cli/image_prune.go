package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/store"
)

const imagePruneUsage = "usage: quasidock image prune [-a|--all] [-f|--force] [--dry-run] [--filter FILTER...]"

type imagePruneOptions struct {
	selection store.ImagePruneOptions
	dry, help bool
}

func parseImagePruneOptions(args []string, now time.Time) (imagePruneOptions, error) {
	var opts imagePruneOptions
	setBool := func(name, raw string) error {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid image prune --%s boolean %q", name, raw)
		}
		switch name {
		case "all", "a":
			opts.selection.All = value
		case "dry-run":
			opts.dry = value
		case "help", "h":
			opts.help = value
		}
		return nil
	}
	addFilter := func(raw string) error {
		key, value, ok := strings.Cut(raw, "=")
		if !ok || value == "" {
			return fmt.Errorf("invalid image prune filter %q: expected KEY=VALUE", raw)
		}
		switch key {
		case "until":
			cutoff, err := parseCommandTime(value, now)
			if err != nil {
				return fmt.Errorf("image prune until: %w", err)
			}
			opts.selection.Until = append(opts.selection.Until, cutoff)
		case "label", "label!":
			name, val, hasValue := strings.Cut(value, "=")
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("invalid image prune label filter %q: label key is empty", raw)
			}
			filter := store.ImageLabelFilter{Key: name, Value: val, HasValue: hasValue}
			if key == "label" {
				opts.selection.Labels = append(opts.selection.Labels, filter)
			} else {
				opts.selection.NotLabels = append(opts.selection.NotLabels, filter)
			}
		default:
			return fmt.Errorf("unsupported image prune filter %q: use until, label, or label!", key)
		}
		return nil
	}
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		if arg == "--" && len(args) == 0 {
			break
		}
		if strings.HasPrefix(arg, "--") {
			name, raw, hasValue := strings.Cut(arg[2:], "=")
			switch name {
			case "filter":
				if !hasValue {
					if len(args) == 0 {
						return opts, errors.New("image prune --filter needs a value")
					}
					raw, args = args[0], args[1:]
				}
				if err := addFilter(raw); err != nil {
					return opts, err
				}
			case "all", "force", "dry-run", "help":
				if !hasValue {
					raw = "true"
				}
				if err := setBool(name, raw); err != nil {
					return opts, err
				}
			default:
				return opts, fmt.Errorf("unknown image prune option %q", arg)
			}
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return opts, errors.New(imagePruneUsage)
		}
		for short := arg[1:]; short != ""; {
			name, raw := short[:1], "true"
			short = short[1:]
			if name != "a" && name != "f" && name != "h" {
				return opts, fmt.Errorf("unknown image prune option -%s", name)
			}
			if strings.HasPrefix(short, "=") {
				raw, short = short[1:], ""
			}
			if err := setBool(name, raw); err != nil {
				return opts, err
			}
		}
	}
	return opts, nil
}

func imagePrune(ctx *context.Context, args []string) (uint32, error) {
	opts, err := parseImagePruneOptions(args, ctx.Now())
	if err != nil {
		return 1, err
	}
	if opts.help {
		_, err := fmt.Fprintln(ctx.Stdout, imagePruneUsage+"\nRemove unnamed, nondefault images; --all also removes unused names and defaults.\nContainer records and active leases protect images. Build-cache content is retained.\nFilters: until=TIME, label=KEY[=VALUE], label!=KEY[=VALUE].\nuntil uses first local cache registration, not image creation time; unknown cache ages are skipped.\nRepeated filters within a key use OR; different keys use AND.\n--dry-run previews without changing images or containers. Commands never prompt; --force is a compatibility flag.")
		return 0, err
	}
	lock, err := ctx.Store.Lock(true)
	if err != nil {
		return 1, err
	}
	defer lock.Release()
	if err := lifecycle.PruneImages(ctx, opts.selection, opts.dry); err != nil {
		return 1, err
	}
	return 0, nil
}
