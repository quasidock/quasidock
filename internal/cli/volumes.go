package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/lifecycle"
)

func volumeCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 0 {
		return 1, errors.New("usage: quasidock volume <create|ls|inspect|rm|prune>")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return 1, errors.New("usage: quasidock volume create NAME")
		}
		v, err := ctx.Store.CreateVolume(rest[0])
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(ctx.Stdout, v.Name)
	case "ls", "list":
		quiet := false
		for _, arg := range rest {
			if arg != "-q" && arg != "--quiet" {
				return 1, fmt.Errorf("unknown volume ls option %q", arg)
			}
			quiet = true
		}
		all, err := ctx.Store.ListVolumes()
		if err != nil {
			return 1, err
		}
		tw := tabwriter.NewWriter(ctx.Stdout, 0, 0, 3, ' ', 0)
		if !quiet {
			fmt.Fprintln(tw, "DRIVER\tVOLUME NAME")
		}
		for _, v := range all {
			if quiet {
				fmt.Fprintln(tw, v.Name)
			} else {
				fmt.Fprintf(tw, "%s\t%s\n", v.Driver, v.Name)
			}
		}
		if err := tw.Flush(); err != nil {
			return 1, err
		}
	case "inspect":
		format, names, err := parseInspectFlags(rest, "usage: quasidock volume inspect [-f TEMPLATE] NAME [NAME...]")
		if err != nil {
			return 1, err
		}
		items := []any{}
		for _, name := range names {
			v, err := ctx.Store.ReadVolume(name)
			if err != nil {
				return 1, err
			}
			items = append(items, v)
		}
		if err := renderInspect(ctx.Stdout, format, items); err != nil {
			return 1, err
		}
	case "rm", "remove":
		if len(rest) == 0 {
			return 1, errors.New("usage: quasidock volume rm NAME [NAME...]")
		}
		if err := hkcu.SettleLeftovers(ctx.Store, ""); err != nil {
			return 1, err
		}
		if err := grants.RepairMounts(ctx.Store); err != nil {
			return 1, err
		}
		guard, err := grants.LockMountSources(ctx.Store)
		if err != nil {
			return 1, err
		}
		defer guard.Close()
		var errs []error
		for _, name := range rest {
			if err := ctx.Store.RemoveVolume(name, guard.Protected); err != nil {
				errs = append(errs, err)
			} else {
				fmt.Fprintln(ctx.Stdout, name)
			}
		}
		if err := errors.Join(errs...); err != nil {
			return 1, err
		}
	case "prune":
		dry := false
		for _, arg := range rest {
			switch arg {
			case "--dry-run":
				dry = true
			case "-f", "--force":
			default:
				return 1, fmt.Errorf("unknown volume prune option %q", arg)
			}
		}
		if !dry {
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
		all, size, err := ctx.Store.PruneVolumes(dry, guard.Protected)
		if err != nil {
			return 1, err
		}
		for _, v := range all {
			fmt.Fprintln(ctx.Stdout, v.Name)
		}
		verb := "Reclaimed"
		if dry {
			verb = "Would reclaim"
		}
		fmt.Fprintf(ctx.Stdout, "%s %s in %d volume(s)\n", verb, lifecycle.FormatBytes(size), len(all))
	case "help", "--help":
		fmt.Fprint(ctx.Stdout, usage)
	default:
		return 1, fmt.Errorf("unknown volume command %q", cmd)
	}
	return 0, nil
}
