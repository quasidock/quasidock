package exports

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/associations"
	"github.com/zigsaw-io/zigsaw/internal/context"
	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/shortcuts"
	"github.com/zigsaw-io/zigsaw/internal/sidecar"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

type ShimKind = sidecar.ShimKind

const (
	ShimConsole = sidecar.ShimConsole
	ShimGUI     = sidecar.ShimGUI
)

var (
	ShimKindFromExe = sidecar.ShimKindFromExe
	ShimExe         = sidecar.ShimExe
	WriteShimIn     = sidecar.WriteShimIn
)

type Shim struct {
	Name    string
	Sidecar sidecar.Sidecar
}

type Found struct {
	Name string
	Path string
}

type whenKind int

const (
	whenInstall whenKind = iota
	whenRun
	whenRepair
)

func binDir(ctx *context.Context) string {
	return ctx.Store.Path("bin")
}

func List(ctx *context.Context) ([]Shim, error) {
	bin := binDir(ctx)
	entries, err := os.ReadDir(bin)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var shims []Shim
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), sidecar.Extension) {
			continue
		}
		bytes, err := os.ReadFile(filepath.Join(bin, entry.Name()))
		if err != nil {
			continue
		}
		sc, err := sidecar.Parse(bytes)
		if err != nil {
			continue
		}
		name := entry.Name()[:len(entry.Name())-len(sidecar.Extension)]
		shims = append(shims, Shim{
			Name:    name,
			Sidecar: *sc,
		})
	}
	sort.Slice(shims, func(i, j int) bool {
		return strings.ToLower(shims[i].Name) < strings.ToLower(shims[j].Name)
	})
	return shims, nil
}

func Sync(ctx *context.Context, manifest oci.Manifest, config oci.AppConfig) error {
	return syncAs(ctx, manifest, config, whenInstall)
}

func syncAs(ctx *context.Context, manifest oci.Manifest, config oci.AppConfig, when whenKind) error {
	lock, err := ctx.Store.LockShims()
	if err != nil {
		return err
	}
	defer lock.Release()

	if err := syncLocked(ctx, manifest, config, when); err != nil {
		return err
	}
	dataDir := ctx.Store.Path("data", config.ID)
	placeholders, err := deployedPlaceholders(ctx, manifest, config, dataDir)
	if err != nil {
		return err
	}
	if err := shortcuts.Sync(ctx, config, placeholders); err != nil {
		return err
	}
	return associations.Sync(ctx, config, placeholders)
}

func Repair(ctx *context.Context) (uint32, error) {
	refs, err := ctx.Store.ListRefs()
	if err != nil {
		return 1, err
	}
	failed := 0
	for _, ref := range refs {
		img, err := ctx.Store.ReadImage(ref.ID, ref.Manifest)
		if err != nil {
			failed++
			continue
		}
		if err := syncAs(ctx, img.Manifest, img.Config, whenRepair); err != nil {
			failed++
		}
	}
	if len(refs) == 0 {
		ctx.Note("no apps are installed")
	}
	_ = associations.Repair(ctx)
	if failed == 0 {
		return 0, nil
	}
	ctx.Note("%d app(s) couldn't be repaired; close them and run `zigsaw repair` again", failed)
	return 1, nil
}

func SyncAfterRun(ctx *context.Context, id string) {
	start := ctx.Now()
	lock, err := ctx.Store.LockShims()
	if err != nil {
		ctx.Note("warning: couldn't update %s's commands: %v", id, err)
		return
	}
	defer lock.Release()

	ref, err := ctx.Store.ReadRef(id)
	if err != nil || ref == nil {
		return
	}
	img, err := ctx.Store.ReadImage(id, ref.Manifest)
	if err != nil {
		return
	}
	if err := syncLocked(ctx, img.Manifest, img.Config, whenRun); err != nil {
		ctx.Note("warning: couldn't update %s's commands: %v", id, err)
	}
	ctx.Timed(start, "sync commands")
}

