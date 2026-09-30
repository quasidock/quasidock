package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/builder"
	"github.com/quasidock/quasidock/internal/catalog"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/credentials"
	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/info"
	"github.com/quasidock/quasidock/internal/lazy"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/peek"
	"github.com/quasidock/quasidock/internal/prompt"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/services"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/trust"
	"github.com/quasidock/quasidock/internal/version"
)

const usage = `usage: quasidock [-v|--verbose] <command> [options]

commands:
  build [--rebuild] [--clean] [--keep-build-dir] [<recipe.json> | <dir>]
                                  build an app from a recipe, or from the quasidock.json of a
                                  project's directory (by default the current one), and
                                  install it; --rebuild builds even if an earlier build had
                                  the same inputs, and --clean also starts a project's
                                  modules without their kept work directories
  search [<words>...]             find apps in the default registry's catalog
  info <image>                    show what an app in a registry is, needs and may reach
  ls <image>                      list the files of an app in a registry without pulling
                                  it, as far as its layers' format allows (eStargz)
  cat <image> <path>              write a file of an app in a registry to stdout, the
                                  same way
  pull [--yes] [--lazy] <image>   install an app from a registry, once you agree to what it
                                  may reach; --yes agrees without asking; --lazy installs
                                  it before its large eStargz layers are downloaded, their
                                  files fetched as they're first used while the rest
                                  downloads
  push [--sources] <app-id> [<image>]
                                  publish an installed app to a registry; --sources also
                                  keeps the files it was built from next to it
  push --catalog <app-id>...      make the default registry's catalog list these apps,
                                  as it has them
  login [--username=<user>] [--password-stdin] <registry>
                                  check and save a login for a registry
  logout <registry>               delete a registry's saved login
  run [options] <app-id> [args]   run an installed app
  list                            list installed apps and their exported commands
  grants [<app-id>]               list what quasidock changed outside its store for apps, in
                                  this store and others, what stores since deleted left,
                                  and low labels on your folders no record explains
  grants --take-back <path>       remove a low label that no record explains
  trust                           list the keys that images' signatures are checked against
  trust add <registry/path> <key> check images under that path against a public key, as
                                  ` + "`cosign generate-key-pair`" + ` makes in cosign.pub
  trust rm <registry/path> [<fingerprint>]
                                  stop checking them against keys added there
  signature payload <image>       print what to sign for an image, for ` + "`cosign sign-blob`" + `
  signature push <image> <payload file> <signature file>
                                  put that signature beside the image, where pull looks
  signature check <image>         say whether an image has a good signature
  rm [--delete-data] <app-id>     uninstall an app
  prune [--dry-run] [--downloads] [--data]
                                  delete what no installed app needs; also cached
                                  downloads and tool caches, and the data of uninstalled
                                  apps
  repair                          take back what apps may no longer reach, and what stores
                                  since deleted left
  version                         print quasidock's version

An image is <registry>/<repository>[:tag][@digest], e.g. ghcr.io/owner/node:24.21.0,
or <app-id>[:tag][@digest] for the app's image in the default registry, e.g.
org.nodejs.node for ghcr.io/zigsaw-io/org.nodejs.node. QUASIDOCK_REGISTRY changes the
default registry. push goes to the app's image there unless given one, and tags
with the app's version unless given a tag.

Pushing, and pulling private images, needs a login. login saves one per registry in
Windows Credential Manager; --password-stdin reads the password from stdin. For CI
and scripts, QUASIDOCK_REGISTRY_USERNAME and QUASIDOCK_REGISTRY_PASSWORD are used instead,
but only for the default registry. Without either, the login ` + "`docker login`" + ` saved is
used. From a multi-platform index, pull takes the image for windows/amd64. Blobs over
64 MiB are pushed in chunks of QUASIDOCK_UPLOAD_CHUNK_SIZE bytes (0: in one request).

run options:
  --command=<name>                run one of the app's exported commands, or another
                                  executable or batch file from the app or System32
  --sandbox=soft|low|appcontainer soft shapes the environment; low (the default, unless
                                  the app's image asks for another) also keeps the app
                                  from writing anywhere but its data directory and the
                                  paths granted; appcontainer enforces all of the app's
                                  permissions
  --filesystem=<location>[:ro]    grant access to a host location: cwd or an absolute path
  --nofilesystem=<location>       take away access the app grants
  --share=network                 allow network access, and pass on your proxy variables
  --share=host                    put quasidock-host on its PATH, which runs your own programs
                                  in your real environment, with the run's rights (not in
                                  an AppContainer)
  --unshare=<what>                take away what --share gives, or the app's image asks for
  --port=<service>=<port>         serve one of the app's services on another port
  --env=NAME=VALUE                set an environment variable
  --ephemeral                     use a fresh data directory, deleted afterwards

What the low and appcontainer sandboxes grant on host paths stays while the app's
permissions give it; what a command line adds goes when the app's last run ends. At
low integrity, a working directory that's your user profile, a directory it's in, or a
drive's root stays read-only.

An app's services (info lists them) are on localhost, on the ports its config says
or --port gives, in every sandbox.

Under appcontainer, a run of an app with the network has its container exempt from
loopback isolation while the app's runs go, so that it reaches other programs on this
machine; with QUASIDOCK_LOOPBACK_EXEMPTION=0, quasidock never exempts one.

Data lives in %QUASIDOCK_HOME%, or %LOCALAPPDATA%\quasidock when that isn't set.
`

