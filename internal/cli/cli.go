package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/quasidock/quasidock/internal/builder"
	"github.com/quasidock/quasidock/internal/catalog"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/credentials"
	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/lazy"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/prompt"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/services"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/trust"
	"github.com/quasidock/quasidock/internal/version"
)

const usage = `usage: quasidock [--verbose] <command> [options]

Docker-style container commands:
  container create | create [options] IMAGE [COMMAND] [ARG...]
                                       create a container without starting it; print its ID
  container ls | ps [options]
                                       list running containers (or all with -a)
  container stats | stats [options] [CONTAINER...]
                                       stream CPU, private memory, I/O and process counts
  container inspect | inspect [-f TEMPLATE] NAME|ID [NAME|ID...]
                                       inspect containers or images (JSON by default)
  container diff | diff CONTAINER      list additions to private data; exclude mounts
  container cp | cp SOURCE DESTINATION copy files/folders between container data and host
                                       use CONTAINER:PATH; - streams a tar archive
  container commit | commit [options] CONTAINER [REPOSITORY[:TAG]]
                                       capture private data and configuration as an image
  container logs | logs [-f|--follow] [-n|--tail LINES] CONTAINER
                                       fetch the logs of a container
  container exec | exec [options] CONTAINER COMMAND [ARG...]
                                       run another command in a running container
  container attach | attach [--no-stdin] [--sig-proxy=false] [--detach-keys KEYS] CONTAINER
                                       attach to a background container's live streams
  container start | start [-a|--attach] [-i|--interactive] [--sig-proxy=false] [--detach-keys KEYS] CONTAINER [CONTAINER...]
                                       start containers; -a/-i attaches to one container
  container restart | restart [-t|--time SECONDS] CONTAINER [CONTAINER...]
                                       restart containers (default grace: 10 seconds)
  container pause | pause CONTAINER [CONTAINER...]
                                       freeze all processes in running containers
  container unpause | unpause CONTAINER [CONTAINER...]
                                       resume paused containers
  container kill | kill [-s|--signal KILL|TERM] CONTAINER [CONTAINER...]
                                       kill a process tree, or request graceful shutdown
  container stop | stop [-t|--time SECONDS] CONTAINER [CONTAINER...]
                                       stop one or more running containers
  container rm | rm [-f|--force] CONTAINER [CONTAINER...]
                                       remove one or more containers
  container prune                      remove all stopped containers
  run [options] IMAGE [COMMAND] [ARG...] run an image in a new container
  system events | events [options]     follow container events or replay retained history

Docker-style image commands:
  image ls | images [-q|--quiet]       list cached image references
  image inspect [-f TEMPLATE] IMAGE [IMAGE...]
                                       inspect local images (JSON by default)
  image pull | pull [--lazy] <image>   cache an image, without prompting
  image tag | tag SOURCE TARGET       name a cached image; an omitted tag is latest
  image push | push [--sources] IMAGE  publish the exact local image
  image save | save [-o|--output FILE] IMAGE [IMAGE...]
                                       save local images and all their aliases as OCI tar
  image load | load [-i|--input FILE] [-q|--quiet] [--force]
                                       load OCI tar (or gzip); --force replaces names
  image rm | rmi IMAGE [IMAGE...]      untag a reference or remove a manifest ID

Persistent storage:
  volume create NAME                 create a reusable named volume
  volume ls [-q]                     list local volumes
  volume inspect [-f TEMPLATE] NAME [NAME...]
  volume rm NAME [NAME...]            remove unreferenced named volumes
  volume prune [--dry-run]            collect unreferenced named volumes

Quasidock extensions:
  convert [CONTEXT] [-f DOCKERFILE] [-o RECIPE] [--target NAME] [--base IMAGE] [--stage-base NAME_OR_INDEX=IMAGE]...
                                       convert a supported Linux Dockerfile to a recipe
  build [--rebuild] [--clean] [--keep-build-dir] [<recipe.json> | <dir>]
                                       build a recipe and cache its image
  image info IMAGE                    describe an image in a registry
  image files IMAGE                   list a remote eStargz image's files
  image cat IMAGE PATH                read a remote eStargz image's file
  push --catalog <app-id>...           publish the default registry's catalog
  search [<words>...]                 search that catalog
  login [--username=USER] [--password-stdin] REGISTRY
  logout REGISTRY
  trust [add REGISTRY/PATH KEY | rm REGISTRY/PATH [FINGERPRINT]]
  signature payload IMAGE
  signature push IMAGE PAYLOAD SIGNATURE
  signature check IMAGE
  grants [APP-ID] | grants --take-back PATH
  prune [--dry-run] [--downloads] [--data]
                                       collect stopped containers, unreferenced images,
                                       and unused files (--data removes orphan data)
  repair                              settle obsolete host grants
  version                             print quasidock's version

ps/container ls options:
  -a, --all                          include created and exited containers
  -q, --quiet                        print container IDs only
  -f, --filter KEY=VALUE              id, name (regexp), status, exited, before, or since
  -n, --last N                       newest N matches, including all states (default: -1)
  -s, --size                         private-data file bytes, excluding mounted storage
  --no-trunc                         show full IDs and recorded commands

stats/container stats options:
  -a, --all                          include created and exited containers
  --no-stream                        sample once and exit (after one second)
  --no-trunc                         show full container IDs
  --format table|json|TEMPLATE        table, JSON Lines, or a Go template

events/system events options:
  --since TIME                       replay retained events since TIME, then follow
  --until TIME                       stop at TIME (past values query history only)
  -f, --filter KEY=VALUE              event, container, image, or type=container
  --format json|TEMPLATE              JSON Lines or a Go template (default: text)

exec options (before CONTAINER):
  -d, --detach                        return after launch; discard command output
  -i, --interactive                   forward STDIN and EOF
  -t, --tty                           pipe-backed TTY preference (no ConPTY)
  -e, --env NAME[=VALUE]               override, inherit, or unset an environment variable
  -w, --workdir DIR                   existing directory (default: container's initial cwd)
  --verbose                           print runtime diagnostics

run/create options (before IMAGE; create rejects -d/--detach):
  --name NAME                         assign a name to the container
  --rm                                automatically remove the container when it exits
  -d, --detach                        run container in background and print container ID
  -i, --interactive                   keep STDIN open (attach console STDIN in foreground)
  -t, --tty                           allocate a pseudo-TTY flag on container config
  --pull=missing|always|never          default: missing; failed pulls never use stale images
  --entrypoint=COMMAND                override the entrypoint and clear the default cmd
  -e, --env NAME[=VALUE]               set or inherit an environment variable
  -v, --volume SOURCE:TARGET[:rw]      mount at ${data} or a data subpath
  --mount type=volume|bind,source=SOURCE,target=TARGET
                                       mount a writable local NTFS directory
  --sandbox=soft|low|appcontainer      choose the Windows sandbox (default: image, then low)
  --filesystem=<location>[:ro]         grant cwd or an absolute host path
  --nofilesystem=LOCATION             remove an image's filesystem grant
  --share=network|host                grant network access or quasidock-host
  --unshare=network|host              remove that grant
  --port=SERVICE=PORT                 override a declared service's localhost port
  --verbose                           print runtime diagnostics

Images use registry/repository[:tag][@sha256:...], or app-id[:tag][@digest].
QUASIDOCK_REGISTRY changes the default registry (ghcr.io/quasidock/quasidock).
A bare cached app ID selects its last successful build/pull; full sha256 IDs
select local manifests. Explicit names and tags select exactly that reference.
Image IDs are manifest digests. images -q prints full IDs; inspect is local-only.
save writes tar to stdout by default; load reads stdin by default (use -o/-i on Windows).
save/load are offline; incomplete lazy images must be fully pulled before saving.
load preserves existing app defaults; conflicting names require --force.
Each run or create allocates isolated data under containers/<id>/data.
create saves configuration without launching; start defaults to background.
ps filters repeat (same key: OR; different keys: AND); status includes all states.
ps --last values <= 0 leave results unlimited with normal visibility rules.
ps --size includes copied image seed files, excludes logs, and has no virtual total.
diff lists A/C/D /PATH relative to image data; mounts are excluded.
commit supports --pause=false, -a/--author, -m/--message and -c/--change.
--change supports CMD/ENTRYPOINT JSON arrays, ENV, WORKDIR and LABEL.
attach and start -a/-i use pipes; Ctrl+C forwards TERM, --sig-proxy=false detaches.
Interactive -t console sessions detach with Ctrl+P, Ctrl+Q (override --detach-keys).
Use an explicit command after IMAGE to replace the image's entrypoint and default cmd.
Mount targets are ${data} or ${data}/PATH. Named volumes survive container removal.
Read-only mounts, overlapping targets, and -p are not supported yet.

Registry credentials use Windows Credential Manager, QUASIDOCK_REGISTRY_USERNAME /
QUASIDOCK_REGISTRY_PASSWORD (for the default registry), or Docker's saved login.
Data lives in QUASIDOCK_HOME, or %LOCALAPPDATA%\quasidock.
`

