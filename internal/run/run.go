package run

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/forward"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/lazy"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/roots"
	"github.com/quasidock/quasidock/internal/services"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
	"github.com/quasidock/quasidock/internal/written"
)

// Settings holds per-run option overrides passed on the command line.
type Settings struct {
	Sandbox      *oci.Sandbox
	Filesystem   []string
	NoFilesystem []string
	Network      *bool
	Host         *bool
	Env          []string
	Ports        []string
}

// SandboxOf returns the sandbox chosen for a run from settings and image permissions.
func SandboxOf(s Settings, p oci.Permissions) oci.Sandbox {
	if s.Sandbox != nil {
		return *s.Sandbox
	}
	if p.Sandbox != "" {
		return p.Sandbox
	}
	return oci.SandboxLow
}

// FilesystemFor applies per-run filesystem grants and removals over base image permissions.
func (s Settings) FilesystemFor(base []string) []string {
	return without(mergeList(base, s.Filesystem), s.NoFilesystem)
}

func mergeList(base, added []string) []string {
	out := append([]string(nil), base...)
	for _, a := range added {
		ak := grantKey(a)
		replaced := false
		for i, existing := range out {
			if grantKey(existing) == ak {
				out[i] = a
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, a)
		}
	}
	return out
}

func without(base, removed []string) []string {
	if len(removed) == 0 {
		return base
	}
	drop := make(map[string]bool, len(removed))
	for _, r := range removed {
		drop[grantKey(r)] = true
	}
	out := make([]string, 0, len(base))
	for _, b := range base {
		if !drop[grantKey(b)] {
			out = append(out, b)
		}
	}
	return out
}

func grantKey(spec string) string {
	s := strings.TrimSuffix(strings.TrimSuffix(spec, ":ro"), ":rw")
	if filepath.IsAbs(s) {
		return strings.ToLower(filepath.Clean(s))
	}
	return s
}

type Options struct {
	Resources     store.ContainerResources
	RestartPolicy store.RestartPolicy
	Mounts        []mounts.Spec
	ID            string
	Args          []string
	Entrypoint    *string
	Pull          string
	Image         *store.Ref
	Name          string
	Detach        bool
	AutoRemove    bool
	Interactive   bool
	TTY           bool
	ContainerID   string
	Settings      Settings
	operation     *store.ContainerOperation
	runLock       *store.Lock
	attachment    *attachRuntime
	attachStart   bool
	controlEvent  win32.HANDLE
}

// ReadyEventName returns the named manual-reset Win32 event signaled by a
// detached container supervisor once the container process has launched.
func ReadyEventName(id, runID string) string {
	return `Local\quasidock-ctr-ready-` + id + "-" + runID
}

type HostGrant struct {
	Path     *string
	ReadOnly bool
}

type Loopback int

const (
	LoopbackOpen Loopback = iota
	LoopbackExempt
	LoopbackIsolated
)

func Run(ctx *context.Context, opts Options) (uint32, error) {
	storeLock, err := ctx.Store.Lock(false)
	if err != nil {
		return 1, err
	}
	defer storeLock.Release()

	selected, err := selectedImage(ctx, opts)
	if err != nil {
		return 1, err
	}

	img, err := ctx.Store.LoadImageRef(*selected)
	if err != nil {
		return 1, err
	}

	runID, err := newRunID()
	if err != nil {
		img.Release()
		return 1, err
	}

	created, err := createContainer(ctx, opts, selected, img.Config, runID)
	if err != nil {
		img.Release()
		storeLock.Release()
		return 1, err
	}
	opts.ID = selected.ID
	opts.ContainerID = created.ID
	hostCwd := created.RunSpec.HostCwd
	op, err := ctx.Store.LockContainer(created.ID)
	if err != nil {
		img.Release()
		return 1, err
	}
	defer op.Release()
	if _, err := ctx.Store.ReadContainer(created.ID); err != nil {
		img.Release()
		return 1, err
	}

	img.Release()
	storeLock.Release()
	if !opts.Detach {
		stream := AttachOptions{Container: created.ID, NoStdin: !opts.Interactive, SigProxy: true}
		code, err := startAttachedRecord(ctx, created, op, stream)
		if err != nil && opts.AutoRemove {
			op.Release()
			autoRemoveContainer(ctx, created)
		}
		return code, err
	}
	if err := launchSupervisor(ctx, created, hostCwd); err != nil {
		if !ctx.Store.IsContainerRunning(created.ID) {
			recordLaunchFailure(ctx.Store, created.ID, created.State.RunID, err)
			if opts.AutoRemove {
				_ = op.Remove()
			}
		}
		return 1, err
	}
	fmt.Fprintln(ctx.Stdout, created.ID)
	return 0, nil
}

const supervisorStartupTimeout win32.DWORD = 120000

func launchSupervisor(ctx *context.Context, created *store.ContainerRecord, hostCwd string) error {
	det, readyEvent, err := spawnSupervisor(ctx, created, hostCwd, false)
	if err != nil {
		return err
	}
	defer win32.CloseHandle(readyEvent)
	defer win32.CloseHandle(det.Process)
	// A cold AppContainer start grants every file in large runtime images.
	// This is launch preparation, before the application's own readiness.
	return awaitSupervisor(ctx, created, det.Process, readyEvent, supervisorStartupTimeout)
}

func spawnSupervisor(ctx *context.Context, created *store.ContainerRecord, hostCwd string, attach bool) (process.Detached, win32.HANDLE, error) {
	readyEvent, err := win32.CreateEvent(ReadyEventName(created.ID, created.State.RunID), true, false)
	if err != nil {
		return process.Detached{}, 0, fmt.Errorf("creating container ready event: %w", err)
	}

	exePath, err := os.Executable()
	if err != nil {
		win32.CloseHandle(readyEvent)
		return process.Detached{}, 0, err
	}
	var supArgs []string
	if ctx.Verbose {
		supArgs = append(supArgs, "--verbose")
	}
	supArgs = append(supArgs, "container-supervisor", ctx.Store.Root, created.ID, created.State.RunID)
	if attach {
		supArgs = append(supArgs, "attach")
	}
	cmdline := process.BuildCommandLine(exePath, supArgs)
	det, err := process.StartDetached(exePath, cmdline, hostCwd)
	if err != nil {
		win32.CloseHandle(readyEvent)
		return process.Detached{}, 0, fmt.Errorf("starting detached supervisor: %w", err)
	}
	return det, readyEvent, nil
}

func awaitSupervisor(ctx *context.Context, created *store.ContainerRecord, supervisor, readyEvent win32.HANDLE, timeoutMs win32.DWORD) error {
	waitRes := win32.WaitForMultipleObjects([]win32.HANDLE{readyEvent, supervisor}, false, timeoutMs)
	if waitRes == win32.WAIT_OBJECT_0 || waitRes == win32.WAIT_OBJECT_0+1 {
		rec, readErr := ctx.Store.ReadContainer(created.ID)
		if readErr != nil {
			return readErr
		}
		if (rec.State.RunID == created.State.RunID && rec.State.Launched) ||
			(created.State.SupervisorID != "" && rec.State.SupervisorID == created.State.SupervisorID && rec.RestartCount > 0) {
			return nil
		}
		if rec.State.Error != "" {
			if !waitForContainer(ctx.Store, created.ID, time.Now().Add(5*time.Second)) {
				return fmt.Errorf("container %s failed startup but did not finish cleanup: %s", created.ShortID(), rec.State.Error)
			}
			return errors.New(rec.State.Error)
		}
		return errors.New("container supervisor exited before container started")
	}
	// This is the handle returned by CreateProcess, not a PID from metadata.
	if !win32.TerminateProcess(supervisor, 1) {
		return errors.New("canceling container startup failed")
	}
	if win32.WaitForSingleObject(supervisor, 5000) != win32.WAIT_OBJECT_0 ||
		!waitForContainer(ctx.Store, created.ID, time.Now().Add(5*time.Second)) {
		return fmt.Errorf("container %s startup cancellation did not complete", created.ShortID())
	}
	_ = hkcu.SettleLeftovers(ctx.Store, created.AppID)
	_ = grants.RepairMounts(ctx.Store)
	if waitRes != win32.WAIT_TIMEOUT {
		return fmt.Errorf("waiting for container %s startup failed: %d", created.ShortID(), waitRes)
	}
	return fmt.Errorf("timed out waiting for container %s to start", created.ShortID())
}

