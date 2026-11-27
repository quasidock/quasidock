package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func networkCleanupCommand(ctx *context.Context, backend networkBackend, command string, args []string) error {
	var targets []string
	dry := false
	if command == "rm" {
		literal := false
		for _, arg := range args {
			if !literal && arg == "--" {
				literal = true
				continue
			}
			if !literal && strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown network rm option %q", arg)
			}
			if arg == "" {
				return errors.New("network rm requires a nonempty name or GUID")
			}
			targets = append(targets, arg)
		}
		if len(targets) == 0 {
			return errors.New("usage: quasidock network rm [--] NAME|GUID [NAME|GUID...]")
		}
	} else {
		for _, arg := range args {
			if arg != "--dry-run" {
				return fmt.Errorf("unexpected network prune argument %q (usage: quasidock network prune [--dry-run])", arg)
			}
			dry = true
		}
	}
	if err := openNetworkStore(ctx); err != nil {
		return err
	}
	ids, err := cleanupNetworks(ctx.Store, backend, targets, dry, nil, nil)
	for _, id := range ids {
		if _, writeErr := fmt.Fprintln(ctx.Stdout, id); writeErr != nil {
			return errors.Join(err, writeErr)
		}
	}
	return err
}

func cleanupNetworks(st *store.Store, backend networkBackend, targets []string, dry bool, until []time.Time, excluded map[string]bool) ([]string, error) {
	release, err := backend.lockMutation()
	if err != nil {
		return nil, networkBackendError(err)
	}
	defer release()
	all, err := backend.list()
	if err != nil {
		return nil, networkBackendError(err)
	}
	prune := len(targets) == 0
	var candidates []win32.HCNNetwork
	var stale []string
	var errs []error
	if prune {
		candidates, stale, err = trackedNetworkCandidates(st, all, until)
		if err != nil {
			return nil, err
		}
	} else {
		seen := make(map[string]bool)
		for _, target := range targets {
			network, err := win32.ResolveHCNNetwork(target, all)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if !seen[network.ID] {
				seen[network.ID] = true
				candidates = append(candidates, network)
			}
		}
	}
	var removed []string
	if len(candidates) > 0 {
		err := st.WithContainerNetworkUsers(excluded, func(users map[string]*store.ContainerRecord) {
			for _, network := range candidates {
				id := network.ID
				selected, err := removeUnusedNetwork(backend, network, users[id], prune, dry)
				if selected {
					removed = append(removed, id)
				}
				gone := errors.Is(err, win32.ErrHCNNetworkNotFound)
				if err != nil && (!prune || !gone) {
					errs = append(errs, fmt.Errorf("network %q (%s): %w", network.Name, id, err))
				}
				if selected || gone {
					stale = append(stale, id)
				}
			}
		})
		if err != nil {
			return nil, errors.Join(append(errs, fmt.Errorf("read saved network memberships: %w", err))...)
		}
	}
	if !dry {
		for _, id := range stale {
			if err := st.ForgetNetwork(id); err != nil {
				errs = append(errs, fmt.Errorf("forget ownership of network %s: %w", id, err))
			}
		}
	}
	return removed, errors.Join(errs...)
}

func trackedNetworkCandidates(st *store.Store, all []win32.HCNNetwork, until []time.Time) (candidates []win32.HCNNetwork, stale []string, err error) {
	ids, err := st.TrackedNetworkIDs()
	if err != nil {
		return nil, nil, fmt.Errorf("read tracked networks: %w", err)
	}
	live := make(map[string]win32.HCNNetwork, len(all))
	for _, network := range all {
		live[network.ID] = network
	}
	slices.Sort(ids)
	for _, id := range ids {
		if len(until) > 0 {
			info, err := os.Lstat(st.Path("networks", id))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("read age of tracked network %s: %w", id, err)
			}
			if !info.Mode().IsRegular() || !store.PruneBefore(info.ModTime(), until) {
				continue
			}
		}
		network, exists := live[id]
		if !exists {
			stale = append(stale, id)
		} else if network.Driver == "private" {
			candidates = append(candidates, network)
		}
	}
	return candidates, stale, nil
}

func removeUnusedNetwork(backend networkBackend, network win32.HCNNetwork, user *store.ContainerRecord, prune, dry bool) (bool, error) {
	if network.Driver != "private" {
		return false, fmt.Errorf("driver %s is protected; only private networks can be removed", network.Driver)
	}
	if user != nil {
		if prune {
			return false, nil
		}
		return false, fmt.Errorf("referenced by container %s (%s); disconnect the container first", user.ShortID(), user.BareName())
	}
	inUse, err := backend.hasEndpoints(network.ID)
	if err != nil {
		return false, fmt.Errorf("check Windows endpoints: %w", networkBackendError(err))
	}
	if inUse {
		if prune {
			return false, nil
		}
		return false, errors.New("has Windows network endpoints")
	}
	if !dry {
		if err := backend.deleteNetwork(network.ID); err != nil {
			return false, fmt.Errorf("delete Windows network: %w", networkBackendError(err))
		}
	}
	return true, nil
}