func isFlag(arg, name string) bool {
	return arg == "--"+name
}

type Option struct {
	Name  string
	Value *string
}

func readOption(args []string, i *int, flags []string) (Option, error) {
	arg := args[*i]
	eq := strings.IndexByte(arg, '=')
	var name string
	if eq != -1 {
		name = arg[2:eq]
		val := arg[eq+1:]
		return Option{Name: name, Value: &val}, nil
	}
	name = arg[2:]
	for _, f := range flags {
		if name == f {
			return Option{Name: name, Value: nil}, nil
		}
	}
	*i++
	if *i == len(args) {
		return Option{}, fmt.Errorf("--%s needs a value", name)
	}
	val := args[*i]
	return Option{Name: name, Value: &val}, nil
}

type settingsParser struct {
	command      string
	settings     run.Settings
	filesystem   []string
	nofilesystem []string
	env          []string
	ports        []string
}

func (p *settingsParser) apply(opt Option) error {
	name := opt.Name
	if name == "ephemeral" {
		return errors.New("--ephemeral was removed; use --rm")
	}
	if opt.Value == nil {
		return fmt.Errorf("--%s needs a value", name)
	}
	val := *opt.Value
	switch name {
	case "sandbox":
		sb := oci.Sandbox(val)
		if sb != oci.SandboxSoft && sb != oci.SandboxLow && sb != oci.SandboxAppContainer {
			return errors.New("--sandbox must be soft, low or appcontainer")
		}
		p.settings.Sandbox = &sb
	case "filesystem", "nofilesystem":
		g := oci.ParseFsGrant(val)
		if g == nil || g.Where.Type == oci.FsGrantUnknown {
			return fmt.Errorf("--%s must be cwd or an absolute path; got %q", name, val)
		}
		if name == "filesystem" {
			p.filesystem = append(p.filesystem, val)
		} else {
			if strings.HasSuffix(val, ":ro") || strings.HasSuffix(val, ":rw") {
				return errors.New("--nofilesystem takes away all access to a location; it takes no :ro or :rw")
			}
			p.nofilesystem = append(p.nofilesystem, val)
		}
	case "share", "unshare":
		on := (name == "share")
		switch val {
		case "network":
			p.settings.Network = &on
		case "host":
			p.settings.Host = &on
		default:
			return fmt.Errorf("--%s takes network or host; got %q", name, val)
		}
	case "env":
		if val == "" || strings.HasPrefix(val, "=") || strings.ContainsRune(val, '\x00') {
			return fmt.Errorf("--env expects NAME[=VALUE], got %q", val)
		}
		p.env = append(p.env, val)
	case "port":
		if _, _, ok := services.ParsePort(val); !ok {
			return fmt.Errorf("--port expects <service>=<port>, a port from 1 to 65535; got %q", val)
		}
		p.ports = append(p.ports, val)
	default:
		return fmt.Errorf("unknown %s option --%s", p.command, name)
	}
	return nil
}