func syncLocked(ctx *context.Context, manifest oci.Manifest, config oci.AppConfig, when whenKind) error {
	installed, err := List(ctx)
	if err != nil {
		return err
	}
	zigsawExe, _ := win32.GetModuleFileName()
	dataDir := ctx.Store.Path("data", config.ID)
	cmdDirs := CommandDirs(config, dataDir)
	found, _ := FindCommands(cmdDirs)
	placeholders, err := deployedPlaceholders(ctx, manifest, config, dataDir)
	if err != nil {
		return err
	}

	var exported []string
	var fromRuns []string
	var added []string
	shimExes := make(map[ShimKind][]byte)

	var exportKeys []string
	for k := range config.Exports {
		exportKeys = append(exportKeys, k)
	}
	sort.Strings(exportKeys)
	exportCount := len(exportKeys)

	for i := 0; i < exportCount+len(found); i++ {
		isExport := i < exportCount
		var name string
		if isExport {
			name = exportKeys[i]
		} else {
			name = found[i-exportCount].Name
		}
		if !isExport && exportsName(config, name) {
			continue
		}
		owner := findShim(installed, name)
		if owner != nil && !strings.EqualFold(owner.Sidecar.App, config.ID) {
			switch when {
			case whenInstall:
				ctx.Note("warning: not exporting %s: %s already provides it", name, owner.Sidecar.App)
			case whenRun, whenRepair:
				if ctx.Verbose {
					ctx.Note("not adding %s: %s already provides it", name, owner.Sidecar.App)
				}
			}
			continue
		}

		var target string
		if isExport {
			target = placeholders.CommandPath(config.Exports[name].Command)
		} else {
			target = found[i-exportCount].Path
		}
		kind := sidecar.ShimKindFromExe(target)
		sc := sidecar.Sidecar{
			Zigsaw:  zigsawExe,
			Home:    ctx.Store.Root,
			App:     config.ID,
			Command: name,
			GUI:     kind == sidecar.ShimGUI,
		}
		if isExport {
			exported = append(exported, name)
		} else {
			fromRuns = append(fromRuns, name)
		}

		if when == whenRun && owner != nil && owner.Sidecar.Equal(&sc) {
			continue
		}
		shimExe, ok := shimExes[kind]
		if !ok {
			shimExe, err = sidecar.ShimExe(zigsawExe, kind)
			if err != nil {
				return err
			}
			shimExes[kind] = shimExe
		}

		bin := binDir(ctx)
		err = sidecar.WriteShimIn(bin, name, shimExe, sc)
		if err != nil {
			switch when {
			case whenInstall, whenRepair:
				return ctx.Fail("writing the shim for %s in %s (is it running?): %v", name, bin, err)
			case whenRun:
				ctx.Note("warning: couldn't add %s to %s: %v", name, bin, err)
				continue
			}
		}
		if owner == nil {
			added = append(added, name)
		}
	}

	var removed []string
	for _, shim := range installed {
		if !strings.EqualFold(shim.Sidecar.App, config.ID) {
			continue
		}
		if containsIgnoreCase(exported, shim.Name) || containsIgnoreCase(fromRuns, shim.Name) {
			continue
		}
		err := removeShim(ctx, shim.Name)
		if err != nil {
			switch when {
			case whenInstall, whenRepair:
				return ctx.Fail("removing the shim for %s from %s (is it running?): %v", shim.Name, binDir(ctx), err)
			case whenRun:
				ctx.Note("warning: couldn't remove %s from %s: %v", shim.Name, binDir(ctx), err)
				continue
			}
		}
		removed = append(removed, shim.Name)
	}

	bin := binDir(ctx)
	switch when {
	case whenInstall:
		if len(exported) > 0 {
			ctx.Note("  exports  %s", strings.Join(exported, ", "))
		}
		if len(fromRuns) > 0 {
			ctx.Note("  commands %s (installed by its runs)", strings.Join(fromRuns, ", "))
		}
		if len(exported)+len(fromRuns) > 0 {
			pathHint(ctx, bin)
		}
	case whenRun:
		if len(added) > 0 {
			ctx.Note("added %s to %s (installed by %s)", strings.Join(added, ", "), bin, config.ID)
			pathHint(ctx, bin)
		}
		if len(removed) > 0 {
			ctx.Note("removed %s from %s (gone from %s)", strings.Join(removed, ", "), bin, config.ID)
		}
	case whenRepair:
		names := append(exported, fromRuns...)
		desc := "no commands"
		if len(names) > 0 {
			desc = strings.Join(names, ", ")
		}
		ctx.Note("repaired %s: %s", config.ID, desc)
	}
	return nil
}

func RemoveAll(ctx *context.Context, app string) ([]string, error) {
	var removed []string
	installed, err := List(ctx)
	if err != nil {
		return nil, err
	}
	bin := binDir(ctx)
	for _, shim := range installed {
		if !strings.EqualFold(shim.Sidecar.App, app) {
			continue
		}
		if err := removeShim(ctx, shim.Name); err != nil {
			return nil, ctx.Fail("removing the shim for %s from %s (is it running?): %v", shim.Name, bin, err)
		}
		removed = append(removed, shim.Name)
	}
	return removed, nil
}

