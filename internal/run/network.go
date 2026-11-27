package run

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

type networkChange struct {
	Target  string
	Connect bool
}

func ChangeContainerNetwork(ctx *context.Context, container, target string, connect bool, list func() ([]win32.HCNNetwork, error), validate func(string) error) error {
	rec, op, err := lockTarget(ctx, container)
	if err != nil {
		return err
	}
	defer op.Release()
	if rec.State.Running {
		if !rec.State.NetworkManaged || rec.State.ControlProtocol < 3 {
			if !connect && len(rec.HostConfig.Networks) == 0 {
				return fmt.Errorf("network %q is not connected to container %s", target, rec.ShortID())
			}
			return fmt.Errorf("stop container %s, connect it while stopped, then start it to enable live network changes", rec.ShortID())
		}
		return sendControlRequest(ctx, rec, controlRequest{Action: actionNetwork, Network: &networkChange{Target: target, Connect: connect}})
	}
	if rec.State.Restarting || ctx.Store.IsContainerRunning(rec.ID) ||
		(rec.State.Status == "created" && rec.State.RunID != "" && !rec.State.Launched && rec.State.Error == "" && rec.State.FinishedAt == "") {
		return fmt.Errorf("container %s is starting or restarting; retry when it reaches a stable state", rec.ShortID())
	}
	if !connect {
		id, err := resolveSavedNetwork(target, rec.HostConfig.Networks)
		if err != nil {
			return err
		}
		_, err = ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
			delete(c.HostConfig.Networks, id)
			return nil
		})
		return err
	}
	all, err := list()
	if err != nil {
		return err
	}
	network, err := win32.ResolveHCNNetwork(target, all)
	if err != nil {
		return err
	}
	if _, exists := rec.HostConfig.Networks[network.ID]; exists {
		return fmt.Errorf("network %q is already connected to container %s", target, rec.ShortID())
	}
	image, err := ctx.Store.ReadImage(rec.AppID, rec.Image)
	if err != nil {
		return err
	}
	settings := Settings{Network: rec.HostConfig.Network}
	if rec.HostConfig.Sandbox != "" {
		sandbox := oci.Sandbox(rec.HostConfig.Sandbox)
		settings.Sandbox = &sandbox
	}
	permissions := image.Config.Permissions
	if err := validateContainerNetworkGrant(SandboxOf(settings, permissions), effectiveNetworkGrant(settings, permissions)); err != nil {
		return err
	}
	_, err = ctx.Store.UpdateContainerNetwork(rec.ID, true, func(c *store.ContainerRecord) error {
		// Removal holds the index lock through its reference check and HCN
		// deletion. Validate under that same lock before adding the reference.
		if err := validate(network.ID); err != nil {
			return err
		}
		saveNetworkMembership(c, network.ID, network.Name)
		return nil
	})
	return err
}

func resolveSavedNetwork(target string, saved map[string]string) (string, error) {
	all := make([]win32.HCNNetwork, 0, len(saved))
	for id, name := range saved {
		all = append(all, win32.HCNNetwork{ID: id, Name: name})
	}
	network, err := win32.ResolveHCNNetwork(target, all)
	if err != nil {
		return "", fmt.Errorf("resolve connected network: %w", err)
	}
	return network.ID, nil
}

func saveNetworkMembership(c *store.ContainerRecord, id, name string) {
	if c.HostConfig.Networks == nil {
		c.HostConfig.Networks = make(map[string]string)
	}
	c.HostConfig.Networks[id] = name
}

func effectiveNetworkGrant(settings Settings, permissions oci.Permissions) bool {
	if settings.Network != nil {
		return *settings.Network
	}
	return permissions.Network
}

func validateContainerNetworkGrant(sandbox oci.Sandbox, permitted bool) error {
	if sandbox == oci.SandboxAppContainer && !permitted {
		return errors.New("AppContainer network attachment requires image network permission or a container created with --share=network")
	}
	return nil
}

func (h *networkHelper) prepare(job win32.HANDLE, saved map[string]string) (map[string]store.NetworkAttachment, error) {
	if _, err := h.call(networkRequest{Action: "begin", Job: job}); err != nil {
		return nil, err
	}
	active := make(map[string]store.NetworkAttachment, len(saved))
	for _, id := range slices.Sorted(maps.Keys(saved)) {
		reply, err := h.call(networkRequest{Action: "connect", Target: id})
		if err != nil {
			return nil, fmt.Errorf("starting with private network %q (%s): %w", saved[id], id, err)
		}
		active[reply.ID] = reply.Endpoint
	}
	return active, nil
}

func (r *execRuntime) changeNetworkLocked(ctx *context.Context, rec *store.ContainerRecord, change networkChange) error {
	if r.network == nil || change.Target == "" {
		return errors.New("network changes require a network-managed container")
	}
	request := networkRequest{Action: "connect", Target: change.Target}
	if !change.Connect {
		current, err := ctx.Store.ReadContainer(rec.ID)
		if err != nil {
			return err
		}
		id, err := resolveSavedNetwork(change.Target, current.HostConfig.Networks)
		if err != nil {
			return err
		}
		request = networkRequest{Action: "disconnect", Target: id}
	}
	reply, err := r.network.call(request)
	if err != nil {
		if reply.Fatal {
			r.stopLocked()
			win32.TerminateJobObject(r.job, 1)
		}
		return err
	}
	_, err = ctx.Store.UpdateContainerNetwork(rec.ID, change.Connect, func(c *store.ContainerRecord) error {
		if c.State.RunID != r.runID || !c.State.Running || c.NetworkSettings == nil {
			return errors.New("container run changed while saving its network membership")
		}
		if change.Connect {
			saveNetworkMembership(c, reply.ID, reply.Endpoint.Name)
			if c.NetworkSettings.Networks == nil {
				c.NetworkSettings.Networks = make(map[string]store.NetworkAttachment)
			}
			c.NetworkSettings.Networks[reply.ID] = reply.Endpoint
		} else {
			delete(c.HostConfig.Networks, reply.ID)
			delete(c.NetworkSettings.Networks, reply.ID)
		}
		return nil
	})
	if err != nil {
		rollback := networkRequest{Action: "restore", Target: reply.ID}
		if change.Connect {
			rollback.Action = "disconnect"
		}
		_, rollbackErr := r.network.call(rollback)
		if rollbackErr == nil && change.Connect {
			_, rollbackErr = r.network.call(networkRequest{Action: "discard", Target: reply.ID})
		}
		if rollbackErr != nil {
			r.stopLocked()
			win32.TerminateJobObject(r.job, 1)
			return errors.Join(err, fmt.Errorf("rolling back network change: %w", rollbackErr))
		}
		return err
	}
	if !change.Connect {
		cleanup, cleanupErr := r.network.call(networkRequest{Action: "discard", Target: reply.ID})
		if cleanupErr != nil {
			if cleanup.Fatal {
				r.stopLocked()
				win32.TerminateJobObject(r.job, 1)
			}
			ctx.Note("warning: reclaiming detached Windows endpoint: %v", cleanupErr)
		}
	}
	return nil
}