func (p *settingsParser) finish() run.Settings {
	s := p.settings
	s.Filesystem = p.filesystem
	s.NoFilesystem = p.nofilesystem
	s.Env = p.env
	s.Ports = p.ports
	return s
}

func parseRunOptions(ctx *context.Context, args []string) (run.Options, error) {
	return parseContainerOptions(ctx, args, "run")
}

func parseCreateOptions(ctx *context.Context, args []string) (run.Options, error) {
	return parseContainerOptions(ctx, args, "create")
}

func parseContainerOptions(ctx *context.Context, args []string, command string) (run.Options, error) {
	opts := run.Options{Pull: "missing"}
	args = append([]string(nil), args...)
	parser := settingsParser{command: command}

	flags := []string{"rm", "detach", "interactive", "tty", "verbose", "ephemeral"}
	i := 0
	for i < len(args) {
		if args[i] == "--" {
			i++
			break
		}
		if args[i] == "-v" {
			args[i] = "--volume"
		} else if strings.HasPrefix(args[i], "-v=") {
			args[i] = "--volume=" + args[i][3:]
		} else if strings.HasPrefix(args[i], "-v") {
			args[i] = "--volume=" + args[i][2:]
		}
		if args[i] == "-e" {
			args[i] = "--env"
		} else if strings.HasPrefix(args[i], "-e=") {
			args[i] = "--env=" + args[i][3:]
		} else if strings.HasPrefix(args[i], "-e") {
			args[i] = "--env=" + args[i][2:]
		}
		if !strings.HasPrefix(args[i], "--") {
			if strings.HasPrefix(args[i], "-") {
				if len(args[i]) > 1 {
					allShort := true
					for _, ch := range args[i][1:] {
						switch ch {
						case 'd':
							if command == "create" {
								return opts, errors.New("create does not support --detach; use start to launch the container")
							}
							opts.Detach = true
						case 'i':
							opts.Interactive = true
						case 't':
							opts.TTY = true
						default:
							allShort = false
						}
						if !allShort {
							break
						}
					}
					if allShort {
						i++
						continue
					}
				}
				if strings.ContainsRune(args[i], 'v') {
					return opts, fmt.Errorf("unsupported %s option %q; pass -v SOURCE:TARGET separately from boolean flag clusters", command, args[i])
				}
				return opts, fmt.Errorf("unknown %s option %q", command, args[i])
			}
			break
		}
		opt, err := readOption(args, &i, flags)
		if err != nil {
			return opts, err
		}
		switch opt.Name {
		case "mount", "volume":
			if opt.Value == nil {
				return opts, errors.New("mount needs a value")
			}
			m, err := mounts.Parse(*opt.Value, opt.Name == "volume")
			if err != nil {
				return opts, err
			}
			opts.Mounts = append(opts.Mounts, m)
		case "ephemeral":
			return opts, errors.New("--ephemeral was removed; use --rm")
		case "rm":
			if opt.Value != nil {
				return opts, errors.New("--rm takes no value")
			}
			opts.AutoRemove = true
		case "detach":
			if command == "create" {
				return opts, errors.New("create does not support --detach; use start to launch the container")
			}
			if opt.Value != nil {
				return opts, errors.New("--detach takes no value")
			}
			opts.Detach = true
		case "interactive":
			if opt.Value != nil {
				return opts, errors.New("--interactive takes no value")
			}
			opts.Interactive = true
		case "tty":
			if opt.Value != nil {
				return opts, errors.New("--tty takes no value")
			}
			opts.TTY = true
		case "verbose":
			if opt.Value != nil {
				return opts, errors.New("--verbose takes no value")
			}
			ctx.Verbose = true
			if ctx.Store != nil {
				ctx.Store.Verbose = true
			}
		case "name":
			if opt.Value == nil || *opt.Value == "" {
				return opts, errors.New("--name needs a value")
			}
			opts.Name = *opt.Value
		case "entrypoint":
			if opt.Value != nil {
				opts.Entrypoint = opt.Value
			}
		case "command":
			return opts, errors.New("--command was removed; use --entrypoint or a command after IMAGE")
		case "pull":
			if opt.Value == nil || (*opt.Value != "missing" && *opt.Value != "always" && *opt.Value != "never") {
				return opts, errors.New("--pull must be missing, always or never")
			}
			opts.Pull = *opt.Value
		default:
			if err := parser.apply(opt); err != nil {
				return opts, err
			}
		}
		i++
	}
	if i == len(args) {
		return opts, fmt.Errorf("usage: quasidock %s [OPTIONS] IMAGE [COMMAND] [ARG...]", command)
	}

	opts.ID = args[i]
	opts.Args = args[i+1:]
	opts.Settings = parser.finish()
	if err := mounts.ValidateTargets(opts.Mounts); err != nil {
		return opts, err
	}
	return opts, nil
}

