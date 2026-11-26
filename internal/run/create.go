package run

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/services"
	"github.com/quasidock/quasidock/internal/store"
)

// Create saves a container without preparing or launching its runtime. The
// caller selects or pulls the image first, just as it does before Run.
func Create(ctx *context.Context, opts Options) (*store.ContainerRecord, error) {
	if opts.Detach {
		return nil, errors.New("create does not support --detach; use start to launch the container")
	}
	lock, err := ctx.Store.Lock(false)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	selected, err := selectedImage(ctx, opts)
	if err != nil {
		return nil, err
	}
	img, err := ctx.Store.ReadImage(selected.ID, selected.Manifest)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(img.Config.ID, selected.ID) || img.Config.Version != selected.Version {
		return nil, fmt.Errorf("image catalog metadata disagrees with manifest %s", selected.Manifest)
	}
	return createContainer(ctx, opts, selected, img.Config, "")
}

// Recheck a previously selected digest while holding the store lock, rather
// than following a tag which may have moved since selection.
func selectedImage(ctx *context.Context, opts Options) (*store.Ref, error) {
	if opts.Image != nil {
		return ctx.Store.ResolveLocalImage("", opts.Image.Manifest)
	}
	return ctx.Store.ResolveLocalImage(ctx.Env["QUASIDOCK_REGISTRY"], opts.ID)
}

// createContainer is shared by Create and Run. A nonempty runID reserves an
// imminent launch; an empty one represents an intentionally unstarted container.
// The caller holds the store lock until this record pins its image.
func createContainer(ctx *context.Context, opts Options, selected *store.Ref, cfg oci.AppConfig, runID string) (*store.ContainerRecord, error) {
	bindings, err := services.NormalizePublishing(cfg, opts.Publish, opts.Settings.Ports)
	if err != nil {
		return nil, err
	}
	if opts.RestartPolicy.Name == "" {
		opts.RestartPolicy.Name = "no"
	}
	if err := validateHostConfig(store.ContainerHostConfig{ContainerResources: opts.Resources, RestartPolicy: opts.RestartPolicy, AutoRemove: opts.AutoRemove}); err != nil {
		return nil, err
	}
	for _, spec := range opts.Settings.FilesystemFor(cfg.Permissions.Filesystem) {
		if oci.ParseFsGrant(spec) == nil {
			return nil, ctx.Fail("invalid filesystem permission %q", spec)
		}
	}
	hostCwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := validateMountSources(ctx, opts.Mounts, hostCwd); err != nil {
		return nil, err
	}
	imageRef := opts.ID
	if imageRef == "" {
		imageRef = selected.ID
	}
	var sandboxStr string
	if opts.Settings.Sandbox != nil {
		sandboxStr = string(*opts.Settings.Sandbox)
	}
	rec := store.ContainerRecord{
		Image:      selected.Manifest,
		ImageRef:   imageRef,
		AppID:      cfg.ID,
		AppVersion: cfg.Version,
		Args:       append([]string(nil), opts.Args...),
		State:      store.ContainerState{Status: "created", RunID: runID, SupervisorID: runID},
		Config: store.ContainerConfig{
			Image:       imageRef,
			Cmd:         append([]string(nil), opts.Args...),
			Env:         append([]string(nil), opts.Settings.Env...),
			Tty:         opts.TTY,
			AttachStdin: runID != "" && !opts.Detach && opts.Interactive,
			OpenStdin:   opts.Interactive,
		},
		HostConfig: store.ContainerHostConfig{
			PortBindings:       bindings,
			ContainerResources: opts.Resources,
			RestartPolicy:      opts.RestartPolicy,
			Mounts:             append([]mounts.Spec(nil), opts.Mounts...),
			AutoRemove:         opts.AutoRemove,
			Sandbox:            sandboxStr,
			Network:            opts.Settings.Network,
			Host:               opts.Settings.Host,
			Filesystem:         append([]string(nil), opts.Settings.Filesystem...),
			NoFilesystem:       append([]string(nil), opts.Settings.NoFilesystem...),
			Ports:              append([]string(nil), opts.Settings.Ports...),
		},
		RunSpec: store.ContainerRunSpec{
			Args:       append([]string(nil), opts.Args...),
			Entrypoint: opts.Entrypoint,
			Env:        append([]string(nil), opts.Settings.Env...),
			HostCwd:    hostCwd,
		},
	}
	if opts.Entrypoint != nil {
		rec.Config.Entrypoint = []string{*opts.Entrypoint}
	}
	snapshotInheritedEnv(ctx.Env, &rec.RunSpec)
	return ctx.Store.CreateContainer(opts.Name, rec, ctx.Store.SeedContainerData)
}
