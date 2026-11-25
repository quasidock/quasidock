package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/catalog"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/info"
	"github.com/quasidock/quasidock/internal/lazy"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/peek"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/services"
	"github.com/quasidock/quasidock/internal/store"
)

type inspectedImage struct {
	Id           string
	RepoTags     []string
	RepoDigests  []string
	OS           string `json:"Os"`
	Architecture string
	Size         int64
	Config       struct {
		Labels     map[string]string
		UnsetEnv   []string `json:",omitempty"`
		WorkingDir string
		Entrypoint []string
		Cmd        []string
		Env        []string
	}
	Quasidock   oci.AppConfig
	Annotations map[string]string `json:",omitempty"`
	Source      string
	Default     bool
	Lazy        *string `json:"Lazy,omitempty"`
}

func inspectImage(ctx *context.Context, idx *store.ImageIndex, ref store.Ref) (inspectedImage, error) {
	out := inspectedImage{Id: ref.Manifest, OS: "windows", Architecture: "amd64", RepoTags: []string{}, RepoDigests: []string{}}
	img, err := ctx.Store.ReadImage(ref.ID, ref.Manifest)
	if err != nil {
		return out, err
	}
	out.Quasidock = img.Config
	out.Annotations = img.Manifest.Annotations
	out.Config.Labels = img.Config.Labels
	out.Config.UnsetEnv = img.Config.UnsetEnv
	out.Source = ref.Source
	out.Default = idx.Defaults[strings.ToLower(ref.ID)] == ref.Manifest
	out.Config.Entrypoint = append([]string(nil), img.Config.Entrypoint...)
	out.Config.Cmd = append([]string(nil), img.Config.Cmd...)
	out.Config.WorkingDir = img.Config.Workdir
	out.Config.Env = []string{}
	for k, v := range img.Config.Env {
		out.Config.Env = append(out.Config.Env, k+"="+v)
	}
	sort.Strings(out.Config.Env)
	out.Size = img.Manifest.Config.Size
	seen := map[string]bool{}
	for _, layer := range img.Manifest.Layers {
		if !seen[layer.Digest] {
			out.Size += layer.Size
			seen[layer.Digest] = true
		}
	}
	seen = map[string]bool{}
	for _, repository := range ref.Repositories {
		d := repository + "@" + ref.Manifest
		out.RepoDigests = append(out.RepoDigests, d)
		seen[d] = true
	}
	for name, digest := range idx.Names {
		if digest != ref.Manifest {
			continue
		}
		r, err := registry.ParseReference(name)
		if err != nil {
			return out, err
		}
		if r.Digest == "" {
			out.RepoTags = append(out.RepoTags, name)
		}
		d := r.Host + "/" + r.Repository + "@" + digest
		if !seen[d] {
			out.RepoDigests = append(out.RepoDigests, d)
			seen[d] = true
		}
	}
	sort.Strings(out.RepoTags)
	sort.Strings(out.RepoDigests)
	out.Lazy = lazy.Describe(ctx.Store, img.Manifest)
	return out, nil
}

func imageList(ctx *context.Context, args []string) error {
	quiet := false
	for _, arg := range args {
		if arg == "-q" || arg == "--quiet" {
			quiet = true
		} else {
			return fmt.Errorf("unknown images option %q", arg)
		}
	}
	idx, err := ctx.Store.ImageCatalog()
	if err != nil {
		return err
	}
	digests := make([]string, 0, len(idx.Images))
	for d := range idx.Images {
		digests = append(digests, d)
	}
	sort.Strings(digests)
	if quiet {
		for _, d := range digests {
			fmt.Fprintln(ctx.Stdout, d)
		}
		return nil
	}
	fmt.Fprintf(ctx.Stdout, "%-54s %-24s %-12s %s\n", "REPOSITORY", "TAG", "IMAGE ID", "SIZE")
	type row struct {
		repository, tag, digest string
		size                    int64
	}
	var rows []row
	for _, d := range digests {
		img, err := inspectImage(ctx, idx, idx.Images[d])
		if err != nil {
			return err
		}
		found := false
		for name, digest := range idx.Names {
			if digest != d {
				continue
			}
			r, err := registry.ParseReference(name)
			if err != nil {
				return err
			}
			tag := r.Tag
			if tag == "" {
				tag = "<none>"
			}
			rows = append(rows, row{r.Host + "/" + r.Repository, tag, d, img.Size})
			found = true
		}
		if !found {
			rows = append(rows, row{"<none>", "<none>", d, img.Size})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.repository != b.repository {
			return a.repository < b.repository
		}
		if a.tag != b.tag {
			return a.tag < b.tag
		}
		return a.digest < b.digest
	})
	for _, r := range rows {
		fmt.Fprintf(ctx.Stdout, "%-54s %-24s %-12s %d B\n", r.repository, r.tag, oci.DigestHex(r.digest)[:12], r.size)
	}
	return nil
}