type pullOptions struct {
	Image string
	Lazy  bool
}

func parsePullOptions(args []string) (pullOptions, error) {
	var opts pullOptions
	var image *string
	for _, arg := range args {
		if isFlag(arg, "yes") {
			// Accepted as a no-op for compatibility; pull is non-interactive by default.
		} else if isFlag(arg, "lazy") {
			opts.Lazy = true
		} else if image == nil && !strings.HasPrefix(arg, "-") {
			val := arg
			image = &val
		} else {
			return opts, errors.New("usage error")
		}
	}
	if image == nil {
		return opts, errors.New("usage error")
	}
	opts.Image = *image
	return opts, nil
}

type loginOptions struct {
	Registry      string
	Username      *string
	PasswordStdin bool
}

func parseLoginOptions(args []string) (loginOptions, error) {
	var opts loginOptions
	var reg *string
	flags := []string{"password-stdin"}
	i := 0
	for i < len(args) {
		if !strings.HasPrefix(args[i], "--") {
			if reg != nil {
				return opts, errors.New("usage error")
			}
			val := args[i]
			reg = &val
			i++
			continue
		}
		opt, err := readOption(args, &i, flags)
		if err != nil {
			return opts, err
		}
		if opt.Name == "password-stdin" && opt.Value == nil {
			opts.PasswordStdin = true
		} else if opt.Name == "username" && opt.Value != nil {
			opts.Username = opt.Value
		} else {
			return opts, fmt.Errorf("unknown login option --%s", opt.Name)
		}
		i++
	}
	if reg == nil {
		return opts, errors.New("usage error")
	}
	opts.Registry = *reg
	return opts, nil
}

