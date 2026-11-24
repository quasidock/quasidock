package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/buildcontext"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/roots"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/tree"
)

type Options struct {
	KeepBuildDir bool
	Rebuild      bool
	Clean        bool
}

const sourceDateEpoch = "315532800"

func RecipePath(ctx *context.Context, arg string) (string, error) {
	if arg != "" {
		stat, err := os.Stat(arg)
		if err == nil && !stat.IsDir() {
			return arg, nil
		}
	}
	dir := arg
	if dir == "" {
		dir = "."
	}
	manifest := filepath.Join(dir, ManifestName)
	if !store.Exists(manifest) {
		shown, err := filepath.Abs(dir)
		if err != nil {
			shown = dir
		}
		return "", fmt.Errorf("%s has no %s; give a recipe, or a project directory with one", shown, ManifestName)
	}
	return manifest, nil
}

func Build(ctx *context.Context, recipePath string, opts Options) (*oci.Image, error) {
	bytesRead, err := os.ReadFile(recipePath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", recipePath, err)
	}
	r, err := recipe.Parse(recipePath, bytesRead)
	if err != nil {
		return nil, err
	}
	source, err := filepath.Abs(recipePath)
	if err != nil {
		source = recipePath
	}

	projects, err := TakeSnapshots(ctx, r, recipePath)
	if err != nil {
		return nil, err
	}

	var inputs *string
	if len(r.Host) == 0 {
		h, err := inputHash(ctx, r, bytesRead, recipePath, projects, true)
		if err != nil {
			return nil, err
		}
		inputs = &h
	}

	var work *Work
	if len(r.Host) == 0 && wantedWork(r, projects) {
		w, err := initWork(ctx, r, bytesRead, recipePath, source, projects)
		if err != nil {
			return nil, err
		}
		work = w
	}
	if opts.Clean && work != nil {
		if err := work.drop(); err != nil {
			return nil, err
		}
	}

	if inputs != nil && !opts.Rebuild && !opts.Clean && !opts.KeepBuildDir {
		if digest, err := ctx.Store.ReadBuildCache(*inputs); err == nil && digest != "" {
			if loaded, err := ctx.Store.ReadCompleteImage(digest); err == nil && loaded != nil {
				if err := projects.recheckGraph(ctx); err != nil {
					return nil, err
				}
				if ctx.Verbose {
					ctx.Note("%s %s: built before from the same inputs (%s); --rebuild builds it again", r.ID, r.Version, oci.ShortDigest(digest))
				}
				return &oci.Image{
					ManifestDigest: digest,
					Manifest:       loaded.Manifest,
					Config:         loaded.Config,
					Source:         source,
				}, nil
			}
		}
	}

	start := ctx.Now()
	img, err := buildImage(ctx, r, recipePath, source, projects, work, opts)
	if err != nil {
		return nil, err
	}
	if r.HasBuildCommands() {
		ctx.Note("built %s %s in %s", r.ID, r.Version, ctx.Since(start))
	}
	if inputs != nil {
		_ = ctx.Store.WriteBuildCache(*inputs, img.ManifestDigest)
	}
	return img, nil
}