func imageInspect(ctx *context.Context, args []string) error {
	format, names, err := parseInspectFlags(args, "usage: quasidock image inspect [-f TEMPLATE] IMAGE [IMAGE...]")
	if err != nil {
		return err
	}
	idx, err := ctx.Store.ImageCatalog()
	if err != nil {
		return err
	}
	items := make([]any, 0, len(names))
	for _, name := range names {
		ref, err := idx.Resolve(ctx.Env["QUASIDOCK_REGISTRY"], name)
		if err != nil {
			return err
		}
		out, err := inspectImage(ctx, idx, *ref)
		if err != nil {
			return err
		}
		items = append(items, out)
	}
	return renderInspect(ctx.Stdout, format, items)
}

func inspectAny(ctx *context.Context, args []string) (uint32, error) {
	format, names, err := parseInspectFlags(args, "usage: quasidock inspect [-f TEMPLATE] NAME|ID [NAME|ID...]")
	if err != nil {
		return 1, err
	}
	lock, err := ctx.Store.Lock(false)
	if err != nil {
		return 1, err
	}
	defer lock.Release()
	idx, err := ctx.Store.ImageCatalog()
	if err != nil {
		return 1, err
	}
	items := make([]any, 0, len(names))
	for _, name := range names {
		rec, cerr := ctx.Store.ResolveContainer(name)
		if cerr == nil {
			items = append(items, rec)
			continue
		}
		if !errors.Is(cerr, store.ErrContainerNotFound) {
			return 1, cerr
		}
		ref, ierr := idx.Resolve(ctx.Env["QUASIDOCK_REGISTRY"], name)
		if ierr != nil {
			return 1, fmt.Errorf("no such container or image %q", name)
		}
		out, err := inspectImage(ctx, idx, *ref)
		if err != nil {
			return 1, err
		}
		items = append(items, out)
	}
	return 0, renderInspect(ctx.Stdout, format, items)
}

func pullImage(ctx *context.Context, image string, isLazy bool) (*oci.Image, error) {
	var prepare func(*registry.Registry, oci.Descriptor) (bool, error)
	if isLazy {
		prepare = func(reg *registry.Registry, desc oci.Descriptor) (bool, error) {
			if !lazy.Eligible(desc) {
				return false, nil
			}
			kind, err := lazy.Prepare(ctx.Store, reg, desc, ctx.Verbose)
			return kind == lazy.PreparedLazy, err
		}
	}
	img, err := remote.FetchPrepared(ctx.Store, ctx.Env["QUASIDOCK_REGISTRY"], image, ctx.Verbose, prepare)
	if err != nil {
		return nil, err
	}
	if err := lifecycle.Install(ctx, img); err != nil {
		return nil, err
	}
	return img, nil
}

func runImage(ctx *context.Context, opts run.Options) (uint32, error) {
	ref, err := selectContainerImage(ctx, opts)
	if err != nil {
		return 1, err
	}
	opts.Image = ref
	return run.Run(ctx, opts)
}

func selectContainerImage(ctx *context.Context, opts run.Options) (*store.Ref, error) {
	// Exclude GC while selecting or filling the cache. Run and Create reacquire
	// this lock and recheck the selected digest before creating the record.
	lock, err := ctx.Store.Lock(false)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	ref, err := ctx.Store.ResolveLocalImage(ctx.Env["QUASIDOCK_REGISTRY"], opts.ID)
	missing := errors.Is(err, store.ErrImageNotFound)
	if err != nil && !missing {
		return nil, err
	}
	if opts.Pull == "always" || missing && opts.Pull != "never" {
		if oci.DigestHex(opts.ID) != "" {
			return nil, errors.New("pull needs a repository reference; a manifest ID is a local selector")
		}
		img, err := pullImage(ctx, opts.ID, false)
		if err != nil {
			return nil, err
		}
		ref = &store.Ref{ID: img.Config.ID, Version: img.Config.Version, Manifest: img.ManifestDigest, Source: img.Source}
	} else if err != nil {
		return nil, err
	}
	return ref, nil
}