func doLogin(ctx *context.Context, opts loginOptions) error {
	host, err := credentials.NormalizeHost(opts.Registry)
	if err != nil {
		return fmt.Errorf("%q isn't a registry, like ghcr.io or localhost:5000", opts.Registry)
	}

	user := ""
	if opts.Username != nil && *opts.Username != "" {
		user = *opts.Username
	} else {
		u, err := prompt.ConsoleLine("Username: ", prompt.EchoInput)
		if err != nil || u == nil {
			return errors.New("login cancelled")
		}
		user = *u
	}

	secret := ""
	if opts.PasswordStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading password from stdin: %w", err)
		}
		secret = strings.TrimRight(string(data), "\r\n")
	} else {
		pw, err := prompt.ConsoleLine("Password: ", prompt.NoEcho)
		if err != nil || pw == nil {
			return errors.New("login cancelled")
		}
		secret = *pw
	}

	// Verify against registry
	cred := credentials.Credential{
		User:   user,
		Secret: secret,
		Source: credentials.SourceCredentialManager,
	}
	check, err := registry.CheckLogin(host, cred, ctx.Verbose)
	if err == nil && check == registry.LoginRefused {
		return fmt.Errorf("%s refused the credentials; nothing was saved", host)
	}

	if err := credentials.Save(host, user, secret); err != nil {
		return err
	}
	context.Note("saved the login for %s in Windows Credential Manager (quasidock:%s)", host, host)
	return nil
}

func doLogout(ctx *context.Context, regStr string) error {
	host, err := credentials.NormalizeHost(regStr)
	if err != nil {
		return fmt.Errorf("%q isn't a registry, like ghcr.io or localhost:5000", regStr)
	}
	removed, err := credentials.Remove(host)
	if err != nil {
		return err
	}
	if removed {
		context.Note("removed the login for %s", host)
	} else {
		context.Note("no login is saved for %s", host)
	}
	return nil
}

func usageError() (uint32, error) {
	context.PrintStderr("%s", usage)
	return 1, nil
}

