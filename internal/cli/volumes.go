package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
)

const volumeCreateUsage = "usage: quasidock volume create [--label KEY[=VALUE]]... NAME"

func volumeCreate(ctx *context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(ctx.Stdout, volumeCreateUsage+"\n--label sets metadata on first creation and can be repeated.")
		return err
	}
	labels := make(map[string]string)
	for len(args) > 0 {
		option, value, assigned := strings.Cut(args[0], "=")
		if option != "--label" {
			break
		}
		args = args[1:]
		if !assigned && len(args) > 0 {
			value, args = args[0], args[1:]
		}
		if !validMetadataKeyValue(value) {
			return errors.New("volume create --label expects KEY[=VALUE] with a nonempty key and no whitespace in the key or NUL characters")
		}
		key, label, _ := strings.Cut(value, "=")
		labels[key] = label
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(volumeCreateUsage)
	}
	v, err := ctx.Store.CreateVolumeWithLabels(args[0], labels)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(ctx.Stdout, v.Name)
	return err
}

func volumeCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 0 {
		return 1, errors.New("usage: quasidock volume <create|ls|inspect|rm|prune>")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		if err := volumeCreate(ctx, rest); err != nil {
			return 1, err
		}
	case "ls", "list":
		if err := volumeList(ctx, rest); err != nil {
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
		return volumePrune(ctx, rest)
	case "help", "--help":
		fmt.Fprint(ctx.Stdout, usage)
	default:
		return 1, fmt.Errorf("unknown volume command %q", cmd)
	}
	return 0, nil
}