func removeShim(ctx *context.Context, name string) error {
	bin := binDir(ctx)
	for _, ext := range []string{".exe", sidecar.Extension} {
		p := filepath.Join(bin, name+ext)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func findShim(shims []Shim, name string) *Shim {
	for i := range shims {
		if strings.EqualFold(shims[i].Name, name) {
			return &shims[i]
		}
	}
	return nil
}

func exportsName(config oci.AppConfig, name string) bool {
	for k := range config.Exports {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

func pathHint(ctx *context.Context, bin string) {
	if OnPath(ctx.Env["PATH"], bin) {
		return
	}
	ctx.Note(`
%s isn't on your PATH, so these commands won't be found by name yet.
To add it for your user, run this in PowerShell and open a new terminal:
  [Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path', 'User') + ';%s', 'User')`, bin, bin)
}

func OnPath(pathVar, dir string) bool {
	want := strings.TrimRight(dir, `\/`)
	for _, entry := range strings.Split(pathVar, ";") {
		got := strings.TrimRight(strings.Trim(entry, ` "`), `\/`)
		if strings.EqualFold(got, want) {
			return true
		}
	}
	return false
}

func HasCommandDirs(config oci.AppConfig) bool {
	for _, p := range config.Path {
		if isDataEntry(p) {
			return true
		}
	}
	for _, r := range config.Runtimes {
		for _, p := range r.Path {
			if isDataEntry(p) {
				return true
			}
		}
	}
	return false
}

func isDataEntry(entry string) bool {
	return strings.HasPrefix(entry, "${data}")
}

func CommandDirs(config oci.AppConfig, dataDir string) []string {
	p := oci.Placeholders{App: "", Data: dataDir}
	var dirs []string
	allEntries := [][]string{config.Path}
	for _, r := range config.Runtimes {
		allEntries = append(allEntries, r.Path)
	}
	for _, entries := range allEntries {
		for _, entry := range entries {
			if !isDataEntry(entry) {
				continue
			}
			dir := p.Expand(entry)
			dir = strings.ReplaceAll(dir, "/", "\\")
			if !containsIgnoreCase(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs
}

func FindCommands(dirs []string) ([]Found, error) {
	var found []Found
	for _, dirPath := range dirs {
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		before := len(found)
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := commandName(entry.Name())
			if name == "" || findName(found, name) {
				continue
			}
			found = append(found, Found{
				Name: name,
				Path: filepath.Join(dirPath, entry.Name()),
			})
		}
		sort.Slice(found[before:], func(i, j int) bool {
			return strings.ToLower(found[before+i].Name) < strings.ToLower(found[before+j].Name)
		})
	}
	return found, nil
}

func commandName(fileName string) string {
	ext := filepath.Ext(fileName)
	for _, e := range []string{".exe", ".com", ".cmd", ".bat"} {
		if strings.EqualFold(ext, e) {
			name := fileName[:len(fileName)-len(ext)]
			if oci.IsValidExportName(name) {
				return name
			}
			return ""
		}
	}
	return ""
}

func findName(found []Found, name string) bool {
	for _, f := range found {
		if strings.EqualFold(f.Name, name) {
			return true
		}
	}
	return false
}

func deployedPlaceholders(ctx *context.Context, manifest oci.Manifest, config oci.AppConfig, dataDir string) (oci.Placeholders, error) {
	layers := manifest.Layers
	runtimes := make([]oci.DirPlaceholder, 0, len(config.Runtimes))
	idx := 0
	for alias := range config.Runtimes {
		if idx < len(layers) {
			depPath, err := ctx.Store.DeployPath(layers[idx].Digest)
			if err != nil {
				return oci.Placeholders{}, err
			}
			runtimes = append(runtimes, oci.DirPlaceholder{Alias: alias, Path: depPath})
			idx++
		}
	}
	ownLayer := oci.OwnLayer(&manifest)
	appPath, err := ctx.Store.DeployPath(ownLayer.Digest)
	if err != nil {
		return oci.Placeholders{}, err
	}
	return oci.Placeholders{
		App:      appPath,
		Data:     dataDir,
		Runtimes: runtimes,
	}, nil
}

func containsIgnoreCase(list []string, name string) bool {
	for _, n := range list {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}