func inputHash(ctx *context.Context, r *recipe.Recipe, bytesRead []byte, recipePath string, projects *Snapshots, withProjects bool) (string, error) {
	h := sha256.New()
	if withProjects {
		h.Write([]byte("quasidock build inputs\n"))
	} else {
		h.Write([]byte("quasidock work inputs\n"))
	}

	selfExe, err := os.Executable()
	if err == nil {
		if fh, err := store.Sha256File(selfExe); err == nil {
			h.Write([]byte(fh.Hex))
		}
	}
	h.Write(bytesRead)

	scopes := []struct {
		r        *recipe.Recipe
		projects *Snapshots
	}{{r, projects}}
	if len(r.Builds) > 0 {
		order, err := r.ReachableBuilds()
		if err != nil {
			return "", err
		}
		for _, unit := range order {
			scopes = append(scopes, struct {
				r        *recipe.Recipe
				projects *Snapshots
			}{unitRecipe(r, unit), projects.units[strings.ToLower(unit.Name)]})
		}
	}
	for _, scope := range scopes {
		baseDir := filepath.Dir(recipePath)
		if baseDir == "" {
			baseDir = "."
		}
		for _, m := range scope.r.Stages {
			for i, src := range m.Sources {
				if src.Context != nil {
					snap := scope.projects.Get(m.Name, i)
					if snap == nil {
						return "", fmt.Errorf("stage %s: missing context snapshot", m.Name)
					}
					h.Write([]byte(snap.Hash))
					continue
				}
				if src.Path == nil {
					continue
				}
				p := *src.Path
				if snap := scope.projects.Get(m.Name, i); snap != nil {
					if withProjects {
						h.Write([]byte(snap.Hash))
					}
					continue
				}
				full := p
				if !filepath.IsAbs(p) {
					full = filepath.Join(baseDir, p)
				}
				fh, err := store.Sha256File(full)
				if err != nil {
					return "", fmt.Errorf("stage %s: reading %s: %w", m.Name, full, err)
				}
				h.Write([]byte(fh.Hex))
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type Tool struct {
	Image    string
	ID       string
	Version  string
	What     string
	Dir      string
	Path     []string
	Env      map[string]string
	Runtimes []oci.DirPlaceholder
	Brings   int
}

func (t Tool) Placeholders(rootsFile string) oci.Placeholders {
	cachePath := ""
	if oci.UsesCache(t.Path, t.Env) {
		cachePath = fmt.Sprintf("%s\\cache\\%s", DriveLetter, t.ID)
	}
	return oci.Placeholders{
		App:      t.Dir,
		Data:     DriveLetter + `\data`,
		Cache:    cachePath,
		Roots:    rootsFile,
		Runtimes: t.Runtimes,
	}
}

type Tools struct {
	recipePath string
	list       []Tool
}

func (ts *Tools) Add(t Tool) (*int, error) {
	for _, other := range ts.list {
		if other.Image == t.Image {
			return nil, nil
		}
		if strings.EqualFold(other.ID, t.ID) {
			return nil, fmt.Errorf("%s: %s is %s %s (%s), and %s is %s %s (%s); a build can't use two versions of %s",
				ts.recipePath, other.What, other.ID, other.Version, oci.ShortDigest(other.Image),
				t.What, t.ID, t.Version, oci.ShortDigest(t.Image), t.ID)
		}
	}
	ts.list = append(ts.list, t)
	idx := len(ts.list) - 1
	return &idx, nil
}

func buildImage(ctx *context.Context, r *recipe.Recipe, recipePath, source string, projects *Snapshots, work *Work, opts Options) (*oci.Image, error) {
	units, err := executeBuildUnits(ctx, r, recipePath, source, projects, opts)
	if err != nil {
		return nil, err
	}
	defer units.close()
	result, err := executeScope(ctx, r, recipePath, projects, work, opts)
	if err != nil {
		return nil, err
	}
	defer result.close(ctx, opts.KeepBuildDir)
	if err := projects.recheckGraph(ctx); err != nil {
		return nil, err
	}
	appTree, from, layers := result.tree, result.from, result.layers
	runtimeDirsList, buildInfo := result.runtimes, result.info
	building := r.HasBuildCommands()
	mergeUnitBuildInfo(buildInfo, units.info)

	config, err := r.AppConfig(from, buildInfo)
	if err != nil {
		return nil, err
	}
	if err := oci.ValidateConfig(recipePath, config); err != nil {
		return nil, err
	}
	if config.Workdir != "" && config.Workdir != "." {
		found := false
		for _, node := range appTree.Sorted() {
			if node.Content.Kind == tree.ContentDir && strings.EqualFold(node.Path, config.Workdir) {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%s: workdir %q is not a directory in the app", recipePath, config.Workdir)
		}
	}

	placeholders := oci.Placeholders{
		App:      "",
		Data:     "",
		Runtimes: runtimeDirsList,
	}
	if err := checkCommand(ctx, appTree, placeholders, "entrypoint runs", config.Entrypoint[0]); err != nil {
		return nil, err
	}
	for _, s := range config.Permissions.Services {
		if s.Command != "" {
			if err := checkCommand(ctx, appTree, placeholders, fmt.Sprintf("service %s runs", s.Name), s.Command); err != nil {
				return nil, err
			}
		}
	}

	start := ctx.Now()
	var prefetch []string
	for _, p := range r.Prefetch {
		if actual, ok := appTree.FilePath(p); ok {
			prefetch = append(prefetch, actual)
		} else {
			ctx.Note("warning: prefetch %q isn't a file of the app's; it's left out", p)
		}
	}

	layerTmp := ctx.Store.TmpPath("layer")
	defer func() { _ = os.Remove(layerTmp) }()
	layerTarTmp := ctx.Store.TmpPath("layer-tar")
	defer func() { _ = os.Remove(layerTarTmp) }()

	layerBlob, err := format.WriteLayer(layerTmp, layerTarTmp, r.Layer.Format(), appTree.WriteTar, prefetch)
	if err != nil {
		return nil, fmt.Errorf("writing layer: %w", err)
	}
	ctx.Timed(start, "write layer (%d entries)", len(appTree.Sorted()))

	if !building {
		if err := projects.recheckGraph(ctx); err != nil {
			return nil, err
		}
	}

	hHex := oci.DigestHex(layerBlob.Digest)
	desc, err := ctx.Store.PutBlobFile(layerBlob.Path, store.FileHash{Hex: hHex, Size: layerBlob.Size}, layerBlob.MediaType)
	if err != nil {
		return nil, err
	}
	layers = append(layers, layerBlob.Descriptor(desc))

	configJSON, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	configType := oci.MediaTypeConfig
	if config.Workdir != "" {
		configType = oci.MediaTypeConfigV3
	}
	cfgDesc, err := ctx.Store.PutBlob(configJSON, configType)
	if err != nil {
		return nil, err
	}

	annotations := map[string]string{
		"org.opencontainers.image.title":   r.ID,
		"org.opencontainers.image.version": r.Version,
	}
	if r.Summary != nil {
		annotations["org.opencontainers.image.description"] = *r.Summary
	}
	if r.License != nil {
		annotations["org.opencontainers.image.licenses"] = *r.License
	}
	if r.Homepage != nil {
		annotations["org.opencontainers.image.url"] = *r.Homepage
	}

	manifest := oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeManifest,
		Config:        cfgDesc,
		Layers:        layers,
		Annotations:   annotations,
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	manifestDesc, err := ctx.Store.PutBlob(manifestJSON, oci.MediaTypeManifest)
	if err != nil {
		return nil, err
	}

	return &oci.Image{
		ManifestDigest: manifestDesc.Digest,
		Manifest:       manifest,
		Config:         *config,
		Source:         source,
	}, nil
}

func checkCommand(ctx *context.Context, appTree *tree.Tree, placeholders oci.Placeholders, what, command string) error {
	if strings.HasPrefix(command, "${app}") {
		sub := command[len("${app}"):]
		sub = strings.TrimPrefix(sub, `\`)
		sub = strings.TrimPrefix(sub, `/`)
		if appTree.IsFile(sub) {
			return nil
		}
	} else if oci.IsPlaceholderPath(command) {
		expanded := placeholders.Expand(command)
		if store.Exists(expanded) {
			return nil
		}
		return fmt.Errorf("%s %q, which is not a file in that runtime", what, command)
	} else if appTree.IsFile(command) {
		return nil
	}
	return fmt.Errorf("%s %q, which is not a file in the app tree the stages produce", what, command)
}

func runtimeDirs(c oci.AppConfig, dirs []string) []oci.DirPlaceholder {
	return oci.RuntimeDirectories(c.From, dirs)
}

type BuildRoot struct {
	path     string
	drive    *Drive
	id       string
	profile  environment.Profile
	pathDirs []string
	toolVars []environment.Var
	msvc     *MSVCToolchain
	caches   []string
	work     *Work
}

func initBuildRoot(ctx *context.Context, r *recipe.Recipe, tools []Tool) (*BuildRoot, error) {
	path, err := ctx.Store.MakeTmpDir(DirPrefix[:len(DirPrefix)-1])
	if err != nil {
		return nil, err
	}

	for _, sub := range []string{"src", "prefix", "cache"} {
		if err := os.MkdirAll(filepath.Join(path, sub), 0755); err != nil {
			_ = os.RemoveAll(path)
			return nil, err
		}
	}
	if err := makeScratchDirs(path, false); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}

	profile := environment.NewProfile(DriveLetter + `\`)

	var pathDirs []string
	rootsFile := ""
	for _, t := range tools {
		if oci.UsesRoots(t.Env, nil) {
			rf, err := roots.File(ctx.Store)
			if err == nil {
				rootsFile = rf
				break
			}
		}
	}

	toolVars, err := computeToolVars(tools, rootsFile)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}

	var caches []string
	for _, t := range tools {
		p := t.Placeholders(rootsFile)
		if p.Cache != "" && !containsFold(caches, t.ID) {
			caches = append(caches, t.ID)
		}
		pDirs, err := environment.PathDirs(p, t.Path)
		if err == nil {
			pathDirs = append(pathDirs, pDirs...)
		}
	}

	pathDirs = append([]string{DriveLetter + `\prefix\bin`}, pathDirs...)

	setSearchPaths(&toolVars, tools)

	var msvc *MSVCToolchain
	if len(r.Host) > 0 {
		tc, err := FindMSVC(ctx, path)
		if err != nil {
			_ = os.RemoveAll(path)
			return nil, err
		}
		msvc = tc
		pathDirs = append(pathDirs, tc.Path...)
		for _, v := range tc.Vars {
			environment.Set(&toolVars, v.Name, v.Value)
		}
	}

	drv, err := AcquireDrive(ctx, path)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}

	br := &BuildRoot{
		path:     path,
		drive:    drv,
		id:       r.ID,
		profile:  profile,
		pathDirs: pathDirs,
		toolVars: toolVars,
		msvc:     msvc,
		caches:   caches,
	}

	for _, cid := range caches {
		br.takeCache(ctx, cid)
	}

	return br, nil
}

func computeToolVars(tools []Tool, rootsFile string) ([]environment.Var, error) {
	var vars []environment.Var
	i := 0
	for i < len(tools) {
		t := tools[i]
		for _, rt := range tools[i+1 : i+1+t.Brings] {
			if err := environment.Expand(&vars, rt.Placeholders(rootsFile), rt.Env); err != nil {
				return nil, err
			}
		}
		if err := environment.Expand(&vars, t.Placeholders(rootsFile), t.Env); err != nil {
			return nil, err
		}
		i += 1 + t.Brings
	}
	return vars, nil
}

func setSearchPaths(vars *[]environment.Var, tools []Tool) {
	pkgConfig := []string{DriveLetter + `\prefix\lib\pkgconfig`, DriveLetter + `\prefix\share\pkgconfig`}
	cmake := []string{DriveLetter + `\prefix`}

	for _, t := range tools {
		any := false
		for _, sub := range []string{`lib\pkgconfig`, `share\pkgconfig`} {
			dir := filepath.Join(t.Dir, sub)
			if store.Exists(dir) {
				pkgConfig = append(pkgConfig, dir)
				any = true
			}
		}
		if any {
			cmake = append(cmake, t.Dir)
		}
	}
	environment.Set(vars, "PKG_CONFIG_PATH", strings.Join(pkgConfig, ";"))
	environment.Set(vars, "CMAKE_PREFIX_PATH", strings.Join(cmake, ";"))
}

func (b *BuildRoot) takeCache(ctx *context.Context, id string) {
	kept := ctx.Store.Path("cache", "tools", id)
	here := filepath.Join(b.path, "cache", id)
	if store.Exists(kept) {
		if err := store.RenameRetrying(kept, here, 5000); err != nil && ctx.Verbose {
			ctx.Note("couldn't use %s's cache (%v); building without it", id, err)
		}
	}
	_ = os.MkdirAll(here, 0755)
}

func (b *BuildRoot) Finish(ctx *context.Context, keep bool) {
	b.drive.Release()
	for _, id := range b.caches {
		b.keepCache(ctx, id)
	}
	if b.work != nil && !keep {
		for _, mName := range b.work.taken {
			_ = b.work.put(b.path, mName)
		}
	}
	if keep {
		ctx.Note("kept the build directory: %s", b.path)
		if b.work != nil {
			ctx.Note("its work directories stay there; the project's next build starts afresh")
		}
	} else {
		_ = os.RemoveAll(b.path)
	}
}

func (b *BuildRoot) keepCache(ctx *context.Context, id string) {
	here := filepath.Join(b.path, "cache", id)
	kept := ctx.Store.Path("cache", "tools", id)
	_ = os.MkdirAll(filepath.Dir(kept), 0755)
	_ = store.RenameRetrying(here, kept, 5000)
}

func (b *BuildRoot) buildStages(ctx *context.Context, sources *Sources, r *recipe.Recipe, recipePath string) (*tree.Tree, error) {
	prefix := filepath.Join(b.path, "prefix")
	for _, m := range r.Stages {
		sources.prefix = prefix
		if len(m.Build) > 0 {
			sources.prefix = filepath.Join(b.path, "src", m.Name)
		}
		t := tree.New()
		if err := sources.Add(t, m, recipePath); err != nil {
			return nil, err
		}
		if m.Vendor == nil {
			if b.work != nil && keepsWork(m, sources.projects) {
				if err := b.work.take(ctx, b.path, t, m); err != nil {
					return nil, err
				}
			} else {
				dest := prefix
				if len(m.Build) > 0 {
					dest = filepath.Join(b.path, "src", m.Name)
				}
				if err := writeSourcesToDisk(t, dest); err != nil {
					return nil, fmt.Errorf("stage %s: unpacking sources: %w", m.Name, err)
				}
			}
			if err := sources.projects.Recheck(ctx, m.Name); err != nil {
				return nil, err
			}
		}
		if len(m.Build) > 0 {
			if err := b.runCommands(ctx, m, stepBuild, false); err != nil {
				return nil, err
			}
		}
	}
	return buildcontext.ReadOutput(prefix)
}

func writeSourcesToDisk(t *tree.Tree, dest string) error {
	_ = os.MkdirAll(dest, 0755)
	return t.WriteFiles(dest)
}

const (
	stepBuild  = "build"
	stepVendor = "vendor"
)

func (b *BuildRoot) vendor(ctx *context.Context, m recipe.Stage, recipePath string, projects *Snapshots, image string) (string, error) {
	v := m.Vendor
	dest := filepath.Join(b.path, "src", m.Name)
	if err := unpackSources(ctx, m, recipePath, projects, dest); err != nil {
		return "", err
	}

	var cached string
	if v.Sha256 != nil {
		c := ctx.Store.Path("cache", "downloads", *v.Sha256)
		if store.Exists(c) {
			cached = c
			if ctx.Verbose {
				ctx.Note("stage %s: vendored files from the cache (sha256 %s)", m.Name, *v.Sha256)
			}
		}
	}

	if cached == "" {
		err := b.runCommands(ctx, m, stepVendor, v.Sha256 != nil)
		if err == nil {
			made := filepath.Join(dest, v.Dir)
			if !store.Exists(made) {
				return "", fmt.Errorf("module %s: the vendor commands left nothing in %s", m.Name, v.Dir)
			}
			t, err := tree.FromDir(made)
			if err != nil {
				return "", err
			}
			layerTmp := ctx.Store.TmpPath("vendor-layer")
			defer func() { _ = os.Remove(layerTmp) }()
			blob, err := format.WriteLayer(layerTmp, "", format.FormatTar, t.WriteTar, nil)
			if err != nil {
				return "", err
			}
			got := oci.DigestHex(blob.Digest)
			kept := ctx.Store.Path("cache", "downloads", got)
			_ = os.MkdirAll(filepath.Dir(kept), 0755)
			_ = store.RenameRetrying(blob.Path, kept, 5000)

			if v.Sha256 == nil {
				return "", fmt.Errorf("module %s: \"vendor\" has no sha256. Pin what its commands made with:\n  \"sha256\": %q", m.Name, got)
			}
			if !strings.EqualFold(*v.Sha256, got) {
				return "", fmt.Errorf("sha256 mismatch for what module %s's vendor commands made in %s\n  expected %s\n  got      %s", m.Name, v.Dir, *v.Sha256, got)
			}
			_ = os.RemoveAll(dest)
			if err := unpackSources(ctx, m, recipePath, projects, dest); err != nil {
				return "", err
			}
			_ = makeScratchDirs(b.path, true)
			cached = kept
		} else {
			if v.Sha256 == nil {
				return "", err
			}
			ctx.Note("module %s's vendored files: its vendor commands failed; fetching sha256 %.12s", m.Name, *v.Sha256)
			var fallbackErr error
			cached, fallbackErr = remote.FetchSource(ctx.Store, ctx.Env[remote.RegistryVar], image, *v.Sha256, ctx.Verbose)
			if fallbackErr != nil {
				return "", fmt.Errorf("%w; vendor source fallback: %w", err, fallbackErr)
			}
			// A failed command can leave modified inputs behind. Restore the
			// original stage before applying the recovered vendor archive.
			rel, pathErr := filepath.Rel(b.path, dest)
			if pathErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				return "", fmt.Errorf("vendor stage is outside build root: %s", dest)
			}
			if err := os.RemoveAll(dest); err != nil {
				return "", err
			}
			if err := unpackSources(ctx, m, recipePath, projects, dest); err != nil {
				return "", err
			}
			if err := makeScratchDirs(b.path, true); err != nil {
				return "", err
			}
		}
	}

	t := tree.New()
	f, err := os.Open(cached)
	if err != nil {
		return "", fmt.Errorf("module %s: reading its vendored files (%s): %w", m.Name, cached, err)
	}
	defer func() { _ = f.Close() }()
	fi, _ := f.Stat()
	entries, err := tree.ListTar(f, fi.Size())
	if err != nil {
		return "", err
	}
	if err := t.AddTarEntries(f, entries, v.Dir, 0); err != nil {
		return "", err
	}
	if err := writeSourcesToDisk(t, dest); err != nil {
		return "", err
	}
	if err := projects.Recheck(ctx, m.Name); err != nil {
		return "", err
	}
	return *v.Sha256, nil
}

func unpackSources(ctx *context.Context, m recipe.Stage, recipePath string, projects *Snapshots, dest string) error {
	sources := NewSources(ctx, filepath.Dir(recipePath), "", projects)
	defer sources.Close()
	t := tree.New()
	if err := sources.Add(t, m, recipePath); err != nil {
		return err
	}
	return writeSourcesToDisk(t, dest)
}

func makeScratchDirs(path string, fresh bool) error {
	profile := environment.NewProfile(path)
	data := filepath.Join(path, "data")
	if fresh {
		_ = os.RemoveAll(profile.Home)
		_ = os.RemoveAll(data)
	}
	for _, dir := range []string{profile.Roaming, profile.Temp, data} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return nil
}

func (b *BuildRoot) runCommands(ctx *context.Context, m recipe.Stage, step string, warnOnly bool) error {
	systemRoot := ""
	if ctx != nil && ctx.Env != nil {
		systemRoot = ctx.Env["SystemRoot"]
	}
	if systemRoot == "" {
		systemRoot = os.Getenv("SystemRoot")
	}
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}

	commands := m.Build
	if step == stepVendor && m.Vendor != nil {
		commands = m.Vendor.Commands
	}

	vars := append([]environment.Var(nil), b.toolVars...)
	environment.Set(&vars, "PREFIX", DriveLetter+`\prefix`)
	environment.Set(&vars, "SOURCE_DATE_EPOCH", sourceDateEpoch)
	environment.Set(&vars, "QUASIDOCK_BUILD", "1")
	environment.Set(&vars, "ZIG_LOCAL_CACHE_DIR", fmt.Sprintf(`%s\src\%s\.zig-cache`, DriveLetter, m.Name))
	environment.Set(&vars, "ZIG_PROGRESS", "off")

	if step == stepBuild && !m.Network {
		for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
			environment.Set(&vars, name, "http://127.0.0.1:9")
		}
		environment.Set(&vars, "NO_PROXY", "")
	}

	if err := environment.Expand(&vars, oci.Placeholders{App: DriveLetter + `\prefix`, Data: ""}, m.Env); err != nil {
		return err
	}
	envBuilt, err := environment.Build(ctx.Env, environment.Spec{
		ID:         b.id,
		Profile:    b.profile,
		Path:       b.pathDirs,
		SystemRoot: systemRoot,
		Vars:       vars,
	})
	if err != nil {
		return err
	}
	envBlock, err := environment.EncodeBlock(envBuilt)
	if err != nil {
		return err
	}

	sh, err := b.findShell(ctx, m, systemRoot)
	if err != nil {
		return err
	}
	cwd := fmt.Sprintf(`%s\src\%s`, DriveLetter, m.Name)
	label := ""
	if step == stepVendor {
		label = " vendor"
	}

	for n, command := range commands {
		ctx.Note("[%s%s %d/%d] %s", m.Name, label, n+1, len(commands), command)
		start := ctx.Now()
		code, err := process.Spawn(process.SpawnSpec{
			Exe:         sh.exe,
			CommandLine: sh.CommandLine(command),
			Cwd:         cwd,
			EnvBlock:    envBlock,
		})
		if err != nil {
			return err
		}
		took := ctx.Since(start)
		if took.Milliseconds() >= 1000 {
			ctx.Note("[%s%s %d/%d] took %s", m.Name, label, n+1, len(commands), took)
		}
		if code != 0 {
			if warnOnly {
				ctx.Note("warning: module %s: %s command %d exited with code %d", m.Name, step, n+1, code)
				return fmt.Errorf("command failed with code %d", code)
			}
			return fmt.Errorf("module %s: %s command %d exited with code %d:\n  %s", m.Name, step, n+1, code, command)
		}
	}
	return nil
}

type ShellKind string

const (
	ShellCmd     ShellKind = "cmd"
	ShellSh      ShellKind = "sh"
	ShellBusybox ShellKind = "busybox"
)

type Shell struct {
	exe  string
	kind ShellKind
}

func (s Shell) CommandLine(command string) string {
	switch s.kind {
	case ShellSh:
		return process.BuildCommandLine(s.exe, []string{"-c", command})
	case ShellBusybox:
		return process.BuildCommandLine(s.exe, []string{"sh", "-c", command})
	default:
		var out strings.Builder
		process.AppendQuoted(&out, s.exe)
		fmt.Fprintf(&out, ` /d /s /c "%s"`, command)
		return out.String()
	}
}

func (b *BuildRoot) findShell(ctx *context.Context, m recipe.Stage, systemRoot string) (*Shell, error) {
	if m.Shell == recipe.ShellCmd {
		return &Shell{exe: filepath.Join(systemRoot, `System32`, `cmd.exe`), kind: ShellCmd}, nil
	}
	for _, candidate := range []struct {
		name string
		kind ShellKind
	}{
		{"sh.exe", ShellSh},
		{"busybox.exe", ShellBusybox},
	} {
		for _, dir := range b.pathDirs {
			exe := filepath.Join(dir, candidate.name)
			if store.Exists(exe) {
				return &Shell{exe: exe, kind: candidate.kind}, nil
			}
		}
	}
	return nil, fmt.Errorf("stage %s: its build commands run in sh, which none of the recipe's sdk or base images provides; add busybox to \"sdk\"", m.Name)
}

func containsFold(list []string, item string) bool {
	for _, x := range list {
		if strings.EqualFold(x, item) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Work directory management
// ---------------------------------------------------------------------------

type Work struct {
	dir   string
	stamp string
	taken []string
}

const stateHeader = "quasidock work 1 "

func wantedWork(r *recipe.Recipe, projects *Snapshots) bool {
	for _, m := range r.Stages {
		if keepsWork(m, projects) {
			return true
		}
	}
	return false
}

func keepsWork(m recipe.Stage, projects *Snapshots) bool {
	if len(m.Build) == 0 || m.Vendor != nil {
		return false
	}
	for i := range m.Sources {
		if projects.Get(m.Name, i) != nil {
			return true
		}
	}
	return false
}

func initWork(ctx *context.Context, r *recipe.Recipe, bytesRead []byte, recipePath, source string, projects *Snapshots) (*Work, error) {
	h := sha256.New()
	h.Write([]byte(strings.ToLower(source)))
	h.Write([]byte("\n"))
	h.Write([]byte(strings.ToLower(r.ID)))
	key := hex.EncodeToString(h.Sum(nil))

	stamp, err := inputHash(ctx, r, bytesRead, recipePath, projects, false)
	if err != nil {
		return nil, err
	}
	return &Work{
		dir:   ctx.Store.Path("cache", "work", key[:16]),
		stamp: stamp,
	}, nil
}

func (w *Work) drop() error {
	if store.Exists(w.dir) {
		return os.RemoveAll(w.dir)
	}
	return nil
}

func (w *Work) take(ctx *context.Context, root string, t *tree.Tree, m recipe.Stage) error {
	kept := filepath.Join(w.dir, m.Name)
	statePath := kept + ".state"
	dest := filepath.Join(root, "src", m.Name)

	var previous []string
	needAfresh := true

	if store.Exists(kept) {
		if stateData, err := os.ReadFile(statePath); err == nil {
			lines := strings.Split(string(stateData), "\n")
			if len(lines) > 0 && strings.HasPrefix(lines[0], stateHeader) && strings.TrimSpace(lines[0][len(stateHeader):]) == w.stamp {
				for _, line := range lines[1:] {
					line = strings.TrimSpace(line)
					if line != "" {
						previous = append(previous, line)
					}
				}
				if err := store.RenameRetrying(kept, dest, 5000); err == nil {
					needAfresh = false
				}
			}
		}
	}

	if needAfresh {
		previous = nil
		_ = os.RemoveAll(kept)
	}

	w.taken = append(w.taken, m.Name)
	_ = os.MkdirAll(dest, 0755)

	synced, err := t.SyncFiles(dest, previous)
	if err != nil {
		return fmt.Errorf("module %s: bringing its sources up to date in %s: %w", m.Name, dest, err)
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%s%s\n", stateHeader, w.stamp)
	for _, f := range synced.Files {
		out.WriteString(f)
		out.WriteString("\n")
	}
	_ = os.MkdirAll(w.dir, 0755)
	_ = os.WriteFile(statePath, []byte(out.String()), 0644)
	return nil
}

func (w *Work) put(root, module string) error {
	here := filepath.Join(root, "src", module)
	kept := filepath.Join(w.dir, module)
	_ = os.MkdirAll(w.dir, 0755)
	return store.RenameRetrying(here, kept, 5000)
}
