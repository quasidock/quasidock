package override

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

// Settings represents run options saved for an app or store-wide.
type Settings struct {
	Sandbox      *oci.Sandbox `json:"sandbox,omitempty"`
	Network      *bool        `json:"network,omitempty"`
	Commands     *bool        `json:"commands,omitempty"`
	Host         *bool        `json:"host,omitempty"`
	Ephemeral    *bool        `json:"ephemeral,omitempty"`
	Filesystem   []string     `json:"filesystem,omitempty"`
	NoFilesystem []string     `json:"nofilesystem,omitempty"`
	Env          []string     `json:"env,omitempty"`
	NoExtension  []string     `json:"noextension,omitempty"`
	Extension    []string     `json:"extension,omitempty"`
	Ports        []string     `json:"ports,omitempty"`
	RealFolders  *bool        `json:"real_folders,omitempty"`
}

func (s Settings) IsEmpty() bool {
	return s.Sandbox == nil && s.Network == nil && s.Commands == nil && s.Host == nil &&
		s.Ephemeral == nil && len(s.Filesystem) == 0 && len(s.NoFilesystem) == 0 &&
		len(s.Env) == 0 && len(s.NoExtension) == 0 && len(s.Extension) == 0 &&
		len(s.Ports) == 0 && s.RealFolders == nil
}

// Merge merges lower with upper: upper takes precedence.
func Merge(lower, upper Settings) Settings {
	sb := upper.Sandbox
	if sb == nil {
		sb = lower.Sandbox
	}
	net := upper.Network
	if net == nil {
		net = lower.Network
	}
	cmds := upper.Commands
	if cmds == nil {
		cmds = lower.Commands
	}
	host := upper.Host
	if host == nil {
		host = lower.Host
	}
	eph := upper.Ephemeral
	if eph == nil {
		eph = lower.Ephemeral
	}
	rf := upper.RealFolders
	if rf == nil {
		rf = lower.RealFolders
	}

	return Settings{
		Sandbox:      sb,
		Network:      net,
		Commands:     cmds,
		Host:         host,
		Ephemeral:    eph,
		Filesystem:   mergeList(without(lower.Filesystem, upper.NoFilesystem, grantPath), upper.Filesystem, grantPath),
		NoFilesystem: mergeList(without(lower.NoFilesystem, upper.Filesystem, grantPath), upper.NoFilesystem, grantPath),
		Env:          mergeList(lower.Env, upper.Env, envName),
		NoExtension:  mergeList(without(lower.NoExtension, upper.Extension, itself), upper.NoExtension, itself),
		Ports:        mergeList(lower.Ports, upper.Ports, envName),
		RealFolders:  rf,
	}
}

func (s Settings) FilesystemFor(appPermissions []string) []string {
	return Merge(Settings{Filesystem: appPermissions}, s).Filesystem
}

func (s Settings) String() string {
	return s.Format()
}

func (s Settings) Format() string {
	var parts []string
	if s.Sandbox != nil {
		parts = append(parts, fmt.Sprintf("--sandbox=%s", string(*s.Sandbox)))
	}
	if s.Network != nil {
		if *s.Network {
			parts = append(parts, "--share=network")
		} else {
			parts = append(parts, "--unshare=network")
		}
	}
	if s.Commands != nil {
		if *s.Commands {
			parts = append(parts, "--share=commands")
		} else {
			parts = append(parts, "--unshare=commands")
		}
	}
	if s.Host != nil {
		if *s.Host {
			parts = append(parts, "--share=host")
		} else {
			parts = append(parts, "--unshare=host")
		}
	}
	if s.Ephemeral != nil && *s.Ephemeral {
		parts = append(parts, "--ephemeral")
	}
	for _, f := range s.Filesystem {
		parts = append(parts, fmt.Sprintf("--filesystem=%s", f))
	}
	for _, f := range s.NoFilesystem {
		parts = append(parts, fmt.Sprintf("--nofilesystem=%s", f))
	}
	for _, e := range s.Env {
		parts = append(parts, fmt.Sprintf("--env=%s", e))
	}
	for _, id := range s.NoExtension {
		parts = append(parts, fmt.Sprintf("--noextension=%s", id))
	}
	for _, id := range s.Extension {
		parts = append(parts, fmt.Sprintf("--extension=%s", id))
	}
	for _, p := range s.Ports {
		parts = append(parts, fmt.Sprintf("--port=%s", p))
	}
	if s.RealFolders != nil {
		if *s.RealFolders {
			parts = append(parts, "--real-folders")
		} else {
			parts = append(parts, "--noreal-folders")
		}
	}
	return strings.Join(parts, " ")
}

func SandboxOf(s Settings, permissions oci.Permissions) oci.Sandbox {
	if s.Sandbox != nil {
		return *s.Sandbox
	}
	if permissions.Sandbox != "" {
		return permissions.Sandbox
	}
	return oci.SandboxLow
}

func ParsePort(value string) (string, uint16, bool) {
	eq := strings.IndexByte(value, '=')
	if eq <= 0 {
		return "", 0, false
	}
	portNum, err := strconv.ParseUint(value[eq+1:], 10, 16)
	if err != nil || portNum == 0 {
		return "", 0, false
	}
	return value[:eq], uint16(portNum), true
}

func mergeList(lower, upper []string, keyFn func(string) string) []string {
	out := append([]string(nil), lower...)
	for _, u := range upper {
		uKey := keyFn(u)
		filtered := out[:0]
		for _, item := range out {
			if !strings.EqualFold(keyFn(item), uKey) {
				filtered = append(filtered, item)
			}
		}
		out = append(filtered, u)
	}
	return out
}

func without(list, others []string, keyFn func(string) string) []string {
	if len(others) == 0 {
		return list
	}
	var out []string
	for _, item := range list {
		k := keyFn(item)
		found := false
		for _, o := range others {
			if strings.EqualFold(k, keyFn(o)) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, item)
		}
	}
	return out
}

func itself(id string) string {
	return id
}

func grantPath(spec string) string {
	if strings.HasSuffix(spec, ":ro") || strings.HasSuffix(spec, ":rw") {
		return spec[:len(spec)-3]
	}
	return spec
}

func envName(assignment string) string {
	if eq := strings.IndexByte(assignment, '='); eq != -1 {
		return assignment[:eq]
	}
	return assignment
}

func FilePath(st *store.Store, id string) (string, error) {
	if id == "" {
		return st.Path("overrides.json"), nil
	}
	if !oci.IsValidID(id) {
		return "", fmt.Errorf("%q is not a valid app id", id)
	}
	return st.Path("overrides", id+".json"), nil
}

func Load(st *store.Store, id string) (Settings, error) {
	p, err := FilePath(st, id)
	if err != nil {
		return Settings{}, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Settings{}, nil
		}
		return Settings{}, err
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		target := id
		if target == "" {
			target = "--global"
		}
		return Settings{}, fmt.Errorf("%s: %w; fix it, or remove it with `zigsaw override --reset %s`", p, err, target)
	}
	return s, nil
}

func LoadFor(st *store.Store, id string) (Settings, error) {
	global, err := Load(st, "")
	if err != nil {
		return Settings{}, err
	}
	app, err := Load(st, id)
	if err != nil {
		return Settings{}, err
	}
	return Merge(global, app), nil
}

func Save(st *store.Store, id string, s Settings) error {
	if s.IsEmpty() {
		return Delete(st, id)
	}
	p, err := FilePath(st, id)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.TmpPath("override")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func Delete(st *store.Store, id string) error {
	p, err := FilePath(st, id)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
