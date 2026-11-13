package associations

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/context"
	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/regkey"
	"github.com/zigsaw-io/zigsaw/internal/shortcuts"
	"github.com/zigsaw-io/zigsaw/internal/sidecar"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

const (
	DefaultClasses = "Software\\Classes"
	Prefix         = "Zigsaw."
	StoreValue     = "ZigsawStore"
	AppValue       = "ZigsawApp"
	ExtensionValue = "ZigsawExtension"
	SchemeValue    = "ZigsawScheme"
	UrlsDir        = "urls"
)

func setStr(k *regkey.Key, name, value string) error {
	if name == "" {
		return k.SetString(nil, value)
	}
	return k.SetString(&name, value)
}

func getStr(k *regkey.Key, name string) (string, bool) {
	if k == nil {
		return "", false
	}
	s, err := k.GetString(name)
	if err != nil || s == nil {
		return "", false
	}
	return *s, true
}

func ClassesKey(ctx *context.Context) string {
	if k := ctx.Env["ZIGSAW_CLASSES_KEY"]; k != "" {
		return k
	}
	if shortcuts.IsDefaultStore(ctx.Env["LOCALAPPDATA"], ctx.Store.Root) {
		return DefaultClasses
	}
	return ""
}

func ProgID(app, ext string) string {
	return Prefix + app + "." + strings.TrimPrefix(ext, ".")
}

func SchemeProgID(app, scheme string) string {
	return Prefix + app + ".url." + scheme
}

type Owned struct {
	ProgID    string
	App       string
	Extension string
}

type OwnedScheme struct {
	ProgID string
	App    string
	Scheme string
}

func ShownAs(config oci.AppConfig, command string) (string, *string) {
	if config.Shortcuts != nil {
		for name, sc := range config.Shortcuts {
			if sc.Command == command {
				return name, sc.Icon
			}
		}
	}
	return command, nil
}

