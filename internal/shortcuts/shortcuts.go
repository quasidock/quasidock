package shortcuts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/zigsaw-io/zigsaw/internal/context"
	"github.com/zigsaw-io/zigsaw/internal/environment"
	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/sidecar"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

const Extension = ".lnk"

// Dir returns the shortcuts folder for the store, or empty string if none.
func Dir(ctx *context.Context) (string, error) {
	ch := choose(ctx.Env["ZIGSAW_SHORTCUTS_DIR"], ctx.Env["LOCALAPPDATA"], ctx.Store.Root)
	switch ch.kind {
	case choiceNone:
		return "", nil
	case choiceOverride:
		return ch.dir, nil
	case choiceStartMenu:
		programs, err := win32.SHGetKnownFolderPath(&win32.FOLDERID_Programs, 0, 0)
		if err != nil {
			return "", nil
		}
		return filepath.Join(programs, "Zigsaw"), nil
	default:
		return "", nil
	}
}

type choiceKind int

const (
	choiceNone choiceKind = iota
	choiceOverride
	choiceStartMenu
)

type choice struct {
	kind choiceKind
	dir  string
}

func choose(override, localAppData, root string) choice {
	if override != "" {
		return choice{kind: choiceOverride, dir: override}
	}
	if IsDefaultStore(localAppData, root) {
		return choice{kind: choiceStartMenu}
	}
	return choice{kind: choiceNone}
}

func IsDefaultStore(localAppData, root string) bool {
	if localAppData == "" || root == "" {
		return false
	}
	want := strings.TrimRight(root, "\\/")
	base := strings.TrimRight(localAppData, "\\/")
	if len(want) != len(base)+len("\\zigsaw") {
		return false
	}
	if !strings.EqualFold(want[:len(base)], base) {
		return false
	}
	return strings.EqualFold(want[len(base):], "\\zigsaw")
}

type Owned struct {
	Name string
	App  string
}

func ListOwned(ctx *context.Context, com *comContext, folder string) ([]Owned, error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	bin := ctx.Store.Path("bin")
	var owned []Owned
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), Extension) {
			continue
		}
		lnk := filepath.Join(folder, entry.Name())
		target, err := com.target(lnk)
		if err != nil {
			continue
		}
		shim := ShimName(target, bin)
		if shim == "" {
			continue
		}
		sidecarPath := filepath.Join(bin, shim+sidecar.Extension)
		app := ""
		if b, err := os.ReadFile(sidecarPath); err == nil {
			if sc, err := sidecar.Parse(b); err == nil {
				app = sc.App
			}
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		owned = append(owned, Owned{
			Name: name,
			App:  app,
		})
	}
	return owned, nil
}

func ShimName(target, bin string) string {
	parent := filepath.Dir(target)
	if !strings.EqualFold(strings.TrimRight(parent, "\\/"), strings.TrimRight(bin, "\\/")) {
		return ""
	}
	base := filepath.Base(target)
	if !strings.EqualFold(filepath.Ext(base), ".exe") {
		return ""
	}
	return base[:len(base)-4]
}

func Sync(ctx *context.Context, config oci.AppConfig, placeholders oci.Placeholders) error {
	declared := config.Shortcuts
	if declared == nil {
		declared = map[string]oci.Shortcut{}
	}
	folder, err := Dir(ctx)
	if err != nil {
		return err
	}
	if folder == "" {
		if len(declared) > 0 {
			ctx.Note("  shortcut none: only the default store adds to the Start menu (or set ZIGSAW_SHORTCUTS_DIR)")
		}
		return nil
	}

	com, err := initCom()
	if err != nil {
		return err
	}
	defer com.deinit()

	owned, err := ListOwned(ctx, com, folder)
	if err != nil {
		return err
	}
	bin := ctx.Store.Path("bin")

	var written []string
	for name, shortcut := range declared {
		sidecarPath := filepath.Join(bin, shortcut.Command+sidecar.Extension)
		var owner string
		if b, err := os.ReadFile(sidecarPath); err == nil {
			if sc, err := sidecar.Parse(b); err == nil {
				owner = sc.App
			}
		}
		if owner != config.ID {
			who := owner
			if who == "" {
				who = "no app"
			}
			ctx.Note("warning: no shortcut %q: %s runs %s, which %s provides", name, name, shortcut.Command, who)
			continue
		}
		lnk := filepath.Join(folder, name+Extension)
		if o := findOwned(owned, name); o != nil {
			if o.App != "" && !strings.EqualFold(o.App, config.ID) {
				ctx.Note("warning: no shortcut %q: %s has one by that name", name, o.App)
				continue
			}
		} else if _, err := os.Stat(lnk); err == nil {
			ctx.Note("warning: no shortcut %q: %s is there already, and isn't zigsaw's", name, lnk)
			continue
		}

		exportDef, ok := config.Exports[shortcut.Command]
		if !ok {
			continue
		}
		exportCmd := placeholders.CommandPath(exportDef.Command)
		icon := exportCmd
		if shortcut.Icon != nil && *shortcut.Icon != "" {
			icon = placeholders.CommandPath(*shortcut.Icon)
		}
		desc := ""
		if shortcut.Description != nil {
			desc = *shortcut.Description
		}
		home := environment.NewProfile(placeholders.Data).Home
		_ = os.MkdirAll(home, 0o755)
		_ = os.MkdirAll(folder, 0o755)

		err = com.write(lnk, linkData{
			target:      filepath.Join(bin, shortcut.Command+".exe"),
			workingDir:  home,
			description: desc,
			icon:        icon,
		})
		if err != nil {
			return err
		}
		written = append(written, name)
	}

	var removed []string
	for _, o := range owned {
		if o.App == "" {
			_ = deleteLnk(folder, o.Name)
			continue
		}
		if !strings.EqualFold(o.App, config.ID) || declaredName(declared, o.Name) {
			continue
		}
		_ = deleteLnk(folder, o.Name)
		removed = append(removed, o.Name)
	}

	if len(written) > 0 {
		ctx.Note("  shortcut %s (in %s)", strings.Join(written, ", "), folder)
	}
	if len(removed) > 0 {
		ctx.Note("  removed shortcut %s", strings.Join(removed, ", "))
	}
	return nil
}

