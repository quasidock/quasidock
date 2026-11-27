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
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

type networkBackend struct {
	list          func() ([]win32.HCNNetwork, error)
	elevated      func() (bool, error)
	createPrivate func(string, func(string) error) (string, error)
	inspect       func(string) (win32.HCNNetworkDetails, error)
	privateSubnet func(string) error
	lockMutation  func() (func(), error)
	hasEndpoints  func(string) (bool, error)
	deleteNetwork func(string) error
}

var (
	windowsNetworkBackend = networkBackend{
		list:          win32.ListHCNNetworks,
		elevated:      win32.ProcessElevated,
		createPrivate: win32.CreatePrivateHCNNetwork,
		inspect:       win32.InspectHCNNetwork,
		privateSubnet: func(id string) error { _, err := win32.PrivateHCNSubnet(id); return err },
		lockMutation:  func() (func(), error) { return win32.LockHCNMutation(30_000) },
		hasEndpoints:  win32.HCNNetworkHasEndpoints,
		deleteNetwork: win32.DeleteHCNNetwork,
	}
	networkNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

func networkCommand(ctx *context.Context, args []string, backend networkBackend) (uint32, error) {
	const usage = "usage: quasidock network <create NAME|ls [-q|--quiet]|inspect [-f TEMPLATE] NETWORK [NETWORK...]|connect NETWORK CONTAINER|disconnect NETWORK CONTAINER|rm NETWORK [NETWORK...]|prune [--dry-run]>"
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
		if err := openNetworkStore(ctx); err != nil {
			return 1, err
		}
		var trackErr error
		id, err := backend.createPrivate(name, func(id string) error {
			trackErr = ctx.Store.TrackNetwork(id)
			return trackErr
		})
		if trackErr != nil {
			return 1, fmt.Errorf("record ownership for network %q: %w", name, trackErr)
		}
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
	case "inspect":
		if err := inspectNetworks(ctx, backend, args[1:]); err != nil {
			return 1, err
		}
	case "rm", "prune":
		if err := networkCleanupCommand(ctx, backend, args[0], args[1:]); err != nil {
			return 1, err
		}
	case "connect", "disconnect":
		if len(args) != 3 || args[1] == "" || args[2] == "" || strings.HasPrefix(args[1], "-") || strings.HasPrefix(args[2], "-") {
			return 1, fmt.Errorf("usage: quasidock network %s NETWORK CONTAINER", args[0])
		}
		if err := openNetworkStore(ctx); err != nil {
			return 1, err
		}
		if err := run.ChangeContainerNetwork(ctx, args[2], args[1], args[0] == "connect", backend.list, backend.privateSubnet); err != nil {
			return 1, networkBackendError(err)
		}
	default:
		return 1, fmt.Errorf("unknown network command %q\n%s", args[0], usage)
	}
	return 0, nil
}

func openNetworkStore(ctx *context.Context) error {
	if ctx.Store != nil {
		return nil
	}
	st, err := store.Open("", ctx.Verbose, "")
	if err != nil {
		return err
	}
	st.EventWarnings = ctx.Stderr
	ctx.Store = st
	return nil
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

type inspectedNetwork struct {
	Id         string
	Name       string
	Driver     string
	Scope      string
	Properties map[string]any
}

func inspectNetworks(ctx *context.Context, backend networkBackend, args []string) error {
	format, targets, err := parseInspectFlags(args, "usage: quasidock network inspect [-f TEMPLATE|--format TEMPLATE] NAME|ID [NAME|ID...]")
	if err != nil {
		return err
	}
	all, err := backend.list()
	if err != nil {
		return networkBackendError(err)
	}
	ids := make([]string, len(targets))
	for i, target := range targets {
		network, err := win32.ResolveHCNNetwork(target, all)
		if err != nil {
			return err
		}
		ids[i] = network.ID
	}

	items := make([]any, 0, len(ids))
	inspected := make(map[string]inspectedNetwork)
	for i, id := range ids {
		item, ok := inspected[id]
		if !ok {
			details, err := backend.inspect(id)
			if errors.Is(err, win32.ErrHCNNetworkNotFound) {
				return fmt.Errorf("network %q not found: %w", targets[i], err)
			}
			if err != nil {
				return fmt.Errorf("inspect Windows network %q: %w", targets[i], networkBackendError(err))
			}
			item = inspectedNetwork{
				Id:         details.ID,
				Name:       details.Name,
				Driver:     details.Driver,
				Scope:      "local",
				Properties: details.Properties,
			}
			inspected[id] = item
		}
		items = append(items, item)
	}
	return renderInspect(ctx.Stdout, format, items)
}
