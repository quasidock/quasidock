package environment

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/win32"
)

var Passthrough = []string{
	"SystemRoot", "windir", "SystemDrive",
	"ComSpec", "PATHEXT", "OS",
	"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "PROCESSOR_IDENTIFIER",
	"PROCESSOR_LEVEL", "PROCESSOR_REVISION", "USERNAME",
}

var Proxies = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
}

type Profile struct {
	Home    string
	Roaming string
	Local   string
	Temp    string
}

func NewProfile(dir string) Profile {
	home := filepath.Join(dir, "home")
	local := filepath.Join(home, "AppData", "Local")
	return Profile{
		Home:    home,
		Roaming: filepath.Join(home, "AppData", "Roaming"),
		Local:   local,
		Temp:    filepath.Join(local, "Temp"),
	}
}

type Var struct {
	Name  string
	Value string
}

type Spec struct {
	ID         string
	Profile    Profile
	Path       []string
	PathAfter  []string
	SystemRoot string
	Vars       []Var
	Extra      []string
	Proxies    bool
}

func lookupFold(m map[string]string, key string) (string, bool) {
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

func Build(host map[string]string, spec Spec) ([]Var, error) {
	var env []Var
	for _, name := range Passthrough {
		if val, ok := lookupFold(host, name); ok {
			Set(&env, name, val)
		}
	}
	if spec.Proxies {
		for _, name := range Proxies {
			if val, ok := lookupFold(host, name); ok {
				Set(&env, name, val)
			}
		}
	}

	var path strings.Builder
	for _, dir := range spec.Path {
		path.WriteString(dir)
		path.WriteByte(';')
	}
	fmt.Fprintf(&path, "%s\\System32;%s;%s\\System32\\Wbem", spec.SystemRoot, spec.SystemRoot, spec.SystemRoot)
	for _, dir := range spec.PathAfter {
		path.WriteByte(';')
		path.WriteString(dir)
	}
	Set(&env, "PATH", path.String())

	p := spec.Profile
	Set(&env, "USERPROFILE", p.Home)
	Set(&env, "HOME", p.Home)
	if len(p.Home) > 2 && p.Home[1] == ':' {
		Set(&env, "HOMEDRIVE", p.Home[0:2])
		Set(&env, "HOMEPATH", p.Home[2:])
	}
	Set(&env, "APPDATA", p.Roaming)
	Set(&env, "LOCALAPPDATA", p.Local)
	Set(&env, "TEMP", p.Temp)
	Set(&env, "TMP", p.Temp)
	Set(&env, "QUASIDOCK_ID", spec.ID)

	for _, kv := range spec.Vars {
		Set(&env, kv.Name, kv.Value)
	}

	for _, extra := range spec.Extra {
		eq := strings.IndexByte(extra, '=')
		if eq == -1 {
			if extra == "" || strings.ContainsRune(extra, '\x00') {
				return nil, fmt.Errorf("invalid environment name %q", extra)
			}
			if val, ok := lookupFold(host, extra); ok {
				Set(&env, extra, val)
			} else {
				kept := env[:0]
				for _, kv := range env {
					if !strings.EqualFold(kv.Name, extra) {
						kept = append(kept, kv)
					}
				}
				env = kept
			}
			continue
		}
		if eq == 0 || strings.ContainsRune(extra, '\x00') {
			return nil, fmt.Errorf("invalid environment setting %q", extra)
		}
		Set(&env, extra[0:eq], extra[eq+1:])
	}

	return env, nil
}

func RestoreUser(m map[string]string) error {
	block, err := process.UserEnvironmentBlock()
	if err != nil {
		return err
	}
	rest := block
	for len(rest) > 0 && rest[0] != 0 {
		end := 0
		for end < len(rest) && rest[end] != 0 {
			end++
		}
		if end >= len(rest) {
			break
		}
		entry := syscall.UTF16ToString(rest[0:end])
		rest = rest[end+1:]

		eq := strings.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		name := entry[0:eq]
		val := entry[eq+1:]
		if strings.HasPrefix(strings.ToUpper(name), "QUASIDOCK_") {
			continue
		}
		m[name] = val
		_ = win32.SetEnvironmentVariable(name, val)
	}
	return nil
}

func Expand(env *[]Var, placeholders oci.Placeholders, vars map[string]string) error {
	// Sort keys for deterministic expansion order
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		val := placeholders.Expand(vars[k])
		Set(env, k, val)
	}
	return nil
}

func Set(env *[]Var, name, value string) {
	for i := range *env {
		if strings.EqualFold((*env)[i].Name, name) {
			(*env)[i].Value = value
			return
		}
	}
	*env = append(*env, Var{Name: name, Value: value})
}

func EncodeBlock(vars []Var) ([]uint16, error) {
	sorted := make([]Var, len(vars))
	copy(sorted, vars)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.ToLower(sorted[i].Name) < strings.ToLower(sorted[j].Name)
	})
	var block []uint16
	for _, kv := range sorted {
		line := kv.Name + "=" + kv.Value
		u16 := syscall.StringToUTF16(line)
		block = append(block, u16...)
	}
	block = append(block, 0)
	return block, nil
}

func PathDirs(placeholders oci.Placeholders, entries []string) ([]string, error) {
	var dirs []string
	for _, entry := range entries {
		var dir string
		if oci.IsPlaceholderPath(entry) {
			dir = placeholders.Expand(entry)
		} else if entry == "." {
			dir = placeholders.App
		} else {
			dir = filepath.Join(placeholders.App, entry)
		}
		dir = strings.ReplaceAll(dir, "/", "\\")
		dirs = append(dirs, dir)
	}
	return dirs, nil
}