// superviseAttempt loads the pinned image and executes one payload launch.
func superviseAttempt(ctx *context.Context, rec *store.ContainerRecord, runLock *store.Lock, event, readyEvent win32.HANDLE, attached bool, operation *store.ContainerOperation) (code uint32, runErr error) {
	var err error
	runID := rec.State.RunID
	var sandboxPtr *oci.Sandbox
	if rec.HostConfig.Sandbox != "" {
		sb := oci.Sandbox(rec.HostConfig.Sandbox)
		sandboxPtr = &sb
	}
	opts := Options{
		Mounts:        append([]mounts.Spec(nil), rec.HostConfig.Mounts...),
		ID:            rec.AppID,
		Args:          append([]string(nil), rec.RunSpec.Args...),
		Entrypoint:    rec.RunSpec.Entrypoint,
		Detach:        true,
		AutoRemove:    rec.HostConfig.AutoRemove,
		Interactive:   rec.Config.OpenStdin,
		TTY:           rec.Config.Tty,
		ContainerID:   rec.ID,
		Name:          rec.BareName(),
		runLock:       runLock,
		controlEvent:  event,
		operation:     operation,
		Resources:     rec.HostConfig.ContainerResources,
		RestartPolicy: rec.HostConfig.RestartPolicy,
		Settings: Settings{
			Sandbox:      sandboxPtr,
			Filesystem:   append([]string(nil), rec.HostConfig.Filesystem...),
			NoFilesystem: append([]string(nil), rec.HostConfig.NoFilesystem...),
			Network:      rec.HostConfig.Network,
			Host:         rec.HostConfig.Host,
			Env:          append([]string(nil), rec.RunSpec.Env...),
			Ports:        append([]string(nil), rec.HostConfig.Ports...),
		},
	}
	opts.attachStart = attached
	opts.attachment, err = newAttachRuntime(rec.ID, runID, rec.Config.OpenStdin, rec.Config.Tty)
	if err != nil {
		recordLaunchFailure(ctx.Store, rec.ID, runID, err)
		return 1, err
	}
	defer func() { opts.attachment.finish(code, runErr) }()

	storeLock, err := ctx.Store.Lock(false)
	if err != nil {
		recordLaunchFailure(ctx.Store, rec.ID, runID, err)
		return 1, err
	}
	selected, err := ctx.Store.ResolveLocalImage("", rec.Image)
	if err != nil {
		storeLock.Release()
		recordLaunchFailure(ctx.Store, rec.ID, runID, err)
		return 1, err
	}
	lazyLayers, err := lazyLayersOf(ctx, selected.Manifest)
	if err != nil {
		storeLock.Release()
		recordLaunchFailure(ctx.Store, rec.ID, runID, err)
		return 1, err
	}
	_ = lazy.EnsureServed(ctx.Store, lazyLayers, ctx.Verbose)
	failuresBefore := make([]int, len(lazyLayers))
	for i, l := range lazyLayers {
		hex := oci.DigestHex(l.Digest)
		fails, _ := lazy.FailedFetches(ctx.Store, hex, 0)
		failuresBefore[i] = len(fails)
	}
	img, err := ctx.Store.LoadImageRef(*selected)
	if err != nil {
		storeLock.Release()
		recordLaunchFailure(ctx.Store, rec.ID, runID, err)
		return 1, err
	}
	storeLock.Release()

	hostCwd := rec.RunSpec.HostCwd
	if hostCwd == "" {
		hostCwd, _ = os.Getwd()
	}
	return executeContainer(ctx, rec, selected, img, lazyLayers, failuresBefore, opts, hostCwd, true, readyEvent)
}