func Sync(ctx *context.Context, config oci.AppConfig, placeholders oci.Placeholders) error {
	declared := config.Associations
	if declared == nil {
		declared = map[string]oci.Association{}
	}
	declaredSchemes := config.Schemes
	if declaredSchemes == nil {
		declaredSchemes = map[string]oci.Scheme{}
	}
	opensAny := len(declared)+len(declaredSchemes) > 0
	dir := ctx.Store.OpenDir(config.ID)

	classesPath := ClassesKey(ctx)
	if classesPath == "" {
		if opensAny {
			ctx.Note("  opens    nothing: only the default store registers file types and links (or set ZIGSAW_CLASSES_KEY)")
		}
		return removeAllShims(dir)
	}

	var classes *regkey.Key
	if opensAny {
		c, err := regkey.Create(win32.HKEY_CURRENT_USER, classesPath)
		if err != nil {
			return err
		}
		classes = &c
	} else {
		var err error
		classes, err = regkey.Open(win32.HKEY_CURRENT_USER, classesPath, win32.KEY_READ|win32.KEY_WRITE)
		if err != nil || classes == nil {
			return removeAllShims(dir)
		}
	}
	defer classes.Close()

	owned, err := listOwned(ctx, classes)
	if err != nil {
		return err
	}
	ownedSchemes, err := listOwnedSchemes(ctx, classes)
	if err != nil {
		return err
	}

	zigsawExe, _ := win32.GetModuleFileName()
	var shims []string
	shimExes := make(map[sidecar.ShimKind][]byte)
	var written []string

	for ext, association := range declared {
		exportDef, ok := config.Exports[association.Command]
		if !ok {
			continue
		}
		target := placeholders.CommandPath(exportDef.Command)
		shownName, shownIcon := ShownAs(config, association.Command)

		if !containsIgnoreCase(shims, association.Command) {
			kind := sidecar.ShimKindFromExe(target)
			shimExe, ok := shimExes[kind]
			if !ok {
				var err error
				shimExe, err = sidecar.ShimExe(zigsawExe, kind)
				if err != nil {
					return err
				}
				shimExes[kind] = shimExe
			}
			_ = os.MkdirAll(dir, 0o755)
			sc := sidecar.Sidecar{
				Zigsaw:  zigsawExe,
				Home:    ctx.Store.Root,
				App:     config.ID,
				Command: association.Command,
				GUI:     kind == sidecar.ShimGUI,
				Files:   true,
			}
			if err := sidecar.WriteShimIn(dir, association.Command, shimExe, sc); err != nil {
				return ctx.Fail("writing the shim that opens %s files in %s (is it running?): %v", ext, dir, err)
			}
			shims = append(shims, association.Command)
		}

		icon := target
		if association.Icon != nil && *association.Icon != "" {
			icon = placeholders.CommandPath(*association.Icon)
		}
		appIcon := target
		if shownIcon != nil && *shownIcon != "" {
			appIcon = placeholders.CommandPath(*shownIcon)
		}

		desc := ""
		if association.Description != nil {
			desc = *association.Description
		}

		err = register(ctx, classes, registration{
			progID:      ProgID(config.ID, ext),
			app:         config.ID,
			extension:   ext,
			description: desc,
			icon:        icon,
			shim:        filepath.Join(dir, association.Command+".exe"),
			appName:     shownName,
			appIcon:     appIcon,
		})
		if err != nil {
			return err
		}
		written = append(written, ext)
	}

	var removed []string
	for _, o := range owned {
		if !strings.EqualFold(o.App, config.ID) || containsKey(declared, o.Extension) {
			continue
		}
		_ = unregister(classes, o)
		removed = append(removed, o.Extension)
	}
	_ = removeShimsExcept(dir, shims)

	urls := filepath.Join(dir, UrlsDir)
	var urlShims []string
	var opened []string
	var offered []string

	for _, scheme := range declaredSchemes {
		if containsIgnoreCase(urlShims, scheme.Command) {
			continue
		}
		var ofCommand []string
		for n, s := range declaredSchemes {
			if s.Command == scheme.Command {
				ofCommand = append(ofCommand, n)
			}
		}
		exportDef, ok := config.Exports[scheme.Command]
		if !ok {
			continue
		}
		target := placeholders.CommandPath(exportDef.Command)
		kind := sidecar.ShimKindFromExe(target)
		shimExe, ok := shimExes[kind]
		if !ok {
			var err error
			shimExe, err = sidecar.ShimExe(zigsawExe, kind)
			if err != nil {
				return err
			}
			shimExes[kind] = shimExe
		}
		_ = os.MkdirAll(urls, 0o755)
		sc := sidecar.Sidecar{
			Zigsaw:  zigsawExe,
			Home:    ctx.Store.Root,
			App:     config.ID,
			Command: scheme.Command,
			GUI:     kind == sidecar.ShimGUI,
			Schemes: strings.Join(ofCommand, ","),
		}
		if err := sidecar.WriteShimIn(urls, scheme.Command, shimExe, sc); err != nil {
			return ctx.Fail("writing the shim that opens links of %s in %s (is it running?): %v", scheme.Command, urls, err)
		}
		urlShims = append(urlShims, scheme.Command)
	}

	for name, scheme := range declaredSchemes {
		exportDef, ok := config.Exports[scheme.Command]
		if !ok {
			continue
		}
		target := placeholders.CommandPath(exportDef.Command)
		shownName, shownIcon := ShownAs(config, scheme.Command)
		icon := target
		if scheme.Icon != nil && *scheme.Icon != "" {
			icon = placeholders.CommandPath(*scheme.Icon)
		}
		appIcon := target
		if shownIcon != nil && *shownIcon != "" {
			appIcon = placeholders.CommandPath(*shownIcon)
		}
		desc := ""
		if scheme.Description != nil {
			desc = *scheme.Description
		}

		direct, err := registerScheme(ctx, classes, schemeRegistration{
			progID:      SchemeProgID(config.ID, name),
			app:         config.ID,
			scheme:      name,
			description: desc,
			icon:        icon,
			shim:        filepath.Join(urls, scheme.Command+".exe"),
			appName:     shownName,
			appIcon:     appIcon,
		})
		if err != nil {
			return err
		}
		if direct {
			opened = append(opened, name+":")
		} else {
			offered = append(offered, name+":")
		}
	}

	for _, o := range ownedSchemes {
		if !strings.EqualFold(o.App, config.ID) || containsKey(declaredSchemes, o.Scheme) {
			continue
		}
		_ = unregisterScheme(ctx, classes, o)
		removed = append(removed, o.Scheme+":")
	}
	_ = removeShimsExcept(urls, urlShims)
	if len(shims) == 0 && len(urlShims) == 0 {
		_ = os.Remove(dir)
	}

	_ = syncCapabilities(ctx, classesPath, config, placeholders)

	opens := append(written, opened...)
	if len(opens) > 0 {
		ctx.Note("  opens    %s", strings.Join(opens, ", "))
	}
	if len(offered) > 0 {
		ctx.Note("  offered  for %s, which something else has: choose it in Settings > Default apps", strings.Join(offered, ", "))
	}
	if len(removed) > 0 {
		ctx.Note("  no longer opens %s", strings.Join(removed, ", "))
	}
	if len(opens)+len(offered)+len(removed) > 0 {
		Changed(classesPath)
	}
	return nil
}