func isFlag(arg, name string) bool {
	if name == "verbose" && arg == "-v" {
		return true
	}
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
	settings     run.Settings
	filesystem   []string
	nofilesystem []string
	env          []string
	ports        []string
}

var settingsFlags = []string{"ephemeral"}

func (p *settingsParser) apply(opt Option) error {
	name := opt.Name
	if name == "ephemeral" {
		if opt.Value != nil {
			return errors.New("--ephemeral takes no value")
		}
		b := true
		p.settings.Ephemeral = &b
		return nil
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
		if g == nil || g.Where.Type == oci.FsGrantUnknown || g.Where.Type == oci.FsGrantFolder {
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
		if !strings.ContainsRune(val, '=') {
			return fmt.Errorf("--env expects NAME=VALUE, got %q", val)
		}
		p.env = append(p.env, val)
	case "port":
		if _, _, ok := services.ParsePort(val); !ok {
			return fmt.Errorf("--port expects <service>=<port>, a port from 1 to 65535; got %q", val)
		}
		p.ports = append(p.ports, val)
	default:
		return fmt.Errorf("unknown run option --%s", name)
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
	var opts run.Options
	var parser settingsParser

	flags := append(settingsFlags, "verbose")
	i := 0
	for i < len(args) {
		if args[i] == "--" {
			i++
			break
		}
		if !strings.HasPrefix(args[i], "--") {
			break
		}
		opt, err := readOption(args, &i, flags)
		if err != nil {
			return opts, err
		}
		if opt.Name == "verbose" && opt.Value == nil {
			ctx.Verbose = true
			if ctx.Store != nil {
				ctx.Store.Verbose = true
			}
		} else if opt.Name == "command" && opt.Value != nil {
			opts.Command = opt.Value
		} else {
			if err := parser.apply(opt); err != nil {
				return opts, err
			}
		}
		i++
	}
	if i == len(args) {
		return opts, errors.New("usage error")
	}

	opts.ID = args[i]
	opts.Args = args[i+1:]
	opts.Settings = parser.finish()
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

func list(ctx *context.Context) error {
	refs, err := ctx.Store.ListRefs()
	if err != nil {
		return err
	}

	fmt.Printf("%-32s %-20s %-20s %s\n", "ID", "VERSION", "MANIFEST", "EXPORTS")
	for _, ref := range refs {
		image, err := ctx.Store.ReadImage(ref.ID, ref.Manifest)
		var names []string
		if err == nil && image != nil {
			for k := range image.Config.Exports {
				names = append(names, k)
			}
			sort.Strings(names)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "%-32s %-20s %-20s %s", ref.ID, ref.Version, oci.ShortDigest(ref.Manifest), strings.Join(names, ", "))

		if err == nil && image != nil {
			cfg := image.Config
			for _, r := range cfg.Runtimes {
				fmt.Fprintf(&sb, "  (runtime %s %s)", r.ID, r.Version)
			}
			for _, k := range cfg.Registry {
				fmt.Fprintf(&sb, "  (keeps HKCU\\%s)", k)
			}
			if state := lazy.Describe(ctx.Store, image.Manifest); state != nil {
				fmt.Fprintf(&sb, "  %s", *state)
			}
		}
		fmt.Println(sb.String())
	}

	_ = grants.NoteLeftovers(ctx)
	return nil
}

func remove(ctx *context.Context, id string, deleteData bool) error {
	ref, err := ctx.Store.ReadRef(id)
	if err != nil || ref == nil {
		return fmt.Errorf("%s is not installed", id)
	}

	_ = grants.UndoAll(ctx, id)
	_ = appcontainer.DeleteProfile(id)
	_ = services.Forget(ctx.Store, id)
	_ = hkcu.SettleLeftovers(ctx.Store, id)

	_ = ctx.Store.DeleteRef(id)

	if deleted, err := ctx.Store.DeleteUnusedDeployments(ref.Manifest); err == nil && deleted {
		context.Note("kept %s's files, which are still in use; `quasidock prune` removes them later", id)
	}
	if deleteData {
		_ = store.DeleteTree(ctx.Store.Path("data", id))
	}

	withData := ""
	if deleteData {
		withData = " and its data"
	}
	context.Note("removed %s %s%s", id, ref.Version, withData)
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
		_, _, err := trust.Add(ctx.Store, rest[1], rest[2])
		if err != nil {
			return 1, err
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
		os.Stdout.Write(payload)
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

	st, err := store.Open("", ctx.Verbose, "")
	if err != nil {
		return 1, err
	}
	ctx.Store = st
	ctx.Store.Verbose = ctx.Verbose

	switch cmd {
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

	case "pull":
		consent := remote.ConsentAsk
		var image *string
		lazily := false
		for _, arg := range rest {
			if isFlag(arg, "yes") {
				consent = remote.ConsentGiven
			} else if isFlag(arg, "lazy") {
				lazily = true
			} else if image == nil && !strings.HasPrefix(arg, "-") {
				val := arg
				image = &val
			} else {
				return usageError()
			}
		}
		if image == nil {
			return usageError()
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
		img, err := remote.Fetch(ctx.Store, envReg, *image, ctx.Verbose)
		if err != nil {
			return 1, err
		}
		if consent == remote.ConsentAsk {
			if err := remote.AgreeTo(ctx, img.Manifest, img.Config, consent); err != nil {
				return 1, err
			}
		}
		_ = lazily
		return 0, lifecycle.Install(ctx, img)

	case "push":
		var opts remote.PushOptions
		makeCatalog := false
		var names []string
		for _, arg := range rest {
			if isFlag(arg, "sources") {
				opts.Sources = true
			} else if isFlag(arg, "catalog") {
				makeCatalog = true
			} else if !strings.HasPrefix(arg, "-") {
				names = append(names, arg)
			} else {
				return usageError()
			}
		}
		if makeCatalog {
			if opts.Sources || len(names) == 0 {
				return usageError()
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
			return 0, catalog.Push(ctx.Store, envReg, names, ctx.Verbose)
		}
		if len(names) != 1 && len(names) != 2 {
			return usageError()
		}
		lock, err := ctx.Store.Lock(false)
		if err != nil {
			return 1, err
		}
		defer lock.Release()
		targetImg := ""
		if len(names) == 2 {
			targetImg = names[1]
		}
		return 0, remote.Push(ctx, names[0], targetImg, opts)

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
			summary := ""
			if m.Entry.Summary != nil {
				summary = *m.Entry.Summary
			}
			fmt.Printf("%-32s %-20s %s\n", m.Entry.ID, m.Entry.Version, summary)
		}
		return 0, nil

	case "info":
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return usageError()
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
		return 0, info.PrintInfo(ctx.Store, envReg, rest[0], ctx.Verbose, os.Stdout)

	case "ls", "cat":
		isLs := (cmd == "ls")
		expected := 1
		if !isLs {
			expected = 2
		}
		if len(rest) != expected {
			return usageError()
		}
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
		envReg, _ := ctx.GetEnv("QUASIDOCK_REGISTRY")
		if isLs {
			return 0, peek.List(ctx.Store, envReg, rest[0], ctx.Verbose, ctx.Stdout)
		}
		return 0, peek.Cat(ctx.Store, envReg, rest[0], rest[1], ctx.Verbose, ctx.Stdout)

	case "run":
		opts, err := parseRunOptions(ctx, rest)
		if err != nil {
			return usageError()
		}
		return run.Run(ctx, opts)

	case "list":
		if len(rest) != 0 {
			return usageError()
		}
		return 0, list(ctx)

	case "trust":
		return trustCommand(ctx, rest)

	case "signature":
		return signatureCommand(ctx, rest)

	case "rm":
		deleteData := false
		var id *string
		for _, arg := range rest {
			if isFlag(arg, "delete-data") {
				deleteData = true
			} else if id == nil && !strings.HasPrefix(arg, "-") {
				val := arg
				id = &val
			} else {
				return usageError()
			}
		}
		if id == nil {
			return usageError()
		}
		lock, err := ctx.Store.Lock(false)
		if err != nil {
			return 1, err
		}
		defer lock.Release()
		return 0, remove(ctx, *id, deleteData)

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