func recordLaunchFailure(st *store.Store, id, runID string, err error) {
	if err == nil {
		return
	}
	_, _ = st.UpdateContainer(id, func(c *store.ContainerRecord) error {
		if c.State.RunID != runID {
			return nil
		}
		c.State = store.ContainerState{
			SupervisorID: c.State.SupervisorID,
			RunID:        runID, Status: "created", ExitCode: 1, Error: err.Error(),
			FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		return nil
	})
}

func executeContainer(
	ctx *context.Context,
	created *store.ContainerRecord,
	selected *store.Ref,
	img *store.Image,
	lazyLayers []oci.Descriptor,
	failuresBefore []int,
	opts Options,
	hostCwd string,
	detached bool,
	readyEvent win32.HANDLE,
) (uint32, error) {
	runLock := opts.runLock
	ownsRunLock := runLock == nil
	if runLock == nil {
		var err error
		runLock, err = store.OpenLock(ctx.Store.ContainerRunLockPath(created.ID), true, false)
		if err != nil {
			img.Release()
			return 1, err // A losing launcher must not overwrite the active run.
		}
	}

	var code uint32 = 1
	var launched bool
	var runErr error

	defer func() {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if launched {
			_, _ = ctx.Store.UpdateContainer(created.ID, func(c *store.ContainerRecord) error {
				if c.State.RunID != created.State.RunID {
					return nil
				}
				c.State.Running = false
				c.State.Paused = false
				c.State.Status = "exited"
				c.State.Pid = 0
				c.State.ExitCode = int(code)
				c.State.FinishedAt = now
				if runErr != nil {
					c.State.Error = runErr.Error()
				}
				return nil
			})
		} else {
			_, _ = ctx.Store.UpdateContainer(created.ID, func(c *store.ContainerRecord) error {
				if c.State.RunID != created.State.RunID {
					return nil
				}
				c.State.Running = false
				c.State.Paused = false
				c.State.Status = "created"
				c.State.Pid = 0
				c.State.ExitCode = 1
				c.State.FinishedAt = now
				if runErr != nil {
					c.State.Error = runErr.Error()
				}
				return nil
			})
		}
		if readyEvent != 0 {
			win32.SetEvent(readyEvent)
		}
		if opts.attachment != nil {
			opts.attachment.finish(code, runErr)
		}
		if ownsRunLock {
			runLock.Release()
		}
		if opts.operation != nil {
			opts.operation.Release()
		}
		if ownsRunLock && created.HostConfig.AutoRemove && (launched || !detached) {
			autoRemoveContainer(ctx, created)
		}
	}()

	code, launched, runErr = launchContainer(ctx, created, selected, img, lazyLayers, failuresBefore, opts, hostCwd, detached, readyEvent)
	return code, runErr
}

func launchContainer(
	ctx *context.Context,
	created *store.ContainerRecord,
	selected *store.Ref,
	img *store.Image,
	lazyLayers []oci.Descriptor,
	failuresBefore []int,
	opts Options,
	hostCwd string,
	detached bool,
	readyEvent win32.HANDLE,
) (uint32, bool, error) {
	defer img.Release()
	cfg := img.Config
	appLease, imageLease, err := ctx.Store.BeginImageRun(*selected)
	if err != nil {
		return 1, false, err
	}
	defer imageLease.Release()
	runs := &grants.Session{ID: cfg.ID, LockPath: ctx.Store.RunsLockPath(cfg.ID), Lock: appLease}
	defer func() { _ = runs.End(ctx.Store) }()

	settings := opts.Settings
	sandbox := SandboxOf(settings, cfg.Permissions)
	WarnUnsupported(cfg, sandbox, ctx.Note)

	network := cfg.Permissions.Network
	if settings.Network != nil {
		network = *settings.Network
	}

	var hostGrants []HostGrant
	cwdGranted := false

	fsSpecs := settings.FilesystemFor(cfg.Permissions.Filesystem)
	for _, spec := range fsSpecs {
		g := oci.ParseFsGrant(spec)
		if g == nil {
			return 1, false, fmt.Errorf("invalid filesystem permission %q", spec)
		}
		switch g.Where.Type {
		case oci.FsGrantCwd:
			cwdGranted = true
			hostGrants = append(hostGrants, HostGrant{Path: nil, ReadOnly: g.ReadOnly})
		case oci.FsGrantPath:
			p := g.Where.Path
			hostGrants = append(hostGrants, HostGrant{Path: &p, ReadOnly: g.ReadOnly})
		default:
			ctx.Note("warning: %s asks for the filesystem permission %q, which this quasidock doesn't know, and leaves out; a newer quasidock may", cfg.ID, spec)
		}
	}

	dataDir := created.DataDir
	if sandbox == oci.SandboxSoft && len(created.Mounts) == 0 {
		_ = unlabelData(ctx, cfg.ID, dataDir)
	}
	mountSession, err := prepareContainerMounts(ctx, created, sandbox, hostGrants, hostCwd)
	if mountSession != nil {
		defer func() {
			if e := mountSession.Close(); e != nil {
				ctx.Note("warning: mount permissions need repair: %v", e)
			}
		}()
	}
	if err != nil {
		return 1, false, err
	}
	profile := environment.NewProfile(dataDir)

	quickAccess := filepath.Join(profile.Roaming, "Microsoft", "Windows", "Recent", "AutomaticDestinations")
	_ = os.MkdirAll(quickAccess, 0o755)
	_ = os.MkdirAll(profile.Temp, 0o755)

	cwd := profile.Home
	if cwdGranted {
		cwd = hostCwd
	}
	if cfg.Workdir != "" {
		cwd = imageWorkdir(oci.Placeholders{App: img.DeployDir, Data: dataDir, Cache: filepath.Join(dataDir, "cache")}, cfg.Workdir)
		info, err := os.Stat(cwd)
		if err != nil {
			return 1, false, fmt.Errorf("image workdir: %w", err)
		}
		if !info.IsDir() {
			return 1, false, fmt.Errorf("image workdir %s is not a directory", cwd)
		}
	}

	cacheDir := filepath.Join(dataDir, "cache")
	if usesCache(cfg) {
		_ = os.MkdirAll(cacheDir, 0o755)
	}

	var rootsFile *string
	if usesRoots(cfg) {
		rf, err := roots.File(ctx.Store)
		if err == nil {
			rootsFile = &rf
		}
	}

	systemRoot := ctx.Env["SystemRoot"]
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}

	used := services.UsedBy(cfg)
	targets := services.Targets(used, settings.Ports)
	servicePorts, err := services.Resolve(ctx.Store, created.ID, cfg, used, settings.Ports, sandbox)
	if err != nil {
		return 1, false, err
	}

	reach := LoopbackOpen
	if sandbox == oci.SandboxAppContainer {
		reach, err = exemptLoopback(ctx, cfg.ID, network, len(targets) > 0)
		if err != nil {
			return 1, false, err
		}
	}

	forwarding := reach == LoopbackIsolated && len(targets) > 0
	if forwarding {
		_ = services.ForwardThrough(ctx.Store, targets, servicePorts)
	}
	uses := services.UsePlaceholders(targets)
	rt := runtimeDirs(cfg, img.RuntimeDirs, dataDir, cacheDir, rootsFile, uses)
	rootsPath := ""
	if rootsFile != nil {
		rootsPath = *rootsFile
	}
	placeholders := oci.Placeholders{
		App:      img.DeployDir,
		Data:     dataDir,
		Cache:    cacheDir,
		Roots:    rootsPath,
		Runtimes: rt.dirs,
		Services: services.Placeholders(servicePorts),
		Uses:     uses,
	}

	var appPath []string
	appDirs, _ := environment.PathDirs(placeholders, cfg.Path)
	appPath = append(appPath, appDirs...)
	for i, r := range rt.own {
		alias := rt.aliases[i]
		rCfg := cfg.From[alias]
		pDirs, _ := environment.PathDirs(r, rCfg.Path)
		appPath = append(appPath, pDirs...)
	}

	cmd, err := commandForRun(placeholders, cfg, opts, appPath, systemRoot)
	if err != nil {
		return 1, false, err
	}
	var allArgs []string
	allArgs = append(allArgs, cmd.Args...)

	exe := cmd.Exe
	if cmd.Batch {
		exe = filepath.Join(systemRoot, "System32", "cmd.exe")
	}

	var commandLine string
	if cmd.Batch {
		commandLine, err = process.BuildBatchCommandLine(exe, cmd.Exe, allArgs)
		if err != nil {
			if errors.Is(err, process.ErrInvalidBatchArgument) {
				return 1, false, fmt.Errorf("%s is a batch file, and cmd.exe can't pass it an argument with a line break or a NUL character", cmd.Exe)
			}
			return 1, false, fmt.Errorf("%s can't be run as a batch file: %v", cmd.Exe, err)
		}
	} else {
		commandLine = process.BuildCommandLine(exe, allArgs)
	}

	var appEnv []environment.Var
	for i, r := range rt.own {
		alias := rt.aliases[i]
		rCfg := cfg.From[alias]
		_ = environment.Expand(&appEnv, r, rCfg.Env)
	}
	_ = environment.Expand(&appEnv, placeholders, cfg.Env)

	sharedPathDirs := sharedPath(ctx, cfg, settings, sandbox)
	envVars, err := environment.Build(containerHostEnv(ctx.Env, created.RunSpec), environment.Spec{
		Unset:      cfg.UnsetEnv,
		ID:         cfg.ID,
		Profile:    profile,
		Path:       appPath,
		PathAfter:  sharedPathDirs,
		SystemRoot: systemRoot,
		Vars:       appEnv,
		Extra:      settings.Env,
		Proxies:    network,
	})
	if err != nil {
		return 1, false, err
	}

	if ctx.Verbose {
		ctx.Note("app      %s %s (%s)", cfg.ID, cfg.Version, img.Ref.Manifest)
		for alias, r := range cfg.From {
			ctx.Note("from     %s: %s %s", alias, r.ID, r.Version)
		}
		netStr := "off"
		if network {
			netStr = "on"
		}
		ctx.Note("sandbox  %v, network %s", sandbox, netStr)
		switch reach {
		case LoopbackOpen:
		case LoopbackExempt:
			ctx.Note("loopback exempt while the app's runs go: it reaches other programs on this machine")
		case LoopbackIsolated:
			if forwarding {
				ctx.Note("loopback isolated: it reaches other programs on this machine only on the ports it uses, which quasidock forwards")
			} else {
				ctx.Note("loopback isolated: it reaches no other program on this machine")
			}
		}
		for _, s := range servicePorts {
			if s.Listen == s.Port {
				ctx.Note("service  %s: localhost:%d", s.Service.Name, s.Port)
			} else {
				ctx.Note("service  %s: localhost:%d, which the app listens for on %d in its container", s.Service.Name, s.Port, s.Listen)
			}
		}
		for _, t := range targets {
			if t.Connect == t.Port {
				ctx.Note("uses     %s: localhost:%d", t.Use.Name, t.Port)
			} else {
				ctx.Note("uses     %s: localhost:%d, which quasidock forwards from %d in its container", t.Use.Name, t.Port, t.Connect)
			}
		}
		ctx.Note("exe      %s", exe)
		if cmd.Batch {
			ctx.Note("script   %s", cmd.Exe)
		}
		ctx.Note("cmdline  %s", commandLine)
		ctx.Note("cwd      %s", cwd)
		for _, kv := range envVars {
			ctx.Note("env      %s=%s", kv.Name, kv.Value)
		}
	}

	var published *services.Published
	if sandbox == oci.SandboxAppContainer {
		cmdStr := ""
		if opts.Entrypoint != nil {
			cmdStr = *opts.Entrypoint
		} else if len(opts.Args) > 0 {
			cmdStr = opts.Args[0]
		}
		published, _ = services.Publish(cfg, servicePorts, cmdStr)
	}
	defer func() {
		if published != nil {
			published.Close()
		}
	}()

	var forwards *forward.Forwards
	if forwarding {
		forwards, _ = forwardsOf(targets)
	}
	defer func() {
		if forwards != nil {
			forwards.Close()
		}
	}()

	relayExe := ""
	if published != nil || forwards != nil {
		relayExe, _ = services.RelayExe(ctx.Store)
	}

	if sandbox == oci.SandboxSoft {
		if rid, err := acl.EffectiveLabel(exe); err == nil && rid != nil && *rid <= win32.SECURITY_MANDATORY_LOW_RID {
			ctx.Note("warning: %s is labelled low integrity, so Windows starts it, and this run, at low integrity; `quasidock repair` takes back a label quasidock gave a folder it's in", exe)
		}
	}

	own := grants.NewOwn(ctx.Store.Root)
	var security *win32.SECURITY_CAPABILITIES
	var token win32.HANDLE

	switch sandbox {
	case oci.SandboxSoft:
		if ctx.Verbose {
			for _, g := range hostGrants {
				if g.Path != nil {
					ctx.Note("note: soft sandbox doesn't enforce filesystem permissions; %s is as reachable as any other path", *g.Path)
				}
			}
		}
	case oci.SandboxLow:
		t, err := setUpLow(ctx, cfg.ID, lowSetup{
			dataPrepared: len(created.Mounts) > 0,
			dataDir:      dataDir,
			hostCwd:      hostCwd,
			grants:       hostGrants,
			network:      network,
			own:          own,
		})
		if err != nil {
			return 1, false, err
		}
		token = t
		defer win32.CloseHandle(token)
	case oci.SandboxAppContainer:
		relayDir := ""
		if relayExe != "" {
			relayDir = filepath.Dir(relayExe)
		}
		rootsPath := ""
		if rootsFile != nil {
			rootsPath = *rootsFile
		}
		sec, err := setUpAppContainer(ctx, cfg.ID, appContainerSetup{
			dataPrepared: len(created.Mounts) > 0,
			deployDir:    img.DeployDir,
			runtimeDirs:  img.RuntimeDirs,
			relayDir:     relayDir,
			rootsFile:    rootsPath,
			dataDir:      dataDir,
			localAppData: profile.Local,
			hostCwd:      hostCwd,
			grants:       hostGrants,
			network:      network,
			own:          own,
		})
		if err != nil {
			return 1, false, err
		}
		security = sec
	}

	if sandbox != oci.SandboxSoft {
		_ = written.Relabel(ctx.Store, cfg.ID)
	}

	var hkcuAccess hkcu.Access
	switch sandbox {
	case oci.SandboxSoft:
		hkcuAccess = hkcu.Access{Type: hkcu.AccessNone}
	case oci.SandboxLow:
		hkcuAccess = hkcu.Access{Type: hkcu.AccessLow}
	case oci.SandboxAppContainer:
		capSid, _ := appcontainer.AppCapability(cfg.ID)
		hkcuAccess = hkcu.Access{Type: hkcu.AccessCapability, SID: capSid}
	}
	// An AutoRemove container may be explicitly restarted. Keep its hive until
	// record removal, which deletes private data while preserving mounted data.
	keysSession, err := hkcu.Enter(ctx.Store, cfg.ID, cfg.Registry, dataDir, hkcuAccess, true)
	if err != nil {
		return 1, false, fmt.Errorf("entering container registry: %w", err)
	}
	defer func() {
		if keysSession != nil {
			hkcu.Leave(ctx.Store, keysSession)
		}
	}()

	if ctx.Verbose && len(cfg.Registry) > 0 {
		ctx.Note("registry %s (kept in %s\\registry)", strings.Join(cfg.Registry, ", "), dataDir)
	}

	envBlock, err := environment.EncodeBlock(envVars)
	if err != nil {
		return 1, false, err
	}

	controlEvent := opts.controlEvent
	if controlEvent == 0 {
		controlEvent, err = win32.CreateEvent(controlEventName(created.ID, controlKey(created)), false, false)
		if err != nil {
			return 1, false, fmt.Errorf("creating container control event: %w", err)
		}
		defer win32.CloseHandle(controlEvent)
	}

	logFile, err := os.OpenFile(created.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 1, false, fmt.Errorf("opening container log: %w", err)
	}
	defer func() {
		if logFile != nil {
			_ = logFile.Close()
		}
	}()

	const pipeSize = 64 * 1024
	outRead, outWrite, err := win32.CreatePipe(nil, pipeSize)
	if err != nil {
		return 1, false, err
	}
	errRead, errWrite, err := win32.CreatePipe(nil, pipeSize)
	if err != nil {
		win32.CloseHandle(outRead)
		win32.CloseHandle(outWrite)
		return 1, false, err
	}

	var stdinHandle win32.HANDLE
	var nulIn win32.HANDLE
	var pipeIn *os.File
	if opts.attachment != nil && opts.Interactive {
		var writer *os.File
		pipeIn, writer, err = os.Pipe()
		if err != nil {
			win32.CloseHandle(outRead)
			win32.CloseHandle(outWrite)
			win32.CloseHandle(errRead)
			win32.CloseHandle(errWrite)
			return 1, false, err
		}
		defer func() { _ = pipeIn.Close() }()
		opts.attachment.setInput(writer)
		stdinHandle = win32.HANDLE(pipeIn.Fd())
	} else if !detached && opts.Interactive {
		stdinHandle = win32.GetStdHandle(win32.STD_INPUT_HANDLE)
	} else {
		nulIn = openNulRead()
		stdinHandle = nulIn
	}
	stdio := [3]win32.HANDLE{stdinHandle, outWrite, errWrite}
	if opts.attachStart {
		select {
		case <-opts.attachment.gate:
		case <-time.After(execStartupTimeout):
			win32.CloseHandle(outRead)
			win32.CloseHandle(outWrite)
			win32.CloseHandle(errRead)
			win32.CloseHandle(errWrite)
			if nulIn != 0 {
				win32.CloseHandle(nulIn)
			}
			return 1, false, errors.New("timed out waiting for startup attachment")
		}
	}

	var creationFlags win32.DWORD
	if detached {
		creationFlags = win32.CREATE_NEW_PROCESS_GROUP
	}

	job, err := process.NewJob()
	if err != nil {
		win32.CloseHandle(outRead)
		win32.CloseHandle(outWrite)
		win32.CloseHandle(errRead)
		win32.CloseHandle(errWrite)
		if nulIn != 0 {
			win32.CloseHandle(nulIn)
		}
		return 1, false, err
	}
	child := process.Child{Job: job}
	defer child.Close()
	if _, _, err := setJobResources(job, created.HostConfig.ContainerResources); err != nil {
		win32.CloseHandle(outRead)
		win32.CloseHandle(outWrite)
		win32.CloseHandle(errRead)
		win32.CloseHandle(errWrite)
		if nulIn != 0 {
			win32.CloseHandle(nulIn)
		}
		return 1, false, fmt.Errorf("setting container resource limits: %w", err)
	}

	if relayExe != "" {
		startRelay(ctx, cfg.ID, job, relayExe, published, forwards, security, envBlock)
	}

	watcherDirs := watchedDirs(sandbox, hostGrants, hostCwd, dataDir, ctx.Env["USERPROFILE"], own)
	if sandbox == oci.SandboxLow {
		for _, m := range created.Mounts {
			watcherDirs = append(watcherDirs, m.Source)
		}
	}
	watcher := written.Start(watcherDirs)

	hProcess, err := process.StartIn(job, process.SpawnSpec{
		Exe:           exe,
		CommandLine:   commandLine,
		EnvBlock:      envBlock,
		Cwd:           cwd,
		Security:      security,
		Token:         token,
		Stdio:         &stdio,
		CreationFlags: creationFlags,
	})
	win32.CloseHandle(outWrite)
	win32.CloseHandle(errWrite)
	if pipeIn != nil {
		_ = pipeIn.Close()
	}
	if nulIn != 0 {
		win32.CloseHandle(nulIn)
	}
	if err != nil {
		win32.CloseHandle(outRead)
		win32.CloseHandle(errRead)
		if watcher != nil {
			_, _ = watcher.Stop()
		}
		return 1, false, err
	}

	var logMu sync.Mutex
	logW := lockedWriter{mu: &logMu, w: logFile}
	var outDst io.Writer = logW
	var errDst io.Writer = logW
	if opts.attachment != nil {
		outDst = io.MultiWriter(logW, attachWriter{opts.attachment, attachStdout})
		errDst = io.MultiWriter(logW, attachWriter{opts.attachment, attachStderr})
	}
	if !detached {
		stdout := ctx.Stdout
		if stdout == nil {
			stdout = os.Stdout
		}
		stderr := ctx.Stderr
		if stderr == nil {
			stderr = os.Stderr
		}
		outDst = io.MultiWriter(stdout, logW)
		errDst = io.MultiWriter(stderr, logW)
	}

	var pipeWG sync.WaitGroup
	var pumpsOnce sync.Once
	startPumps := func() {
		pumpsOnce.Do(func() {
			pipeWG.Add(2)
			go func() {
				defer pipeWG.Done()
				pumpPipe(outRead, outDst)
			}()
			go func() {
				defer pipeWG.Done()
				pumpPipe(errRead, errDst)
			}()
		})
	}
	if opts.attachment == nil {
		startPumps()
	}

	child.Process = hProcess
	live := &execRuntime{
		primary: hProcess, job: job, token: token, security: security,
		env: envVars, cwd: cwd, systemRoot: systemRoot, placeholders: placeholders,
	}
	live.commitContext, live.commitRecord = ctx, created
	if err := live.start(created.ID, created.State.RunID); err != nil {
		process.EndJob(job)
		startPumps()
		pipeWG.Wait()
		if watcher != nil {
			written.Finish(ctx.Store, cfg.ID, watcher)
		}
		return 1, false, fmt.Errorf("starting exec listener: %w", err)
	}
	defer live.Close()

	pid := uint32(win32.GetProcessId(hProcess))
	startedAt := time.Now().UTC().Format(time.RFC3339Nano)
	_, stateErr := ctx.Store.UpdateContainer(created.ID, func(c *store.ContainerRecord) error {
		if c.State.RunID != created.State.RunID {
			return errors.New("container launch attempt was superseded")
		}
		c.Path = cmd.Exe
		c.Args = append([]string(nil), allArgs...)
		c.Config.WorkingDir = cwd
		c.State = store.ContainerState{
			SupervisorID:    created.State.SupervisorID,
			TopProtocol:     topProtocol,
			StatsProtocol:   statsProtocol,
			CommitProtocol:  commitProtocol,
			ControlProtocol: controlProtocol,
			ExecProtocol:    execProtocol,
			RunID:           created.State.RunID,
			Status:          "running",
			Running:         true,
			Pid:             pid,
			ExitCode:        0,
			StartedAt:       startedAt,
			Launched:        true,
		}
		if opts.attachment != nil {
			c.State.AttachProtocol = attachProtocol
			c.State.WaitProtocol = waitProtocol
		}
		return nil
	})
	if stateErr != nil {
		live.Close()
		process.EndJob(job)
		startPumps()
		pipeWG.Wait()
		if watcher != nil {
			written.Finish(ctx.Store, cfg.ID, watcher)
		}
		return 1, false, stateErr
	}
	if created.RestartCount > 0 {
		ctx.Store.RecordLiveContainerEvent(created, "restart", map[string]string{"automatic": "true"})
	}
	if opts.attachment != nil {
		opts.attachment.mu.Lock()
		opts.attachment.admit = live.attachAllowed
		opts.attachment.mu.Unlock()
		opts.attachment.markStarted(func() error {
			return live.control(ctx, created, controlRequest{Signal: SignalTERM}, detached)
		})
	}
	startPumps()
	if readyEvent != 0 {
		win32.SetEvent(readyEvent)
	}
	if opts.operation != nil {
		opts.operation.Release()
	}

	code, err := waitControlled(ctx, created, &child, detached, controlEvent, live, opts.attachment)
	pipeWG.Wait()

	if code != 0 {
		for i, l := range lazyLayers {
			hex := oci.DigestHex(l.Digest)
			fails, _ := lazy.FailedFetches(ctx.Store, hex, failuresBefore[i])
			for _, line := range fails {
				ctx.Note("quasidock: %s of a layer still being fetched couldn't be: %s", cfg.ID, line)
			}
		}
	}

	if watcher != nil {
		written.Finish(ctx.Store, cfg.ID, watcher)
	}

	return code, true, err
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func pumpPipe(pipeRead win32.HANDLE, dst io.Writer) {
	defer win32.CloseHandle(pipeRead)
	buf := make([]byte, 32*1024)
	for {
		n, err := win32.ReadFile(pipeRead, buf)
		if n > 0 && dst != nil {
			_, _ = dst.Write(buf[:n])
		}
		if err != nil || n == 0 {
			return
		}
	}
}

func openNulRead() win32.HANDLE {
	h, err := win32.CreateFile(
		`\\.\NUL`,
		win32.GENERIC_READ,
		win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE,
		nil,
		win32.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return 0
	}
	return h
}

func lazyLayersOf(ctx *context.Context, digest string) ([]oci.Descriptor, error) {
	manifest, err := ctx.Store.ReadManifest(digest)
	if err != nil {
		return nil, err
	}
	var found []oci.Descriptor
	for _, l := range manifest.Layers {
		hex := oci.DigestHex(l.Digest)
		if hex != "" && lazy.IsLazy(ctx.Store, hex) {
			found = append(found, l)
		}
	}
	return found, nil
}

func WarnUnsupported(cfg oci.AppConfig, sandbox oci.Sandbox, note func(string, ...any)) {
	if cfg.Permissions.Unsupported == nil {
		return
	}
	why := cfg.Permissions.Unsupported.Of(sandbox)
	if why == nil {
		return
	}
	pref := cfg.Permissions.Sandbox
	if pref == "" {
		pref = oci.SandboxLow
	}
	note("warning: %s can't run under the %v sandbox: %s; its image asks for --sandbox=%v", cfg.ID, sandbox, *why, pref)
}

func sharedPath(ctx *context.Context, cfg oci.AppConfig, settings Settings, sandbox oci.Sandbox) []string {
	var dirs []string
	host := false
	if settings.Host != nil {
		host = *settings.Host
	} else if cfg.Permissions.Host != nil {
		host = *cfg.Permissions.Host
	}
	if host {
		if sandbox != oci.SandboxAppContainer {
			hd, err := hostDir(ctx)
			if err == nil {
				dirs = append(dirs, hd)
			}
		} else {
			ctx.Note("note: %s doesn't get quasidock-host in an AppContainer: your programs couldn't read their own files from there", cfg.ID)
		}
	}
	return dirs
}

func hostDir(ctx *context.Context) (string, error) {
	self, err := win32.GetModuleFileName()
	if err != nil {
		return "", err
	}
	src := filepath.Join(filepath.Dir(self), "quasidock-host.exe")
	bytes, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("reading %s, which should be installed next to quasidock.exe: %v", src, err)
	}
	dir := ctx.Store.Path("host")
	_ = os.MkdirAll(dir, 0o755)
	dest := filepath.Join(dir, "quasidock-host.exe")
	had, _ := os.ReadFile(dest)
	if had != nil && string(had) == string(bytes) {
		return dir, nil
	}
	tmp := ctx.Store.TmpPath("host")
	_ = os.WriteFile(tmp, bytes, 0o755)
	err = os.Rename(tmp, dest)
	if err != nil {
		_ = os.Remove(tmp)
		if had == nil {
			return "", err
		}
	}
	return dir, nil
}