func RemoveAll(ctx *context.Context, app string) ([]string, error) {
	var removed []string
	classesPath := ClassesKey(ctx)
	if classesPath != "" {
		classes, err := regkey.Open(win32.HKEY_CURRENT_USER, classesPath, win32.KEY_READ|win32.KEY_WRITE)
		if err == nil && classes != nil {
			defer classes.Close()
			owned, _ := listOwned(ctx, classes)
			for _, o := range owned {
				if strings.EqualFold(o.App, app) {
					_ = unregister(classes, o)
					removed = append(removed, o.Extension)
				}
			}
			ownedSchemes, _ := listOwnedSchemes(ctx, classes)
			for _, o := range ownedSchemes {
				if strings.EqualFold(o.App, app) {
					_ = unregisterScheme(ctx, classes, o)
					removed = append(removed, o.Scheme+":")
				}
			}
			_ = removeCapabilities(classesPath, app)
			_ = RemoveStale(ctx, classes)
			if len(removed) > 0 {
				Changed(classesPath)
			}
		}
	}
	_ = os.RemoveAll(ctx.Store.OpenDir(app))
	return removed, nil
}

func RemoveStale(ctx *context.Context, classes *regkey.Key) error {
	subKeys, err := classes.SubKeys()
	if err != nil {
		return err
	}
	for _, name := range subKeys {
		if name == "" || name[0] == '.' || strings.HasPrefix(strings.ToLower(name), strings.ToLower(Prefix)) {
			continue
		}
		if namesThisStore(ctx, classes, name) {
			_ = classes.DeleteTree(name)
			ctx.Note("removed the handler of %s: links, which it started an app's program for directly", name)
		}
	}
	return nil
}

func Repair(ctx *context.Context) error {
	classesPath := ClassesKey(ctx)
	if classesPath == "" {
		return nil
	}
	classes, err := regkey.Open(win32.HKEY_CURRENT_USER, classesPath, win32.KEY_READ|win32.KEY_WRITE)
	if err != nil || classes == nil {
		return nil
	}
	defer classes.Close()
	return RemoveStale(ctx, classes)
}

func Names(ctx *context.Context, app string) ([]string, error) {
	classesPath := ClassesKey(ctx)
	if classesPath == "" {
		return nil, nil
	}
	classes, err := regkey.Open(win32.HKEY_CURRENT_USER, classesPath, win32.KEY_READ)
	if err != nil || classes == nil {
		return nil, nil
	}
	defer classes.Close()

	var found []string
	owned, _ := listOwned(ctx, classes)
	for _, o := range owned {
		if strings.EqualFold(o.App, app) {
			found = append(found, o.Extension)
		}
	}
	ownedSchemes, _ := listOwnedSchemes(ctx, classes)
	for _, o := range ownedSchemes {
		if strings.EqualFold(o.App, app) {
			found = append(found, o.Scheme+":")
		}
	}
	return found, nil
}

