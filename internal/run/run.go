package run

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/forward"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/lazy"
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
	Ephemeral    *bool
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
	ID         string
	Args       []string
	Entrypoint *string
	Pull       string
	Image      *store.Ref
	Settings   Settings
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
	selected := opts.Image
	if selected == nil {
		selected, err = ctx.Store.ResolveLocalImage(ctx.Env["QUASIDOCK_REGISTRY"], opts.ID)
		if err != nil {
			return 1, err
		}
	} else {
		selected, err = ctx.Store.ResolveLocalImage("", selected.Manifest)
		if err != nil {
			return 1, err
		}
	}
	opts.ID = selected.ID

	lazyLayers, err := lazyLayersOf(ctx, selected.Manifest)
	if err != nil {
		storeLock.Release()
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
		return 1, err
	}
	defer img.Release()
	cfg := img.Config
	appLease, imageLease, err := ctx.Store.BeginImageRun(*selected)
	if err != nil {
		return 1, err
	}
	defer imageLease.Release()
	runs := &grants.Session{ID: cfg.ID, LockPath: ctx.Store.RunsLockPath(cfg.ID), Lock: appLease}
	defer runs.End(ctx.Store)

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
			storeLock.Release()
			return 1, ctx.Fail("invalid filesystem permission %q", spec)
		}
		switch g.Where.Type {
		case oci.FsGrantCwd:
			cwdGranted = true
			hostGrants = append(hostGrants, HostGrant{Path: nil, ReadOnly: g.ReadOnly})
		case oci.FsGrantPath:
			p := g.Where.Path
			hostGrants = append(hostGrants, HostGrant{Path: &p, ReadOnly: g.ReadOnly})
		case oci.FsGrantFolder:
			// Named user-folder grants are ignored without folder junction linking.
		default:
			ctx.Note("warning: %s asks for the filesystem permission %q, which this quasidock doesn't know, and leaves out; a newer quasidock may", cfg.ID, spec)
		}
	}

	hostCwd, err := os.Getwd()
	if err != nil {
		storeLock.Release()
		return 1, err
	}

	var runDir *store.RunDir
	if settings.Ephemeral != nil && *settings.Ephemeral {
		runDir, err = ctx.Store.MakeRunDir()
		if err != nil {
			storeLock.Release()
			return 1, err
		}
		defer runDir.Delete()
	}

	if sandbox == oci.SandboxSoft && runDir == nil {
		_ = unlabelData(ctx, cfg.ID)
	}

	storeLock.Release()

	dataDir := ctx.Store.Path("data", cfg.ID)
	if runDir != nil {
		dataDir = runDir.Path
	}
	profile := environment.NewProfile(dataDir)

	quickAccess := filepath.Join(profile.Roaming, "Microsoft", "Windows", "Recent", "AutomaticDestinations")
	_ = os.MkdirAll(quickAccess, 0o755)
	_ = os.MkdirAll(profile.Temp, 0o755)

	cwd := profile.Home
	if cwdGranted {
		cwd = hostCwd
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
	servicePorts, err := services.Resolve(ctx.Store, cfg, used, settings.Ports, sandbox)
	if err != nil {
		return 1, err
	}

	reach := LoopbackOpen
	if sandbox == oci.SandboxAppContainer {
		reach, err = exemptLoopback(ctx, cfg.ID, network, len(targets) > 0)
		if err != nil {
			return 1, err
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
		rCfg := cfg.Runtimes[alias]
		pDirs, _ := environment.PathDirs(r, rCfg.Path)
		appPath = append(appPath, pDirs...)
	}

	cmd, err := commandForRun(placeholders, cfg, opts, appPath, systemRoot)
	if err != nil {
		return 1, err
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
				return 1, ctx.Fail("%s is a batch file, and cmd.exe can't pass it an argument with a line break or a NUL character", cmd.Exe)
			}
			return 1, ctx.Fail("%s can't be run as a batch file: %v", cmd.Exe, err)
		}
	} else {
		commandLine = process.BuildCommandLine(exe, allArgs)
	}

	var appEnv []environment.Var
	for i, r := range rt.own {
		alias := rt.aliases[i]
		rCfg := cfg.Runtimes[alias]
		_ = environment.Expand(&appEnv, r, rCfg.Env)
	}
	_ = environment.Expand(&appEnv, placeholders, cfg.Env)

	sharedPathDirs := sharedPath(ctx, cfg, settings, sandbox)
	envVars, err := environment.Build(ctx.Env, environment.Spec{
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
		return 1, err
	}

	if ctx.Verbose {
		ctx.Note("app      %s %s (%s)", cfg.ID, cfg.Version, img.Ref.Manifest)
		for alias, r := range cfg.Runtimes {
			ctx.Note("runtime  %s: %s %s", alias, r.ID, r.Version)
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
			dataDir: dataDir,
			hostCwd: hostCwd,
			grants:  hostGrants,
			network: network,
			own:     own,
		})
		if err != nil {
			return 1, err
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
			return 1, err
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
	keysSession, err := hkcu.Enter(ctx.Store, cfg.ID, cfg.Registry, dataDir, hkcuAccess, runDir == nil)
	if err != nil {
		return 1, err
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
		return 1, err
	}

	job, err := process.NewJob()
	if err != nil {
		return 1, err
	}
	defer win32.CloseHandle(job)

	if relayExe != "" {
		startRelay(ctx, cfg.ID, job, relayExe, published, forwards, security, envBlock)
	}

	watcherDirs := watchedDirs(sandbox, hostGrants, hostCwd, dataDir, ctx.Env["USERPROFILE"], own)
	watcher := written.Start(watcherDirs)

	hProcess, err := process.StartIn(job, process.SpawnSpec{
		Exe:         exe,
		CommandLine: commandLine,
		EnvBlock:    envBlock,
		Cwd:         cwd,
		Security:    security,
		Token:       token,
	})
	if err != nil {
		if watcher != nil {
			_, _ = watcher.Stop()
		}
		return 1, err
	}

	child := process.Child{Job: job, Process: hProcess}
	code, err := child.Wait()

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

	return code, err
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
		return "", ctx.Fail("reading %s, which should be installed next to quasidock.exe: %v", src, err)
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
	wanted = append(wanted, wantedGrant{path: setup.dataDir, access: acl.AccessFull, host: false})

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
					return nil, ctx.Fail("--sandbox=appcontainer won't let %s write %s: %s, and the app could then change every app's files; grant a directory %s, or read-only access (:ro)", id, p, why, elsewhere)
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
		Capabilities:    (*win32.SID_AND_ATTRIBUTES)(unsafe.Pointer(&caps[0])),
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
	dataDir string
	hostCwd string
	grants  []HostGrant
	network bool
	own     grants.Own
}

func unlabelData(ctx *context.Context, id string) error {
	dataDir := ctx.Store.Path("data", id)
	if _, err := os.Stat(dataDir); err != nil {
		return nil
	}
	rid, err := acl.OwnLabel(dataDir)
	if err != nil || rid == nil || *rid > win32.SECURITY_MANDATORY_LOW_RID {
		return nil
	}
	below, _ := acl.BelowMedium()
	var lock *store.Lock
	if !below {
		lock, _ = store.TryLock(ctx.Store.RunsLockPath(id), true)
		if lock == nil {
			ctx.Note("warning: %s's data folder stays labelled low integrity, as another run of it is going: programs this run writes there start at low integrity", id)
			return nil
		}
		defer lock.Release()
	} else {
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
	if err == nil {
		_, _ = acl.RevokeAccess(setup.dataDir, acl.Revoke{Kind: acl.RevokePackageSids})
	}
	dataStart := time.Now()
	if changed, _ := acl.LabelLow(setup.dataDir); changed {
		ms := time.Since(dataStart).Milliseconds()
		if ms >= 1000 || ctx.Verbose {
			ctx.Note("labelled %s low integrity (%d ms)", setup.dataDir, ms)
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
			return 0, ctx.Fail("--sandbox=low won't label %s low integrity: %s, and every low-integrity process could then write anywhere in it; grant a directory %s, or read-only access (:ro)", p, why, elsewhere)
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
	idx := 0
	for alias := range cfg.Runtimes {
		if idx < len(dirs) {
			dir := dirs[idx]
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
			idx++
		}
	}
	return runtimesInfo{dirs: named, own: own, aliases: aliasesList}
}

func usesCache(cfg oci.AppConfig) bool {
	if oci.UsesCache(cfg.Path, cfg.Env) {
		return true
	}
	for _, r := range cfg.Runtimes {
		if oci.UsesCache(r.Path, r.Env) {
			return true
		}
	}
	return false
}

func usesRoots(cfg oci.AppConfig) bool {
	if oci.UsesRoots(cfg.Env, cfg.Args) {
		return true
	}
	for _, e := range cfg.Exports {
		if oci.UsesRoots(nil, e.Args) {
			return true
		}
	}
	for _, r := range cfg.Runtimes {
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
	if opts.Entrypoint != nil && *opts.Entrypoint != "" {
		requested = opts.Entrypoint
	} else if len(args) > 0 {
		requested = &args[0]
		args = args[1:]
	} else if opts.Entrypoint != nil {
		return Command{}, errors.New("empty --entrypoint needs a command after IMAGE")
	}
	cmd, err := resolveCommand(p, cfg, requested, path, systemRoot)
	if err != nil {
		return Command{}, err
	}
	cmd.Args = append(cmd.Args, args...)
	return cmd, nil
}

func resolveCommand(placeholders oci.Placeholders, config oci.AppConfig, requested *string, appPath []string, systemRoot string) (Command, error) {
	if requested == nil {
		cmdPath := placeholders.CommandPath(config.Command)
		args := expandAll(placeholders, config.Args)
		return newCommand(cmdPath, args), nil
	}
	name := *requested
	for k, e := range config.Exports {
		if strings.EqualFold(k, name) {
			cmdPath := placeholders.CommandPath(e.Command)
			args := expandAll(placeholders, e.Args)
			return newCommand(cmdPath, args), nil
		}
	}
	if oci.IsPlaceholderPath(name) {
		name = placeholders.Expand(name)
	}
	resolved, ok := findCommand(placeholders.App, name, appPath, systemRoot)
	if !ok {
		if strings.ContainsAny(name, `/\`) && !filepath.IsAbs(name) {
			return Command{}, fmt.Errorf("command %q is not a file in the app; for a program of yours, give its absolute path (e.g. --entrypoint=%%CD%%\\%s)", name, filepath.Base(name))
		}
		return Command{}, fmt.Errorf("command %q is not an export of the app, or an executable or batch file in it or in System32", name)
	}
	return newCommand(resolved, nil), nil
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