func RemoveAll(ctx *context.Context, app string) ([]string, error) {
	folder, err := Dir(ctx)
	if err != nil || folder == "" {
		return nil, err
	}
	com, err := initCom()
	if err != nil {
		return nil, err
	}
	defer com.deinit()

	var removed []string
	owned, err := ListOwned(ctx, com, folder)
	if err != nil {
		return nil, err
	}
	for _, o := range owned {
		if o.App == "" || !strings.EqualFold(o.App, app) {
			continue
		}
		_ = deleteLnk(folder, o.Name)
		removed = append(removed, o.Name)
	}
	return removed, nil
}

func Names(ctx *context.Context, app string) ([]string, error) {
	folder, err := Dir(ctx)
	if err != nil || folder == "" {
		return nil, err
	}
	com, err := initCom()
	if err != nil {
		return nil, err
	}
	defer com.deinit()

	var found []string
	owned, err := ListOwned(ctx, com, folder)
	if err != nil {
		return nil, err
	}
	for _, o := range owned {
		if strings.EqualFold(o.App, app) {
			found = append(found, o.Name)
		}
	}
	return found, nil
}

func deleteLnk(folder, name string) error {
	p := filepath.Join(folder, name+Extension)
	err := os.Remove(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(folder) // removes if empty
	return nil
}

func findOwned(owned []Owned, name string) *Owned {
	for i := range owned {
		if strings.EqualFold(owned[i].Name, name) {
			return &owned[i]
		}
	}
	return nil
}

func declaredName(declared map[string]oci.Shortcut, name string) bool {
	for k := range declared {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// COM IShellLinkW / IPersistFile
// ---------------------------------------------------------------------------

type comContext struct {
	initialized bool
}

func initCom() (*comContext, error) {
	hr := win32.CoInitializeEx(0, win32.COINIT_APARTMENTTHREADED)
	return &comContext{
		initialized: hr >= 0,
	}, nil
}

func (c *comContext) deinit() {
	if c.initialized {
		win32.CoUninitialize()
	}
}

type linkData struct {
	target      string
	workingDir  string
	description string
	icon        string
}

func createLinkObjects() (win32.IShellLinkW, win32.IPersistFile, error) {
	var obj uintptr
	hr := win32.CoCreateInstance(&win32.CLSID_ShellLink, 0, uint32(win32.CLSCTX_INPROC_SERVER), &win32.IID_IShellLinkW, &obj)
	if hr < 0 {
		return 0, 0, fmt.Errorf("CoCreateInstance ShellLink failed: 0x%x", uint32(hr))
	}
	link := win32.IShellLinkW(obj)

	var fileObj uintptr
	hr = link.QueryInterface(&win32.IID_IPersistFile, &fileObj)
	if hr < 0 {
		link.Release()
		return 0, 0, fmt.Errorf("QueryInterface IPersistFile failed: 0x%x", uint32(hr))
	}
	return link, win32.IPersistFile(fileObj), nil
}

func (c *comContext) write(path string, l linkData) error {
	link, file, err := createLinkObjects()
	if err != nil {
		return err
	}
	defer link.Release()
	defer file.Release()

	targetW, err := syscall.UTF16PtrFromString(l.target)
	if err == nil {
		_ = link.SetPath(targetW)
	}
	if l.workingDir != "" {
		wdW, err := syscall.UTF16PtrFromString(l.workingDir)
		if err == nil {
			_ = link.SetWorkingDirectory(wdW)
		}
	}
	if l.description != "" {
		descW, err := syscall.UTF16PtrFromString(l.description)
		if err == nil {
			_ = link.SetDescription(descW)
		}
	}
	if l.icon != "" {
		iconW, err := syscall.UTF16PtrFromString(l.icon)
		if err == nil {
			_ = link.SetIconLocation(iconW, 0)
		}
	}

	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	hr := file.Save(pathW, true)
	if hr < 0 {
		return fmt.Errorf("IPersistFile.Save failed: 0x%x", uint32(hr))
	}
	return nil
}

func (c *comContext) target(path string) (string, error) {
	link, file, err := createLinkObjects()
	if err != nil {
		return "", err
	}
	defer link.Release()
	defer file.Release()

	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	hr := file.Load(pathW, uint32(win32.STGM_READ))
	if hr < 0 {
		return "", fmt.Errorf("IPersistFile.Load failed: 0x%x", uint32(hr))
	}

	var buf [32 * 1024]uint16
	hr = link.GetPath(&buf[0], len(buf), 0, uint32(win32.SLGP_RAWPATH))
	if hr < 0 {
		return "", fmt.Errorf("IShellLinkW.GetPath failed: 0x%x", uint32(hr))
	}
	return syscall.UTF16ToString(buf[:]), nil
}