func startRelay(ctx *context.Context, id string, job win32.HANDLE, exe string, published *services.Published, forwards *forward.Forwards, appCaps *win32.SECURITY_CAPABILITIES, envBlock []uint16) {
	var handles []win32.HANDLE
	var args []string
	if published != nil {
		for _, s := range published.Sockets {
			handles = append(handles, win32.HANDLE(s))
		}
		args = append(args, published.Args...)
	}
	if forwards != nil {
		handles = append(handles, forwards.RelayIn, forwards.RelayOut)
		args = append(args, forwards.Args()...)
	}
	cmdLine := process.BuildCommandLine(exe, args)
	var ownCaps win32.SECURITY_CAPABILITIES
	if appCaps != nil {
		ownCaps = *appCaps
		ownCaps.CapabilityCount = 1
	}

	relay, err := process.StartIn(job, process.SpawnSpec{
		Exe:           exe,
		CommandLine:   cmdLine,
		EnvBlock:      envBlock,
		Cwd:           filepath.Dir(exe),
		Security:      &ownCaps,
		CreationFlags: win32.DETACHED_PROCESS,
		Inherit:       handles,
	})
	if err != nil {
		if forwards != nil {
			forwards.Close()
		}
		what := "publishes no service"
		if published != nil && forwards != nil {
			what = "publishes no service, and reaches none of the ports it uses,"
		} else if forwards != nil {
			what = "reaches none of the ports it uses"
		}
		ctx.Note("note: the relay of %s's run didn't start, so it %s this run", id, what)
		return
	}
	if forwards != nil {
		if !forwards.Serve(relay) {
			ctx.Note("note: the relay of %s's run couldn't listen for the ports it uses, so it doesn't reach them this run", id)
		}
	}
	_ = win32.CloseHandle(relay)
}