func trustCommand(ctx *context.Context, rest []string) (uint32, error) {
	lock, err := ctx.Store.Lock(false)
	if err != nil {
		return 1, err
	}
	defer lock.Release()

	if len(rest) == 0 {
		return 0, trust.List(ctx.Store)
	}
	if rest[0] == "add" && len(rest) == 3 {
		key, added, err := trust.Add(ctx.Store, rest[1], rest[2])
		if err != nil {
			return 1, err
		}
		if added {
			ctx.Note("added key %s for %s", key.Fingerprint, strings.TrimRight(rest[1], "/"))
		} else {
			ctx.Note("%s already has key %s", strings.TrimRight(rest[1], "/"), key.Fingerprint)
		}
		return 0, nil
	}
	if rest[0] == "rm" && (len(rest) == 2 || len(rest) == 3) {
		var fp *string
		if len(rest) == 3 {
			fp = &rest[2]
		}
		_, err := trust.Remove(ctx.Store, rest[1], fp)
		if err != nil {
			return 1, err
		}
		return 0, nil
	}
	return usageError()
}

func signatureCommand(ctx *context.Context, rest []string) (uint32, error) {
	if len(rest) < 2 {
		return usageError()
	}
	lock, err := ctx.Store.Lock(false)
	if err != nil {
		return 1, err
	}
	defer lock.Release()

	what := rest[0]
	image := rest[1]
	if strings.HasPrefix(image, "-") {
		return usageError()
	}

	switch what {
	case "payload":
		if len(rest) != 2 {
			return usageError()
		}
		payload, err := remote.SignaturePayload(ctx, image)
		if err != nil {
			return 1, err
		}
		if _, err := os.Stdout.Write(payload); err != nil {
			return 1, err
		}
		return 0, nil
	case "push":
		if len(rest) != 4 {
			return usageError()
		}
		payload, err := os.ReadFile(rest[2])
		if err != nil {
			return 1, fmt.Errorf("reading %s: %w", rest[2], err)
		}
		sigText, err := os.ReadFile(rest[3])
		if err != nil {
			return 1, fmt.Errorf("reading %s: %w", rest[3], err)
		}
		if err := remote.PushSignature(ctx, image, payload, sigText); err != nil {
			return 1, err
		}
		return 0, nil
	case "check":
		if len(rest) != 2 {
			return usageError()
		}
		envReg := ""
		if ctx.Env != nil {
			envReg = ctx.Env["QUASIDOCK_REGISTRY"]
		}
		target, err := remote.Resolve(envReg, image)
		if err != nil {
			return 1, err
		}
		reg, err := registry.New(target.Ref, "pull", ctx.Verbose)
		if err != nil {
			return 1, err
		}
		dig, err := remote.ManifestDigest(ctx, image)
		if err != nil {
			return 1, err
		}
		check, err := remote.CheckSignature(ctx.Store, reg, target.Ref, nil, dig)
		if err != nil {
			return 1, err
		}
		switch check.Kind {
		case remote.SigNoKey:
			return 1, fmt.Errorf("%s: no key here for its registry", image)
		case remote.SigMissing:
			return 1, fmt.Errorf("%s: not signed", image)
		case remote.SigWrong:
			return 1, fmt.Errorf("%s: %s", image, check.Why)
		case remote.SigSigned:
			context.Note("%s: signed, by key %s", image, check.Fingerprint)
			return 0, nil
		}
	}
	return usageError()
}

func grantsCommand(ctx *context.Context, rest []string) (uint32, error) {
	var id *string
	var takeBack *string

	i := 0
	for i < len(rest) {
		if !strings.HasPrefix(rest[i], "--") {
			if id != nil {
				return usageError()
			}
			val := rest[i]
			id = &val
			i++
			continue
		}
		opt, err := readOption(rest, &i, nil)
		if err != nil || opt.Name != "take-back" || opt.Value == nil {
			return usageError()
		}
		takeBack = opt.Value
		i++
	}

	if id != nil && takeBack != nil {
		return usageError()
	}

	lock, err := ctx.Store.Lock(false)
	if err != nil {
		return 1, err
	}
	defer lock.Release()

	if takeBack != nil {
		return 0, grants.TakeBackLabel(ctx, *takeBack)
	}
	return 0, grants.Show(ctx, id)
}