func namesThisStore(ctx *context.Context, classes *regkey.Key, name string) bool {
	key, err := regkey.Open(classes.Handle, name, win32.KEY_READ)
	if err != nil || key == nil {
		return false
	}
	defer key.Close()
	if _, ok := getStr(key, "URL Protocol"); !ok {
		return false
	}
	if _, ok := getStr(key, StoreValue); ok {
		return false
	}
	cmdKey, err := regkey.Open(key.Handle, `shell\open\command`, win32.KEY_READ)
	if err != nil || cmdKey == nil {
		return false
	}
	defer cmdKey.Close()
	line, ok := getStr(cmdKey, "")
	if !ok {
		return false
	}
	deploy := filepath.Join(strings.TrimRight(ctx.Store.Root, `\/`), "deploy") + `\`
	return strings.Contains(strings.ToLower(line), strings.ToLower(deploy))
}

func listOwned(ctx *context.Context, classes *regkey.Key) ([]Owned, error) {
	subKeys, err := classes.SubKeys()
	if err != nil {
		return nil, err
	}
	var owned []Owned
	for _, name := range subKeys {
		if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(Prefix)) {
			continue
		}
		key, err := regkey.Open(classes.Handle, name, win32.KEY_READ)
		if err != nil || key == nil {
			continue
		}
		st, ok := getStr(key, StoreValue)
		if !ok || !strings.EqualFold(strings.TrimRight(st, `\/`), strings.TrimRight(ctx.Store.Root, `\/`)) {
			key.Close()
			continue
		}
		app, _ := getStr(key, AppValue)
		ext, _ := getStr(key, ExtensionValue)
		key.Close()
		if app != "" && ext != "" {
			owned = append(owned, Owned{
				ProgID:    name,
				App:       app,
				Extension: ext,
			})
		}
	}
	return owned, nil
}

type registration struct {
	progID      string
	app         string
	extension   string
	description string
	icon        string
	shim        string
	appName     string
	appIcon     string
}

func register(ctx *context.Context, classes *regkey.Key, r registration) error {
	_ = classes.DeleteTree(r.progID)
	key, err := regkey.Create(classes.Handle, r.progID)
	if err != nil {
		return err
	}
	defer key.Close()

	if r.description != "" {
		_ = setStr(&key, "", r.description)
	}
	_ = setStr(&key, StoreValue, ctx.Store.Root)
	_ = setStr(&key, AppValue, r.app)
	_ = setStr(&key, ExtensionValue, r.extension)
	_ = setIn(&key, "DefaultIcon", "", fmt.Sprintf("%s,0", r.icon))
	_ = setIn(&key, "Application", "ApplicationName", r.appName)
	_ = setIn(&key, "Application", "ApplicationIcon", fmt.Sprintf("%s,0", r.appIcon))
	_ = setIn(&key, `shell\open\command`, "", fmt.Sprintf(`"%s" "%%1"`, r.shim))

	openWith, err := regkey.Create(classes.Handle, r.extension+`\OpenWithProgids`)
	if err == nil {
		defer openWith.Close()
		_ = setStr(&openWith, r.progID, "")
	}
	return nil
}

func setIn(parent *regkey.Key, sub, name, value string) error {
	key, err := regkey.Create(parent.Handle, sub)
	if err != nil {
		return err
	}
	defer key.Close()
	return setStr(&key, name, value)
}

func unregister(classes *regkey.Key, o Owned) error {
	_ = classes.DeleteTree(o.ProgID)
	if !oci.IsValidExtension(o.Extension) {
		return nil
	}
	openWithPath := o.Extension + `\OpenWithProgids`
	if openWith, err := regkey.Open(classes.Handle, openWithPath, win32.KEY_READ|win32.KEY_WRITE); err == nil && openWith != nil {
		_ = openWith.DeleteValue(o.ProgID)
		empty, _ := openWith.IsEmpty()
		openWith.Close()
		if !empty {
			return nil
		}
	}
	_ = classes.DeleteKey(openWithPath)
	if ext, err := regkey.Open(classes.Handle, o.Extension, win32.KEY_READ); err == nil && ext != nil {
		empty, _ := ext.IsEmpty()
		ext.Close()
		if !empty {
			return nil
		}
	}
	_ = classes.DeleteKey(o.Extension)
	return nil
}

func listOwnedSchemes(ctx *context.Context, classes *regkey.Key) ([]OwnedScheme, error) {
	subKeys, err := classes.SubKeys()
	if err != nil {
		return nil, err
	}
	var owned []OwnedScheme
	for _, name := range subKeys {
		if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(Prefix)) {
			continue
		}
		key, err := regkey.Open(classes.Handle, name, win32.KEY_READ)
		if err != nil || key == nil {
			continue
		}
		st, ok := getStr(key, StoreValue)
		if !ok || !strings.EqualFold(strings.TrimRight(st, `\/`), strings.TrimRight(ctx.Store.Root, `\/`)) {
			key.Close()
			continue
		}
		app, _ := getStr(key, AppValue)
		scheme, _ := getStr(key, SchemeValue)
		key.Close()
		if app != "" && scheme != "" {
			owned = append(owned, OwnedScheme{
				ProgID: name,
				App:    app,
				Scheme: scheme,
			})
		}
	}
	return owned, nil
}

type schemeRegistration struct {
	progID      string
	app         string
	scheme      string
	description string
	icon        string
	shim        string
	appName     string
	appIcon     string
}

func registerScheme(ctx *context.Context, classes *regkey.Key, r schemeRegistration) (bool, error) {
	_ = classes.DeleteTree(r.progID)
	_ = writeSchemeKey(ctx, classes, r.progID, r)
	if !mayTake(ctx, classes, r.scheme, r.app) {
		return false, nil
	}
	_ = classes.DeleteTree(r.scheme)
	_ = writeSchemeKey(ctx, classes, r.scheme, r)
	return true, nil
}

func writeSchemeKey(ctx *context.Context, classes *regkey.Key, name string, r schemeRegistration) error {
	key, err := regkey.Create(classes.Handle, name)
	if err != nil {
		return err
	}
	defer key.Close()

	desc := r.description
	if desc == "" {
		desc = r.scheme
	}
	_ = setStr(&key, "", "URL:"+desc)
	_ = setStr(&key, "URL Protocol", "")
	_ = setStr(&key, StoreValue, ctx.Store.Root)
	_ = setStr(&key, AppValue, r.app)
	_ = setStr(&key, SchemeValue, r.scheme)
	_ = setIn(&key, "DefaultIcon", "", fmt.Sprintf("%s,0", r.icon))
	_ = setIn(&key, "Application", "ApplicationName", r.appName)
	_ = setIn(&key, "Application", "ApplicationIcon", fmt.Sprintf("%s,0", r.appIcon))
	_ = setIn(&key, `shell\open\command`, "", fmt.Sprintf(`"%s" "%%1"`, r.shim))
	return nil
}

func mayTake(ctx *context.Context, classes *regkey.Key, scheme, app string) bool {
	if k, err := regkey.Open(classes.Handle, scheme, win32.KEY_READ); err == nil && k != nil {
		defer k.Close()
		st, ok := getStr(k, StoreValue)
		if ok && strings.EqualFold(strings.TrimRight(st, `\/`), strings.TrimRight(ctx.Store.Root, `\/`)) {
			whose, _ := getStr(k, AppValue)
			return strings.EqualFold(whose, app)
		}
		return namesThisStore(ctx, classes, scheme)
	}
	exists, _ := regkey.Exists(win32.HKEY_CLASSES_ROOT, scheme)
	return !exists
}

func unregisterScheme(ctx *context.Context, classes *regkey.Key, o OwnedScheme) error {
	_ = classes.DeleteTree(o.ProgID)
	key, err := regkey.Open(classes.Handle, o.Scheme, win32.KEY_READ)
	if err != nil || key == nil {
		return nil
	}
	defer key.Close()
	st, ok := getStr(key, StoreValue)
	if !ok || !strings.EqualFold(strings.TrimRight(st, `\/`), strings.TrimRight(ctx.Store.Root, `\/`)) {
		return nil
	}
	whose, _ := getStr(key, AppValue)
	if strings.EqualFold(whose, o.App) {
		_ = classes.DeleteTree(o.Scheme)
	}
	return nil
}

type capabilityKeys struct {
	registered string
	apps       string
}

func getCapabilityKeys(classesPath string) capabilityKeys {
	if strings.EqualFold(classesPath, DefaultClasses) {
		return capabilityKeys{
			registered: `Software\RegisteredApplications`,
			apps:       `Software\Zigsaw\Apps`,
		}
	}
	return capabilityKeys{
		registered: classesPath + `\RegisteredApplications`,
		apps:       classesPath + `\Apps`,
	}
}

func syncCapabilities(ctx *context.Context, classesPath string, config oci.AppConfig, placeholders oci.Placeholders) error {
	extensions := config.Associations
	schemes := config.Schemes
	_ = removeCapabilities(classesPath, config.ID)
	if len(extensions)+len(schemes) == 0 {
		return nil
	}

	keys := getCapabilityKeys(classesPath)
	var command string
	for _, ext := range extensions {
		command = ext.Command
		break
	}
	if command == "" {
		for _, s := range schemes {
			command = s.Command
			break
		}
	}
	shownName, shownIcon := ShownAs(config, command)
	icon := ""
	if shownIcon != nil && *shownIcon != "" {
		icon = *shownIcon
	} else if exp, ok := config.Exports[command]; ok {
		icon = exp.Command
	}

	path := fmt.Sprintf(`%s\%s\Capabilities`, keys.apps, config.ID)
	caps, err := regkey.Create(win32.HKEY_CURRENT_USER, path)
	if err != nil {
		return err
	}
	defer caps.Close()

	name := shownName
	if config.Name != nil && *config.Name != "" {
		name = *config.Name
	}
	_ = setStr(&caps, "ApplicationName", name)
	desc := name
	if config.Summary != nil && *config.Summary != "" {
		desc = *config.Summary
	}
	_ = setStr(&caps, "ApplicationDescription", desc)
	_ = setStr(&caps, "ApplicationIcon", fmt.Sprintf("%s,0", placeholders.CommandPath(icon)))

	if len(extensions) > 0 {
		files, err := regkey.Create(caps.Handle, "FileAssociations")
		if err == nil {
			for ext := range extensions {
				_ = setStr(&files, ext, ProgID(config.ID, ext))
			}
			files.Close()
		}
	}
	if len(schemes) > 0 {
		urls, err := regkey.Create(caps.Handle, "URLAssociations")
		if err == nil {
			for scheme := range schemes {
				_ = setStr(&urls, scheme, SchemeProgID(config.ID, scheme))
			}
			urls.Close()
		}
	}

	registered, err := regkey.Create(win32.HKEY_CURRENT_USER, keys.registered)
	if err == nil {
		_ = setStr(&registered, Prefix+config.ID, path)
		registered.Close()
	}
	return nil
}

func removeCapabilities(classesPath, app string) error {
	keys := getCapabilityKeys(classesPath)
	if reg, err := regkey.Open(win32.HKEY_CURRENT_USER, keys.registered, win32.KEY_READ|win32.KEY_WRITE); err == nil && reg != nil {
		_ = reg.DeleteValue(Prefix + app)
		empty, _ := reg.IsEmpty()
		reg.Close()
		if !strings.EqualFold(classesPath, DefaultClasses) && empty {
			_ = win32.RegDeleteKey(win32.HKEY_CURRENT_USER, keys.registered)
		}
	}

	if apps, err := regkey.Open(win32.HKEY_CURRENT_USER, keys.apps, win32.KEY_READ|win32.KEY_WRITE); err == nil && apps != nil {
		_ = apps.DeleteTree(app)
		empty, _ := apps.IsEmpty()
		apps.Close()
		if empty {
			_ = win32.RegDeleteKey(win32.HKEY_CURRENT_USER, keys.apps)
			if strings.EqualFold(keys.apps, `Software\Zigsaw\Apps`) {
				if zs, err := regkey.Open(win32.HKEY_CURRENT_USER, `Software\Zigsaw`, win32.KEY_READ); err == nil && zs != nil {
					zsEmpty, _ := zs.IsEmpty()
					zs.Close()
					if zsEmpty {
						_ = win32.RegDeleteKey(win32.HKEY_CURRENT_USER, `Software\Zigsaw`)
					}
				}
			}
		}
	}
	return nil
}

func removeAllShims(dir string) error {
	_ = removeShimsExcept(filepath.Join(dir, UrlsDir), nil)
	return removeShimsExcept(dir, nil)
}

func removeShimsExcept(dir string, commands []string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		stem := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if containsIgnoreCase(commands, stem) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
	if len(commands) == 0 {
		_ = os.Remove(dir)
	}
	return nil
}

func Changed(classesPath string) {
	if !strings.EqualFold(classesPath, DefaultClasses) {
		return
	}
	win32.SHChangeNotify(win32.SHCNE_ASSOCCHANGED, win32.SHCNF_IDLIST, 0, 0)
}

func containsIgnoreCase(list []string, name string) bool {
	for _, n := range list {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

func containsKey[V any](m map[string]V, key string) bool {
	for k := range m {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}