func forwardsOf(targets []services.Target) (*forward.Forwards, error) {
	ports := make([]uint16, len(targets))
	connect := make([]uint16, len(targets))
	for i, t := range targets {
		ports[i] = t.Port
		connect[i] = t.Connect
	}
	return forward.Open(ports, connect)
}

type appContainerSetup struct {
	dataPrepared bool
	deployDir    string
	runtimeDirs  []string
	relayDir     string
	rootsFile    string
	dataDir      string
	localAppData string
	hostCwd      string
	grants       []HostGrant
	network      bool
	own          grants.Own
}

func setUpAppContainer(ctx *context.Context, id string, setup appContainerSetup) (*win32.SECURITY_CAPABILITIES, error) {
	profile, err := appcontainer.Ensure(id)
	if err != nil {
		return nil, err
	}
	tempDir := filepath.Join(setup.localAppData, "Packages", profile.Name, "AC", "Temp")
	_ = os.MkdirAll(tempDir, 0o755)

	type wantedGrant struct {
		path   string
		access acl.Access
		host   bool
		cwd    bool
	}
	var wanted []wantedGrant
	wanted = append(wanted, wantedGrant{path: setup.deployDir, access: acl.AccessReadExecute, host: false})
	for _, dir := range setup.runtimeDirs {
		wanted = append(wanted, wantedGrant{path: dir, access: acl.AccessReadExecute, host: false})
	}
	if setup.relayDir != "" {
		wanted = append(wanted, wantedGrant{path: setup.relayDir, access: acl.AccessReadExecute, host: false})
	}
	if setup.rootsFile != "" {
		wanted = append(wanted, wantedGrant{path: setup.rootsFile, access: acl.AccessReadExecute, host: false})
	}
	if !setup.dataPrepared {
		wanted = append(wanted, wantedGrant{path: setup.dataDir, access: acl.AccessFull, host: false})
	}

	for _, g := range setup.grants {
		p := setup.hostCwd
		isCwd := true
		if g.Path != nil {
			p = *g.Path
			isCwd = false
		}
		access := acl.AccessFull
		if g.ReadOnly {
			access = acl.AccessReadExecute
		}
		if access == acl.AccessFull && !grants.IsWithin(p, setup.dataDir) {
			if why, refused := setup.own.Refusal(p); refused {
				elsewhere := "inside it"
				if setup.own.Contains(p) {
					elsewhere = "elsewhere"
				}
				if g.Path != nil {
					return nil, fmt.Errorf("--sandbox=appcontainer won't let %s write %s: %s, and the app could then change every app's files; grant a directory %s, or read-only access (:ro)", id, p, why, elsewhere)
				}
				ctx.Note("warning: %s is read-only for this run, as %s: run the app from a folder %s to let it write there, or with --sandbox=soft", p, why, elsewhere)
				access = acl.AccessReadExecute
			}
		}
		wanted = append(wanted, wantedGrant{path: p, access: access, host: true, cwd: isCwd})
	}

	var changes *store.Lock
	defer func() {
		if changes != nil {
			changes.Release()
		}
	}()

	for _, w := range wanted {
		if w.host && grants.IsWithin(w.path, setup.dataDir) {
			continue
		}
		if w.host {
			if changes == nil {
				changes, err = ctx.Store.LockGrantChanges()
				if err != nil {
					return nil, err
				}
			}
			_ = ctx.Store.RecordGrant(id, store.Grant{Kind: store.GrantAppContainer, Path: w.path})
			if w.cwd {
				_ = ctx.Store.RecordCwdGrant(id, w.path)
			}
		}
		old := acl.Revoke{Kind: acl.RevokePackageSids}
		if w.host {
			old = acl.Revoke{Kind: acl.RevokeSids, Sids: []win32.PSID{profile.Sid}}
		}
		start := time.Now()
		changed, err := acl.Grant(w.path, profile.Capability, w.access, old)
		if err == nil && changed && (w.host || ctx.Verbose) {
			ctx.Note("granted %s %v access to %s (%d ms)", profile.Name, w.access, w.path, time.Since(start).Milliseconds())
		}
	}

	caps := []win32.SID_AND_ATTRIBUTES{
		{Sid: profile.Capability, Attributes: win32.SE_GROUP_ENABLED},
	}
	if setup.network {
		sids := []string{
			appcontainer.CapabilityInternetClient,
			appcontainer.CapabilityPrivateNetworkClientServer,
		}
		for _, s := range sids {
			sid, err := win32.ConvertStringSidToSid(s)
			if err == nil {
				caps = append(caps, win32.SID_AND_ATTRIBUTES{Sid: sid, Attributes: win32.SE_GROUP_ENABLED})
			}
		}
	}

	sec := &win32.SECURITY_CAPABILITIES{
		AppContainerSid: profile.Sid,
		Capabilities:    &caps[0],
		CapabilityCount: win32.DWORD(len(caps)),
	}
	return sec, nil
}