func imageRemove(ctx *context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: quasidock image rm IMAGE [IMAGE...]")
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unknown rmi option %q; application data is removed with prune --data", arg)
		}
	}
	var failures []error
	for _, arg := range args {
		ref, last, err := ctx.Store.RemoveImage(ctx.Env["QUASIDOCK_REGISTRY"], arg)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if last {
			if err := grants.UndoAll(ctx, ref.ID); err != nil {
				failures = append(failures, err)
			}
			if err := appcontainer.DeleteProfile(ref.ID); err != nil {
				ctx.Note("warning: cleaning profile for %s: %v", ref.ID, err)
			}
			if err := services.Forget(ctx.Store, ref.ID); err != nil {
				failures = append(failures, err)
			}
			if err := hkcu.SettleLeftovers(ctx.Store, ref.ID); err != nil {
				failures = append(failures, err)
			}
		}
		if _, err := lazy.Prune(ctx.Store, ""); err != nil {
			failures = append(failures, err)
		}
		if _, err := ctx.Store.DeleteUnusedDeployments(ref.Manifest); err != nil {
			failures = append(failures, err)
		}
		fmt.Fprintln(ctx.Stdout, "Removed:", arg)
	}
	return errors.Join(failures...)
}

func imageCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 0 {
		return 1, errors.New("usage: quasidock image <ls|inspect|history|pull|tag|push|save|load|rm|info|files|cat>")
	}
	cmd, rest := args[0], args[1:]
	if cmd == "--help" || cmd == "help" {
		fmt.Fprint(ctx.Stdout, usage)
		return 0, nil
	}
	// Archive operations own their shared store lock, including stream I/O.
	if cmd == "save" {
		return 0, imageSave(ctx, rest)
	}
	if cmd == "load" {
		return 0, imageLoad(ctx, rest)
	}
	lock, err := ctx.Store.Lock(cmd == "rm" || cmd == "remove")
	if err != nil {
		return 1, err
	}
	defer lock.Release()
	switch cmd {
	case "ls", "list":
		return 0, imageList(ctx, rest)
	case "inspect":
		return 0, imageInspect(ctx, rest)
	case "history":
		return historyCommand(ctx, rest)
	case "tag":
		if len(rest) != 2 {
			return 1, errors.New("usage: quasidock tag SOURCE TARGET")
		}
		return 0, ctx.Store.TagImage(ctx.Env["QUASIDOCK_REGISTRY"], rest[0], rest[1])
	case "pull":
		opts, err := parsePullOptions(rest)
		if err != nil {
			return 1, err
		}
		_, err = pullImage(ctx, opts.Image, opts.Lazy)
		return 0, err
	case "push":
		var opts remote.PushOptions
		var names []string
		makeCatalog := false
		for _, arg := range rest {
			if arg == "--sources" {
				opts.Sources = true
			} else if arg == "--catalog" {
				makeCatalog = true
			} else if strings.HasPrefix(arg, "-") {
				return 1, fmt.Errorf("unknown push option %q", arg)
			} else {
				names = append(names, arg)
			}
		}
		if makeCatalog && !opts.Sources && len(names) > 0 {
			return 0, catalog.Push(ctx.Store, ctx.Env["QUASIDOCK_REGISTRY"], names, ctx.Verbose)
		}
		if makeCatalog || len(names) != 1 {
			return 1, errors.New("usage: quasidock push [--sources] IMAGE; to publish elsewhere, tag SOURCE TARGET then push TARGET")
		}
		return 0, remote.Push(ctx, names[0], opts)
	case "rm", "remove":
		return 0, imageRemove(ctx, rest)
	case "info":
		if len(rest) != 1 {
			return 1, errors.New("usage: quasidock image info IMAGE")
		}
		return 0, info.PrintInfo(ctx.Store, ctx.Env["QUASIDOCK_REGISTRY"], rest[0], ctx.Verbose, ctx.Stdout)
	case "files":
		if len(rest) != 1 {
			return 1, errors.New("usage: quasidock image files IMAGE [PATH]")
		}
		return 0, peek.List(ctx.Store, ctx.Env["QUASIDOCK_REGISTRY"], rest[0], ctx.Verbose, ctx.Stdout)
	case "cat":
		if len(rest) != 2 {
			return 1, errors.New("usage: quasidock image cat IMAGE [PATH]")
		}
		return 0, peek.Cat(ctx.Store, ctx.Env["QUASIDOCK_REGISTRY"], rest[0], rest[1], ctx.Verbose, ctx.Stdout)
	default:
		return 1, fmt.Errorf("unknown image command %q", cmd)
	}
}