func Dispatch(ctx *context.Context, allArgs []string) (uint32, error) {
	args := allArgs
	for len(args) > 0 && isFlag(args[0], "verbose") {
		ctx.Verbose = true
		args = args[1:]
	}
	if len(args) == 0 {
		return usageError()
	}

	cmd := args[0]
	rest := args[1:]

	if cmd == "help" || isFlag(cmd, "help") || cmd == "-h" {
		context.PrintStderr("%s", usage)
		return 0, nil
	}
	if cmd == "version" || isFlag(cmd, "version") || cmd == "-V" {
		fmt.Printf("quasidock %s\n", version.Version)
		return 0, nil
	}
	if replacement := map[string]string{"list": "images", "info": "image info", "ls": "image files", "cat": "image cat", "-v": "--verbose"}[cmd]; replacement != "" {
		return 1, fmt.Errorf("%s was removed; use quasidock %s", cmd, replacement)
	}
	if cmd == "container-supervisor" {
		if len(rest) != 3 && (len(rest) != 4 || rest[3] != "attach") {
			return usageError()
		}
		_ = os.Setenv("QUASIDOCK_HOME", rest[0])
		st, err := store.Open(rest[0], ctx.Verbose, "")
		if err != nil {
			return 1, err
		}
		ctx.Store = st
		ctx.Store.Verbose = ctx.Verbose
		ctx.Store.EventWarnings = ctx.Stderr
		return run.Supervise(ctx, rest[1], rest[2], len(rest) == 4)
	}
	if cmd == "login" {
		opts, err := parseLoginOptions(rest)
		if err != nil {
			return usageError()
		}
		return 0, doLogin(ctx, opts)
	}
	if cmd == "logout" {
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return usageError()
		}
		return 0, doLogout(ctx, rest[0])
	}
	if cmd == "lazy-helper" {
		if len(rest) != 2 {
			return usageError()
		}
		_ = os.Setenv("QUASIDOCK_HOME", rest[0])
		st, err := store.Open(rest[0], ctx.Verbose, "")
		if err != nil {
			return 1, err
		}
		ctx.Store = st
		if err := lazy.RunHelper(st, rest[1], ctx.Verbose); err != nil {
			return 1, err
		}
		return 0, nil
	}
	switch cmd {
	case "stats", "events", "system", "convert", "build", "volume", "container", "create", "commit", "ps", "diff", "cp", "exec", "attach", "start", "restart", "pause", "unpause", "kill", "stop", "rm", "logs", "image", "images", "inspect", "tag", "pull", "push", "save", "load", "rmi", "run", "search", "trust", "signature", "prune", "repair", "grants":
	default:
		return usageError()
	}

	st, err := store.Open("", ctx.Verbose, "")
	if err != nil {
		return 1, err
	}
	ctx.Store = st
	ctx.Store.Verbose = ctx.Verbose
	ctx.Store.EventWarnings = ctx.Stderr

	switch cmd {
	case "events":
		return eventsCommand(ctx, rest)
	case "system":
		if len(rest) == 0 || rest[0] != "events" {
			return 1, errors.New("usage: quasidock system events [OPTIONS]")
		}
		return eventsCommand(ctx, rest[1:])
	case "volume":
		return volumeCommand(ctx, rest)
	case "convert":
		return convertCommand(ctx, rest)
	case "container":
		return containerCommand(ctx, rest)
	case "exec":
		return execCommand(ctx, rest)
	case "ps":
		return containerCommand(ctx, append([]string{"ls"}, rest...))
	case "stats", "create", "commit", "diff", "cp", "attach", "start", "restart", "pause", "unpause", "kill", "stop", "rm", "logs":
		return containerCommand(ctx, append([]string{cmd}, rest...))
	case "inspect":
		return inspectAny(ctx, rest)
	case "image":
		return imageCommand(ctx, rest)
	case "images":
		return imageCommand(ctx, append([]string{"ls"}, rest...))
	case "tag", "pull", "push", "save", "load":
		return imageCommand(ctx, append([]string{cmd}, rest...))
	case "rmi":
		return imageCommand(ctx, append([]string{"rm"}, rest...))
	case "build":
		var opts builder.Options
		var recipePath *string
		for _, arg := range rest {
			if isFlag(arg, "keep-build-dir") {
				opts.KeepBuildDir = true
			} else if isFlag(arg, "rebuild") {
				opts.Rebuild = true
			} else if isFlag(arg, "clean") {
				opts.Clean = true
			} else if recipePath == nil && !strings.HasPrefix(arg, "-") {
				val := arg
				recipePath = &val
			} else {
				return usageError()
			}
		}
		lock, err := ctx.Store.Lock(false)
		if err != nil {
			return 1, err
		}
		defer lock.Release()

		var rArg string
		if recipePath != nil {
			rArg = *recipePath
		}
		rp, err := builder.RecipePath(ctx, rArg)
		if err != nil {
			return 1, err
		}
		img, err := builder.Build(ctx, rp, opts)
		if err != nil {
			return 1, err
		}
		return 0, lifecycle.Install(ctx, img)

	case "search":
		for _, arg := range rest {
			if strings.HasPrefix(arg, "-") {
				return usageError()
			}
		}
		lock, err := ctx.Store.Lock(false)
		if err != nil {
			return 1, err
		}
		defer lock.Release()
		envReg := ""
		if ctx.Env != nil {
			envReg = ctx.Env["QUASIDOCK_REGISTRY"]
		}
		matches, err := catalog.Search(ctx.Store, envReg, rest, ctx.Verbose)
		if err != nil {
			return 1, err
		}
		if len(matches) == 0 {
			context.Note("no app in catalog matches %s", strings.Join(rest, " "))
			return 1, nil
		}
		fmt.Printf("%-32s %-20s %s\n", "ID", "VERSION", "SUMMARY")
		for _, m := range matches {
			fmt.Printf("%-32s %-20s %s\n", m.Entry.ID, m.Entry.Version, m.Entry.Describe())
		}
		return 0, nil

	case "run":
		opts, err := parseRunOptions(ctx, rest)
		if err != nil {
			return 1, err
		}
		return runImage(ctx, opts)

	case "trust":
		return trustCommand(ctx, rest)

	case "signature":
		return signatureCommand(ctx, rest)

	case "prune":
		var opts lifecycle.PruneOptions
		for _, arg := range rest {
			if isFlag(arg, "dry-run") {
				opts.DryRun = true
			} else if isFlag(arg, "downloads") {
				opts.Downloads = true
			} else if isFlag(arg, "data") {
				opts.Data = true
			} else {
				return usageError()
			}
		}
		lock, err := ctx.Store.Lock(true) // Exclusive lock for prune
		if err != nil {
			return 1, err
		}
		defer lock.Release()
		return 0, lifecycle.Prune(ctx, opts)

	case "repair":
		if len(rest) != 0 {
			return usageError()
		}
		lock, err := ctx.Store.Lock(false)
		if err != nil {
			return 1, err
		}
		defer lock.Release()
		_ = hkcu.SettleLeftovers(ctx.Store, "")
		if err := grants.RepairMounts(ctx.Store); err != nil {
			return 1, err
		}
		unsettled1, _ := grants.SettleAll(ctx)
		unsettled2, _ := grants.SettleOrphans(ctx)
		if (unsettled1 + unsettled2) > 0 {
			return 1, nil
		}
		return 0, nil

	case "grants":
		return grantsCommand(ctx, rest)

	default:
		return usageError()
	}
}

// Run is the main entry point called by cmd/quasidock.
func Run(args []string) uint32 {
	envMap := make(map[string]string)
	for _, kv := range os.Environ() {
		if eq := strings.IndexByte(kv, '='); eq != -1 {
			envMap[kv[:eq]] = kv[eq+1:]
		}
	}
	if envMap["QUASIDOCK_ID"] != "" {
		_ = environment.RestoreUser(envMap)
	}

	ctx := context.New()
	for k, v := range envMap {
		ctx.Env[k] = v
	}

	code, err := Dispatch(ctx, args)
	if err != nil {
		context.PrintStderr("error: %v\n", err)
		return 1
	}
	return code
}