func exemptLoopback(ctx *context.Context, id string, network, uses bool) (Loopback, error) {
	if !network {
		return LoopbackIsolated, nil
	}
	if v, ok := ctx.Env["QUASIDOCK_LOOPBACK_EXEMPTION"]; ok && v == "0" {
		return LoopbackIsolated, nil
	}
	sid, err := appcontainer.PackageSid(id)
	if err != nil {
		return LoopbackIsolated, err
	}
	changes, err := ctx.Store.LockGrantChanges()
	if err != nil {
		return LoopbackIsolated, err
	}
	defer changes.Release()

	existingGrants, _ := ctx.Store.ReadGrants(id)
	recorded := false
	for _, g := range existingGrants {
		if g.Kind == store.GrantLoopback {
			recorded = true
			break
		}
	}
	if !recorded {
		if isEx := appcontainer.IsExempt(sid); isEx != nil && *isEx {
			return LoopbackExempt, nil
		}
		_ = ctx.Store.RecordGrant(id, store.Grant{Kind: store.GrantLoopback})
	}

	start := time.Now()
	res, err := appcontainer.Exempt(sid)
	if err != nil {
		return LoopbackIsolated, err
	}
	switch res.Status {
	case appcontainer.ExemptAdded:
		if ctx.Verbose {
			ctx.Note("exempted %s from loopback isolation (%d ms)", appcontainer.ProfileName(id), time.Since(start).Milliseconds())
		}
	case appcontainer.ExemptAlready:
	case appcontainer.ExemptRefused:
		if !recorded {
			_ = ctx.Store.ForgetGrant(id, store.Grant{Kind: store.GrantLoopback})
		}
		what := "no other program"
		if uses {
			what = "other programs only on the ports it uses, through quasidock,"
		}
		ctx.Note("note: Windows didn't let quasidock exempt %s from loopback isolation (error %d), so this run reaches %s on this machine", appcontainer.ProfileName(id), res.Refused, what)
		return LoopbackIsolated, nil
	}
	return LoopbackExempt, nil
}

