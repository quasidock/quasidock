package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/win32"
)

type networkBackend struct {
	list          func() ([]win32.HCNNetwork, error)
	createPrivate func(string) (string, error)
}

var (
	windowsNetworkBackend = networkBackend{list: win32.ListHCNNetworks, createPrivate: win32.CreatePrivateHCNNetwork}
	networkNamePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

func networkCommand(ctx *context.Context, args []string, backend networkBackend) (uint32, error) {
	const usage = "usage: quasidock network <create NAME|ls [-q|--quiet]>"
	if len(args) == 0 {
		return 1, errors.New(usage)
	}
	switch args[0] {
	case "create":
		if len(args) != 2 {
			return 1, errors.New("usage: quasidock network create NAME")
		}
		name := args[1]
		if !networkNamePattern.MatchString(name) {
			return 1, fmt.Errorf("invalid network name %q: use 1–128 ASCII letters, digits, dots, underscores or hyphens, starting with a letter or digit", name)
		}
		all, err := backend.list()
		if err != nil {
			return 1, networkBackendError(err)
		}
		for _, network := range all {
			if strings.EqualFold(network.Name, name) {
				return 1, fmt.Errorf("network %q already exists", network.Name)
			}
		}
		id, err := backend.createPrivate(name)
		if errors.Is(err, win32.ErrHCNNetworkExists) {
			return 1, fmt.Errorf("network %q already exists: %w", name, err)
		}
		if err != nil {
			return 1, networkBackendError(err)
		}
		if _, err := fmt.Fprintln(ctx.Stdout, id); err != nil {
			return 1, fmt.Errorf("network %q was created with ID %s: %w", name, id, err)
		}
	case "ls", "list":
		quiet := false
		for _, arg := range args[1:] {
			if arg != "-q" && arg != "--quiet" {
				return 1, fmt.Errorf("unknown network ls option %q", arg)
			}
			quiet = true
		}
		if err := listNetworks(ctx, backend, quiet); err != nil {
			return 1, err
		}
	default:
		return 1, fmt.Errorf("unknown network command %q\n%s", args[0], usage)
	}
	return 0, nil
}

func networkBackendError(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("run this command from an elevated PowerShell session: %w", err)
	}
	if errors.Is(err, win32.ErrHCNUnavailable) {
		return fmt.Errorf("this command requires the Windows Host Network Service (HNS) and Host Compute Network (HCN) API: %w", err)
	}
	return err
}

func listNetworks(ctx *context.Context, backend networkBackend, quiet bool) error {
	all, err := backend.list()
	if err != nil {
		return networkBackendError(err)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := strings.ToLower(all[i].Name), strings.ToLower(all[j].Name)
		if a == b {
			return all[i].ID < all[j].ID
		}
		return a < b
	})
	if quiet {
		for _, network := range all {
			if _, err := fmt.Fprintln(ctx.Stdout, network.ID); err != nil {
				return err
			}
		}
		return nil
	}
	table := tabwriter.NewWriter(ctx.Stdout, 0, 0, 3, ' ', 0)
	if _, err := fmt.Fprintln(table, "NETWORK ID\tNAME\tDRIVER\tSCOPE"); err != nil {
		return err
	}
	for _, network := range all {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\tlocal\n", network.ID, network.Name, network.Driver); err != nil {
			return err
		}
	}
	return table.Flush()
}