type lowSetup struct {
	dataPrepared bool
	dataDir      string
	hostCwd      string
	grants       []HostGrant
	network      bool
	own          grants.Own
}

func unlabelData(ctx *context.Context, id, dataDir string) error {
	if _, err := os.Stat(dataDir); err != nil {
		return nil
	}
	rid, err := acl.OwnLabel(dataDir)
	if err != nil || rid == nil || *rid > win32.SECURITY_MANDATORY_LOW_RID {
		return nil
	}
	below, _ := acl.BelowMedium()
	if below {
		ctx.Note("warning: %s's data folder stays labelled low integrity, as quasidock runs at low integrity here: programs this run writes there start at low integrity", id)
		return nil
	}
	start := time.Now()
	_ = acl.Unlabel(dataDir)
	ms := time.Since(start).Milliseconds()
	if ms >= 1000 || ctx.Verbose {
		ctx.Note("took the low integrity label off %s (%d ms)", dataDir, ms)
	}
	return nil
}

func setUpLow(ctx *context.Context, id string, setup lowSetup) (win32.HANDLE, error) {
	packageSid, err := appcontainer.PackageSid(id)
	if err == nil && !setup.dataPrepared {
		_, _ = acl.RevokeAccess(setup.dataDir, acl.Revoke{Kind: acl.RevokePackageSids})
	}
	dataStart := time.Now()
	if !setup.dataPrepared {
		if changed, _ := acl.LabelLow(setup.dataDir); changed {
			ms := time.Since(dataStart).Milliseconds()
			if ms >= 1000 || ctx.Verbose {
				ctx.Note("labelled %s low integrity (%d ms)", setup.dataDir, ms)
			}
		}
	}

	var changes *store.Lock
	if len(setup.grants) > 0 {
		changes, err = ctx.Store.LockGrantChanges()
		if err != nil {
			return 0, err
		}
		defer changes.Release()
	}

	for _, g := range setup.grants {
		p := setup.hostCwd
		isCwd := true
		if g.Path != nil {
			p = *g.Path
			isCwd = false
		}
		if grants.IsWithin(p, setup.dataDir) {
			continue
		}
		if packageSid != 0 {
			_, _ = acl.RevokeAccess(p, acl.Revoke{Kind: acl.RevokeSids, Sids: []win32.PSID{packageSid}})
		}
		if g.ReadOnly {
			continue
		}
		if why := LabelRefusal(p, ctx.Env["USERPROFILE"], setup.own); why != "" {
			elsewhere := "inside it"
			if setup.own.Contains(p) {
				elsewhere = "elsewhere"
			}
			if g.Path == nil {
				ctx.Note("warning: %s is read-only for this run, as %s: run the app from a folder %s to let it write there, or with --sandbox=soft", p, why, elsewhere)
				continue
			}
			return 0, fmt.Errorf("--sandbox=low won't label %s low integrity: %s, and every low-integrity process could then write anywhere in it; grant a directory %s, or read-only access (:ro)", p, why, elsewhere)
		}

		label := store.Grant{Kind: store.GrantLow, Path: p}
		if rid, err := acl.OwnLabel(p); err == nil && rid != nil && *rid <= win32.SECURITY_MANDATORY_LOW_RID {
			if granted, _ := ctx.Store.GrantedToOthers(id, label); granted {
				_ = recordLabel(ctx, id, label, isCwd)
			}
			continue
		}
		_ = recordLabel(ctx, id, label, isCwd)
		start := time.Now()
		if changed, _ := acl.LabelLow(p); changed {
			ctx.Note("labelled %s low integrity (%d ms)", p, time.Since(start).Milliseconds())
		}
	}

	if ctx.Verbose {
		ctx.Note("note: low sandbox doesn't keep the app from reading what you can")
		if !setup.network {
			ctx.Note("note: low sandbox doesn't enforce network permissions")
		}
	}
	_ = acl.LetChildrenQueryUs()
	return process.LowIntegrityToken()
}

func recordLabel(ctx *context.Context, id string, label store.Grant, cwd bool) error {
	_ = ctx.Store.RecordGrant(id, label)
	if cwd {
		_ = ctx.Store.RecordCwdGrant(id, label.Path)
	}
	return nil
}

func watchedDirs(sandbox oci.Sandbox, hostGrants []HostGrant, hostCwd, dataDir, userProfile string, own grants.Own) []string {
	var dirs []string
next:
	for _, g := range hostGrants {
		if g.ReadOnly {
			continue
		}
		p := hostCwd
		if g.Path != nil {
			p = *g.Path
		}
		if grants.IsWithin(p, dataDir) {
			continue
		}
		readOnly := false
		switch sandbox {
		case oci.SandboxLow:
			readOnly = LabelRefusal(p, userProfile, own) != ""
		case oci.SandboxAppContainer:
			_, refused := own.Refusal(p)
			readOnly = refused
		case oci.SandboxSoft:
			_, refused := own.Refusal(p)
			if refused {
				readOnly = true
			} else if rid, err := acl.EffectiveLabel(p); err != nil || rid == nil || *rid > win32.SECURITY_MANDATORY_LOW_RID {
				continue
			}
		}
		if readOnly {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			continue
		}
		for _, d := range dirs {
			if grants.IsWithin(p, d) {
				continue next
			}
		}
		dirs = append(dirs, p)
	}
	return dirs
}

func LabelRefusal(path, userProfile string, own grants.Own) string {
	trimmed := strings.TrimRight(path, `\/`)
	vol := filepath.VolumeName(trimmed)
	if vol != "" && strings.EqualFold(trimmed, vol) {
		return "it's the root of a drive"
	}
	if filepath.Dir(trimmed) == trimmed {
		return "it's the root of a drive"
	}
	if userProfile != "" && grants.IsWithin(userProfile, path) {
		if grants.IsWithin(path, userProfile) {
			return "it's your user profile"
		}
		return "your user profile is in it"
	}
	if reason, refused := own.Refusal(path); refused {
		return reason
	}
	return ""
}

type runtimesInfo struct {
	dirs    []oci.DirPlaceholder
	own     []oci.Placeholders
	aliases []string
}

func runtimeDirs(cfg oci.AppConfig, dirs []string, dataDir, cacheDir string, rootsFile *string, uses []oci.UsedPort) runtimesInfo {
	named := make([]oci.DirPlaceholder, 0, len(dirs))
	own := make([]oci.Placeholders, 0, len(dirs))
	aliasesList := make([]string, 0, len(dirs))
	for _, runtime := range oci.RuntimeDirectories(cfg.From, dirs) {
		alias, dir := runtime.Alias, runtime.Path
		named = append(named, oci.DirPlaceholder{Alias: alias, Path: dir})
		rootsStr := ""
		if rootsFile != nil {
			rootsStr = *rootsFile
		}
		own = append(own, oci.Placeholders{
			App:   dir,
			Data:  dataDir,
			Cache: cacheDir,
			Roots: rootsStr,
			Uses:  uses,
		})
		aliasesList = append(aliasesList, alias)
	}
	return runtimesInfo{dirs: named, own: own, aliases: aliasesList}
}

func usesCache(cfg oci.AppConfig) bool {
	if oci.UsesCache(cfg.Path, cfg.Env) {
		return true
	}
	for _, r := range cfg.From {
		if oci.UsesCache(r.Path, r.Env) {
			return true
		}
	}
	return false
}

func usesRoots(cfg oci.AppConfig) bool {
	if oci.UsesRoots(cfg.Env, cfg.Entrypoint) || oci.UsesRoots(nil, cfg.Cmd) {
		return true
	}
	for _, r := range cfg.From {
		if oci.UsesRoots(r.Env, nil) {
			return true
		}
	}
	return false
}

type Command struct {
	Exe   string
	Args  []string
	Batch bool
}

func newCommand(exe string, args []string) Command {
	ext := filepath.Ext(exe)
	return Command{
		Exe:   exe,
		Args:  args,
		Batch: strings.EqualFold(ext, ".bat") || strings.EqualFold(ext, ".cmd"),
	}
}

func commandForRun(p oci.Placeholders, cfg oci.AppConfig, opts Options, path []string, systemRoot string) (Command, error) {
	var requested *string
	args := opts.Args
	explicitEntrypoint := opts.Entrypoint != nil
	if opts.Entrypoint != nil && *opts.Entrypoint != "" {
		requested = opts.Entrypoint
	} else if len(args) > 0 {
		requested = &args[0]
		args = args[1:]
	} else if opts.Entrypoint != nil {
		return Command{}, errors.New("empty --entrypoint needs a command after IMAGE")
	}
	cmd, err := resolveCommand(p, cfg, requested, explicitEntrypoint, path, systemRoot)
	if err != nil {
		return Command{}, err
	}
	cmd.Args = append(cmd.Args, args...)
	return cmd, nil
}

func resolveCommand(placeholders oci.Placeholders, config oci.AppConfig, requested *string, explicitEntrypoint bool, appPath []string, systemRoot string) (Command, error) {
	if requested == nil {
		if config.FormatVersion == 4 {
			command := config.Entrypoint
			args := config.Cmd
			if len(command) == 0 {
				command, args = config.Cmd, nil
			}
			if len(command) == 0 {
				return Command{}, errors.New("image has no command")
			}
			name := placeholders.Expand(command[0])
			cwd := imageWorkdir(placeholders, config.Workdir)
			search := append([]string{placeholders.App}, appPath...)
			exe, ok := findCommand(cwd, name, search, systemRoot)
			if !ok {
				return Command{}, fmt.Errorf("image command %q is not an executable in the app, data, PATH or System32", command[0])
			}
			arguments := append(expandAll(placeholders, command[1:]), expandAll(placeholders, args)...)
			return newCommand(exe, arguments), nil
		}
		if len(config.Entrypoint) == 0 {
			return Command{}, errors.New("image has no entrypoint")
		}
		cmdPath := placeholders.CommandPath(config.Entrypoint[0])
		args := expandAll(placeholders, config.Entrypoint[1:])
		args = append(args, expandAll(placeholders, config.Cmd)...)
		return newCommand(cmdPath, args), nil
	}
	name := *requested
	if config.FormatVersion == 4 && strings.EqualFold(name, config.ID) && len(config.Entrypoint) == 0 && len(config.Cmd) > 0 {
		config.Entrypoint = []string{config.Cmd[0]}
		config.Cmd = nil
		return resolveCommand(placeholders, config, nil, false, appPath, systemRoot)
	}
	if len(config.Entrypoint) > 0 && (strings.EqualFold(name, config.ID) || (!explicitEntrypoint && len(config.Entrypoint) > 1 && strings.EqualFold(name, oci.CommandStem(config.Entrypoint[0])))) {
		if config.FormatVersion == 4 {
			config.Cmd = nil
			return resolveCommand(placeholders, config, nil, false, appPath, systemRoot)
		}
		cmdPath := placeholders.CommandPath(config.Entrypoint[0])
		args := expandAll(placeholders, config.Entrypoint[1:])
		return newCommand(cmdPath, args), nil
	}
	if oci.IsPlaceholderPath(name) {
		name = placeholders.Expand(name)
	}
	var resolved string
	var ok bool
	if config.Workdir != "" {
		searchPath := append([]string{placeholders.App}, appPath...)
		resolved, ok = findCommand(imageWorkdir(placeholders, config.Workdir), name, searchPath, systemRoot)
	}
	if !ok {
		resolved, ok = findCommand(placeholders.App, name, appPath, systemRoot)
	}
	if !ok {
		if strings.ContainsAny(name, `/\`) && !filepath.IsAbs(name) {
			return Command{}, fmt.Errorf("command %q is not a file in the app; for a program of yours, give its absolute path (e.g. --entrypoint=%%CD%%\\%s)", name, filepath.Base(name))
		}
		return Command{}, fmt.Errorf("command %q is not an executable or batch file in the app or in System32", name)
	}
	return newCommand(resolved, nil), nil
}

func imageWorkdir(p oci.Placeholders, workdir string) string {
	if oci.IsPlaceholderPath(workdir) {
		return p.Expand(workdir)
	}
	return filepath.Join(p.App, filepath.FromSlash(workdir))
}

func expandAll(p oci.Placeholders, templates []string) []string {
	out := make([]string, len(templates))
	for i, t := range templates {
		out[i] = p.Expand(t)
	}
	return out
}

func findCommand(deployDir, name string, appPath []string, systemRoot string) (string, bool) {
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			return name, true
		}
		return "", false
	}
	if strings.ContainsAny(name, `/\`) {
		p := filepath.Join(deployDir, name)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
		return "", false
	}
	dirs := []string{deployDir}
	dirs = append(dirs, appPath...)
	dirs = append(dirs, filepath.Join(systemRoot, "System32"))
	exts := []string{""}
	if filepath.Ext(name) == "" {
		exts = []string{".exe", ".com", ".cmd", ".bat"}
	}
	for _, dir := range dirs {
		for _, ext := range exts {
			p := filepath.Join(dir, name+ext)
			if _, err := os.Stat(p); err == nil {
				return p, true
			}
		}
	}
	return "", false
}
