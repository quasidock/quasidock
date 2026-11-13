package oci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Media types
const (
	MediaTypeManifest      = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeConfig        = "application/vnd.zigsaw.app.config.v2+json"
	MediaTypeConfigV1      = "application/vnd.zigsaw.app.config.v1+json"
	MediaTypeLayerTar      = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeLayerTarGzip  = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeSourcesConfig = "application/vnd.zigsaw.sources.v1+json"
	MediaTypeSource        = "application/vnd.zigsaw.source.v1"
)

type SourcesConfig struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Image   string `json:"image"`
}

func SourcesTag(imageDigest string) (string, error) {
	hex := DigestHex(imageDigest)
	if hex == "" {
		return "", fmt.Errorf("invalid digest: %s", imageDigest)
	}
	return fmt.Sprintf("sha256-%s.sources", hex), nil
}

type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// Image represents an image ready to install (produced by build or pulled from remote).
type Image struct {
	ManifestDigest string
	Manifest       Manifest
	Config         AppConfig
	Source         string
	Described      bool
}

type Sandbox string

const (
	SandboxSoft         Sandbox = "soft"
	SandboxLow          Sandbox = "low"
	SandboxAppContainer Sandbox = "appcontainer"
)

type Permissions struct {
	Sandbox     Sandbox     `json:"sandbox,omitempty"`
	Network     bool        `json:"network,omitempty"`
	Filesystem  []string    `json:"filesystem,omitempty"`
	Services    []Service   `json:"services,omitempty"`
	Uses        []Use       `json:"uses,omitempty"`
	Unsupported *Unsupported `json:"unsupported,omitempty"`
	Commands    *bool       `json:"commands,omitempty"`
	Host        *bool       `json:"host,omitempty"`
}

type Unsupported struct {
	Soft         *string `json:"soft,omitempty"`
	Low          *string `json:"low,omitempty"`
	AppContainer *string `json:"appcontainer,omitempty"`
}

func (u *Unsupported) Of(s Sandbox) *string {
	if u == nil {
		return nil
	}
	switch s {
	case SandboxSoft:
		return u.Soft
	case SandboxLow:
		return u.Low
	case SandboxAppContainer:
		return u.AppContainer
	default:
		return nil
	}
}

type Service struct {
	Name    string `json:"name"`
	Port    uint16 `json:"port"`
	Command string `json:"command,omitempty"`
}

type Use struct {
	Name string `json:"name"`
	Port uint16 `json:"port"`
}

type Export struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Drop    []string `json:"drop,omitempty"`
}

type Shortcut struct {
	Command     string  `json:"command"`
	Description *string `json:"description,omitempty"`
	Icon        *string `json:"icon,omitempty"`
}

type Association struct {
	Command     string  `json:"command"`
	Description *string `json:"description,omitempty"`
	Icon        *string `json:"icon,omitempty"`
}

type Scheme struct {
	Command     string  `json:"command"`
	Description *string `json:"description,omitempty"`
	Icon        *string `json:"icon,omitempty"`
}

type Runtime struct {
	ID         string                    `json:"id"`
	Version    string                    `json:"version"`
	Image      string                    `json:"image"`
	Layer      string                    `json:"layer"`
	Path       []string                  `json:"path,omitempty"`
	Env        map[string]string         `json:"env,omitempty"`
	Aliases    map[string]Export         `json:"aliases,omitempty"`
	Extensions map[string]ExtensionPoint `json:"extensions,omitempty"`
	Uses       []Use                     `json:"uses,omitempty"`
}

type ExtensionPoint struct {
	Version string   `json:"version"`
	Join    []string `json:"join,omitempty"`
}

type Extends struct {
	Point   string   `json:"point"`
	Version string   `json:"version"`
	Needs   []string `json:"needs,omitempty"`
}

type Build struct {
	SDK     map[string]string `json:"sdk,omitempty"`
	Sources []string          `json:"sources,omitempty"`
	Vendor  map[string]string `json:"vendor,omitempty"`
	Images  []string          `json:"images,omitempty"`
	Network bool              `json:"network,omitempty"`
	Host    map[string]string `json:"host,omitempty"`
}

type AppConfig struct {
	ID           string                    `json:"id"`
	Version      string                    `json:"version"`
	Name         *string                   `json:"name,omitempty"`
	Summary      *string                   `json:"summary,omitempty"`
	Homepage     *string                   `json:"homepage,omitempty"`
	License      *string                   `json:"license,omitempty"`
	Command      string                    `json:"command"`
	Args         []string                  `json:"args,omitempty"`
	Path         []string                  `json:"path,omitempty"`
	Env          map[string]string         `json:"env,omitempty"`
	Permissions  Permissions               `json:"permissions,omitempty"`
	Exports      map[string]Export         `json:"exports,omitempty"`
	Aliases      map[string]Export         `json:"aliases,omitempty"`
	Shortcuts    map[string]Shortcut       `json:"shortcuts,omitempty"`
	Associations map[string]Association    `json:"associations,omitempty"`
	Schemes      map[string]Scheme         `json:"schemes,omitempty"`
	Registry     []string                  `json:"registry,omitempty"`
	Runtimes     map[string]Runtime        `json:"runtimes,omitempty"`
	Extensions   map[string]ExtensionPoint `json:"extensions,omitempty"`
	Extends      *Extends                  `json:"extends,omitempty"`
	Build        *Build                    `json:"build,omitempty"`
}

func ToJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Trim trailing newline from Encoder
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func IsConfigType(t string) bool {
	return t == MediaTypeConfig || t == MediaTypeConfigV1
}

func ParseConfig(what, configType string, data []byte) (*AppConfig, error) {
	if !IsConfigType(configType) {
		return nil, fmt.Errorf("%s is not a zigsaw app (its config type is %s; it may be a container image)", what, configType)
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: its app config can't be read: %w", what, err)
	}
	if configType == MediaTypeConfigV1 && len(cfg.Runtimes) > 0 {
		return nil, fmt.Errorf("%s: a v1 config can't have runtimes", what)
	}
	if err := ValidateConfig(what, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func ValidateLayers(what string, m *Manifest, c *AppConfig) error {
	for _, l := range m.Layers {
		if l.MediaType != MediaTypeLayerTar && l.MediaType != MediaTypeLayerTarGzip {
			return fmt.Errorf("%s has a layer of type %s, which zigsaw can't unpack", what, l.MediaType)
		}
	}
	expectedLayers := len(c.Runtimes) + 1
	if len(m.Layers) != expectedLayers {
		return fmt.Errorf("%s has %d layer(s), but its config accounts for %d: one per runtime, and the app's own", what, len(m.Layers), expectedLayers)
	}
	idx := 0
	for alias, r := range c.Runtimes {
		if r.Layer != m.Layers[idx].Digest {
			return fmt.Errorf("%s: runtime %s's layer %s isn't in its manifest's place for it", what, alias, r.Layer)
		}
		idx++
	}
	return nil
}

func OwnLayer(m *Manifest) Descriptor {
	return m.Layers[len(m.Layers)-1]
}

func DigestHex(digest string) string {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return ""
	}
	hex := digest[len(prefix):]
	if IsSha256Hex(hex) {
		return hex
	}
	return ""
}

func ShortDigest(digest string) string {
	limit := len("sha256:") + 12
	if len(digest) < limit {
		return digest
	}
	return digest[:limit]
}

func IsSha256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

func IsValidID(id string) bool {
	if len(id) == 0 || len(id) > 57 || id[0] == '.' {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

type DirPlaceholder struct {
	Alias string
	Path  string
}

type ServicePorts struct {
	Name   string
	Listen string
	Port   string
}

type UsedPort struct {
	Name string
	Port string
}

type Placeholders struct {
	App      string
	Data     string
	Cache    string
	Roots    string
	Runtimes []DirPlaceholder
	Services []ServicePorts
	Uses     []UsedPort
}

func (p *Placeholders) Expand(template string) string {
	var sb strings.Builder
	i := 0
	for i < len(template) {
		if name, ok := placeholderAt(template[i:]); ok {
			if val, has := p.lookup(name); has {
				sb.WriteString(val)
				i += len(name) + len("${}")
				continue
			}
		}
		sb.WriteByte(template[i])
		i++
	}
	return sb.String()
}

func (p *Placeholders) CommandPath(command string) string {
	var path string
	if IsPlaceholderPath(command) {
		path = p.Expand(command)
	} else {
		path = filepath.Join(p.App, command)
	}
	return strings.ReplaceAll(path, "/", "\\")
}

func (p *Placeholders) lookup(name string) (string, bool) {
	if name == "app" {
		return p.App, true
	}
	if name == "data" {
		return p.Data, true
	}
	if name == "cache" && p.Cache != "" {
		return p.Cache, true
	}
	if name == "roots" && p.Roots != "" {
		return p.Roots, true
	}
	for _, r := range p.Runtimes {
		if r.Alias == name {
			return r.Path, true
		}
	}
	if s, ok := servicePlaceholder(name); ok {
		for _, ports := range p.Services {
			if ports.Name == s.service {
				if s.listen {
					return ports.Listen, true
				}
				return ports.Port, true
			}
		}
	}
	if u, ok := usePlaceholder(name); ok {
		for _, used := range p.Uses {
			if used.Name == u {
				return used.Port, true
			}
		}
	}
	return "", false
}

func placeholderAt(s string) (string, bool) {
	if !strings.HasPrefix(s, "${") {
		return "", false
	}
	end := strings.IndexByte(s[2:], '}')
	if end == -1 {
		return "", false
	}
	return s[2 : 2+end], true
}

func IsPlaceholderPath(entry string) bool {
	_, ok := placeholderAt(entry)
	return ok
}

type Anchors struct {
	App     bool
	Data    bool
	Aliases []string
}

func (a Anchors) allows(name string) bool {
	if name == "app" {
		return a.App
	}
	if name == "data" || name == "cache" {
		return a.Data
	}
	for _, alias := range a.Aliases {
		if alias == name {
			return true
		}
	}
	return false
}

func isValidAnchoredPath(p string, anchors Anchors, bare bool) bool {
	name, ok := placeholderAt(p)
	if !ok {
		return IsSafeRelPath(p)
	}
	if !anchors.allows(name) {
		return false
	}
	rest := p[len(name)+len("${}"):]
	if len(rest) == 0 {
		return bare
	}
	return (rest[0] == '\\' || rest[0] == '/') && IsSafeRelPath(rest[1:])
}

func IsValidPathEntry(entry string, aliases []string) bool {
	return isValidAnchoredPath(entry, Anchors{App: true, Data: true, Aliases: aliases}, true)
}

func IsValidCommand(command string, aliases []string) bool {
	return isValidAnchoredPath(command, Anchors{App: true, Aliases: aliases}, false)
}

type serviceDesc struct {
	service string
	listen  bool
}

func servicePlaceholder(name string) (serviceDesc, bool) {
	if strings.HasPrefix(name, "listen:") {
		return serviceDesc{service: name[len("listen:"):], listen: true}, true
	}
	if strings.HasPrefix(name, "port:") {
		return serviceDesc{service: name[len("port:"):], listen: false}, true
	}
	return serviceDesc{}, false
}

func usePlaceholder(name string) (string, bool) {
	if strings.HasPrefix(name, "uses:") {
		return name[len("uses:"):], true
	}
	return "", false
}

type useScope struct {
	own      []Use
	runtimes []Runtime
}

func (s useScope) has(name string) bool {
	for _, u := range s.own {
		if u.Name == name {
			return true
		}
	}
	for _, r := range s.runtimes {
		for _, u := range r.Uses {
			if u.Name == name {
				return true
			}
		}
	}
	return false
}

func undeclaredUse(template string, scope useScope) (string, bool) {
	i := 0
	for {
		at := strings.Index(template[i:], "${")
		if at == -1 {
			break
		}
		pos := i + at
		i = pos + 2
		name, ok := placeholderAt(template[pos:])
		if !ok {
			return "", false
		}
		u, ok := usePlaceholder(name)
		if !ok {
			continue
		}
		if !scope.has(u) {
			return name, true
		}
	}
	return "", false
}

func undeclaredService(template string, services []Service) (string, bool) {
	i := 0
	for {
		at := strings.Index(template[i:], "${")
		if at == -1 {
			break
		}
		pos := i + at
		i = pos + 2
		name, ok := placeholderAt(template[pos:])
		if !ok {
			return "", false
		}
		s, ok := servicePlaceholder(name)
		if !ok {
			continue
		}
		found := false
		for _, declared := range services {
			if declared.Name == s.service {
				found = true
				break
			}
		}
		if !found {
			return name, true
		}
	}
	return "", false
}

func UsesCache(paths []string, env map[string]string) bool {
	for _, p := range paths {
		if strings.Contains(p, "${cache}") {
			return true
		}
	}
	for _, v := range env {
		if strings.Contains(v, "${cache}") {
			return true
		}
	}
	return false
}

func UsesRoots(env map[string]string, args []string) bool {
	for _, v := range env {
		if strings.Contains(v, "${roots}") {
			return true
		}
	}
	for _, a := range args {
		if strings.Contains(a, "${roots}") {
			return true
		}
	}
	return false
}

func ValidateConfig(what string, c *AppConfig) error {
	if err := validateRuntimes(what, c.Runtimes); err != nil {
		return err
	}
	aliases := make([]string, 0, len(c.Runtimes))
	for k := range c.Runtimes {
		aliases = append(aliases, k)
	}
	if err := ValidateEntryPoints(what, c, aliases); err != nil {
		return err
	}
	if c.Extends != nil && len(c.Runtimes) > 0 {
		return fmt.Errorf("%s extends %s, so it can't have runtimes: the runs it joins wouldn't have them", what, c.Extends.Point)
	}

	runtimesList := make([]Runtime, 0, len(c.Runtimes))
	for _, r := range c.Runtimes {
		runtimesList = append(runtimesList, r)
	}
	scope := useScope{own: c.Permissions.Uses, runtimes: runtimesList}
	where := "the app's arguments, variables and exports' arguments"
	for _, a := range c.Args {
		if name, ok := undeclaredUse(a, scope); ok {
			return fmt.Errorf("%s: ${%s} names a port neither the app nor its runtimes use (%s may name those)", what, name, where)
		}
	}
	for _, v := range c.Env {
		if name, ok := undeclaredUse(v, scope); ok {
			return fmt.Errorf("%s: ${%s} names a port neither the app nor its runtimes use (%s may name those)", what, name, where)
		}
	}
	for _, e := range c.Exports {
		for _, a := range e.Args {
			if name, ok := undeclaredUse(a, scope); ok {
				return fmt.Errorf("%s: ${%s} names a port neither the app nor its runtimes use (%s may name those)", what, name, where)
			}
		}
	}
	return nil
}

func ValidateEntryPoints(what string, c *AppConfig, aliases []string) error {
	if !IsValidID(c.ID) {
		return fmt.Errorf("%s: id %q must be 1-57 characters of letters, digits, '.', '-', '_'", what, c.ID)
	}
	if strings.EqualFold(c.ID, "catalog") {
		return fmt.Errorf("%s: the id \"catalog\" is the name of a registry's catalog of apps", what)
	}
	if len(c.Version) == 0 {
		return fmt.Errorf("%s: version must not be empty", what)
	}
	if c.Name != nil && !isOneLine(*c.Name, 64) {
		return fmt.Errorf("%s: name must be 1-64 bytes on one line", what)
	}
	if c.Summary != nil && !isOneLine(*c.Summary, 160) {
		return fmt.Errorf("%s: summary must be 1-160 bytes on one line", what)
	}
	if c.Homepage != nil && !isValidHomepage(*c.Homepage) {
		return fmt.Errorf("%s: homepage %q must be an https:// URL of up to 256 bytes, without spaces", what, *c.Homepage)
	}
	if c.License != nil && !isValidLicense(*c.License) {
		return fmt.Errorf("%s: license %q must be an SPDX expression, such as \"GPL-2.0-or-later\" or \"MIT OR Apache-2.0\"", what, *c.License)
	}
	if !IsValidCommand(c.Command, aliases) {
		return fmt.Errorf("%s: command %q must be a relative path inside the app, or start with a runtime's ${alias}\\", what, c.Command)
	}
	for _, p := range c.Path {
		if !IsValidPathEntry(p, aliases) {
			return fmt.Errorf("%s: path entry %q must be a relative path inside the app, or start with ${app}, ${data}, ${cache} or a runtime's ${alias}", what, p)
		}
	}
	for _, spec := range c.Permissions.Filesystem {
		if ParseFsGrant(spec) == nil {
			return fmt.Errorf("%s: filesystem permission %q must be \"cwd\", one of the user's folders (%s) or an absolute path, optionally with \":ro\"", what, spec, UserFolderList)
		}
	}
	if err := validateServices(what, c); err != nil {
		return err
	}
	if err := validateUses(what, "", c.Permissions.Uses, c.Permissions.Services); err != nil {
		return err
	}
	if c.Permissions.Unsupported != nil {
		sandboxes := []Sandbox{SandboxSoft, SandboxLow, SandboxAppContainer}
		for _, sb := range sandboxes {
			if why := c.Permissions.Unsupported.Of(sb); why != nil && !isOneLine(*why, 256) {
				return fmt.Errorf("%s: the reason it can't run under %s must be 1-256 bytes on one line", what, sb)
			}
		}
		chosenSb := c.Permissions.Sandbox
		if chosenSb == "" {
			chosenSb = SandboxLow
		}
		if c.Permissions.Unsupported.Of(chosenSb) != nil {
			return fmt.Errorf("%s: it asks for the %s sandbox and says it can't run under it", what, chosenSb)
		}
	}
	for name, exp := range c.Exports {
		if !IsValidExportName(name) {
			return fmt.Errorf("%s: export name %q must be letters, digits, '.', '-', '_', '+' (and not zigsaw's own)", what, name)
		}
		if !IsValidCommand(exp.Command, aliases) {
			return fmt.Errorf("%s: export %s command %q must be a relative path inside the app, or start with a runtime's ${alias}\\", what, name, exp.Command)
		}
		if len(exp.Drop) > 0 {
			return fmt.Errorf("%s: export %s has \"drop\", which only aliases may have", what, name)
		}
	}
	if len(c.Aliases) > 0 {
		if err := validateAliases(what, "", c.Aliases, aliases); err != nil {
			return err
		}
	}
	if len(c.Shortcuts) > 0 {
		seen := make(map[string]bool)
		for name, sc := range c.Shortcuts {
			if !IsValidShortcutName(name) {
				return fmt.Errorf("%s: shortcut name %q must be 1-64 bytes without control characters or any of <>:\"/\\|?*, not start or end with a space or '.', and not be a device name such as CON", what, name)
			}
			lower := strings.ToLower(name)
			if seen[lower] {
				return fmt.Errorf("%s: shortcuts with case-insensitive name %q would be the same file", what, name)
			}
			seen[lower] = true
			if _, ok := c.Exports[sc.Command]; !ok {
				return fmt.Errorf("%s: shortcut %q runs %q, which isn't one of the app's exports", what, name, sc.Command)
			}
			if sc.Icon != nil && !IsValidCommand(*sc.Icon, aliases) {
				return fmt.Errorf("%s: shortcut %q icon %q must be a relative path inside the app, or start with a runtime's ${alias}\\", what, name, *sc.Icon)
			}
			if sc.Description != nil && len(*sc.Description) > 256 {
				return fmt.Errorf("%s: shortcut %q description is longer than 256 bytes", what, name)
			}
		}
	}
	if len(c.Associations) > 0 {
		seenExt := make(map[string]bool)
		for ext, assoc := range c.Associations {
			if !IsValidExtension(ext) {
				return fmt.Errorf("%s: association %q must be a file extension: a '.', then 1-32 letters, digits, '-', '_' or '+'", what, ext)
			}
			lower := strings.ToLower(ext)
			if seenExt[lower] {
				return fmt.Errorf("%s: associations are the same extension: %s", what, ext)
			}
			seenExt[lower] = true
			if _, ok := c.Exports[assoc.Command]; !ok {
				return fmt.Errorf("%s: association %q opens with %q, which isn't one of the app's exports", what, ext, assoc.Command)
			}
			if assoc.Icon != nil && !IsValidCommand(*assoc.Icon, aliases) {
				return fmt.Errorf("%s: association %q icon %q must be a relative path inside the app, or start with a runtime's ${alias}\\", what, ext, *assoc.Icon)
			}
			if assoc.Description != nil && (len(*assoc.Description) == 0 || len(*assoc.Description) > 256 || strings.ContainsAny(*assoc.Description, "\r\n")) {
				return fmt.Errorf("%s: association %q description must be 1-256 bytes on one line", what, ext)
			}
		}
	}
	if len(c.Schemes) > 0 {
		for schemeName, sch := range c.Schemes {
			if !IsValidScheme(schemeName) {
				return fmt.Errorf("%s: scheme %q must be a URL scheme of up to 64 bytes: a lowercase letter, then lowercase letters, digits, '+', '-' or '.', and not one of Windows' own, such as file or ms-settings", what, schemeName)
			}
			if _, ok := c.Exports[sch.Command]; !ok {
				return fmt.Errorf("%s: scheme %q opens with %q, which isn't one of the app's exports", what, schemeName, sch.Command)
			}
			if sch.Icon != nil && !IsValidCommand(*sch.Icon, aliases) {
				return fmt.Errorf("%s: scheme %q icon %q must be a relative path inside the app, or start with a runtime's ${alias}\\", what, schemeName, *sch.Icon)
			}
			if sch.Description != nil && !isOneLine(*sch.Description, 256) {
				return fmt.Errorf("%s: scheme %q description must be 1-256 bytes on one line", what, schemeName)
			}
		}
	}
	if len(c.Registry) > 0 {
		for i, key := range c.Registry {
			if !IsValidRegistryKey(key) {
				return fmt.Errorf("%s: registry key %q must be a key of the app's own in HKEY_CURRENT_USER\\Software, such as \"Software\\\\7-Zip\": no other root, not Software itself, nor Classes, Microsoft, Policies, Wow6432Node, AppDataLow or zigsaw in it", what, key)
			}
			for _, earlier := range c.Registry[:i] {
				if registryKeyWithin(earlier, key) || registryKeyWithin(key, earlier) {
					return fmt.Errorf("%s: registry keys %q and %q overlap", what, earlier, key)
				}
			}
		}
	}
	if len(c.Extensions) > 0 {
		if err := validateExtensionPoints(what, "", c.Extensions); err != nil {
			return err
		}
	}
	if c.Extends != nil {
		if !IsValidID(c.Extends.Point) {
			return fmt.Errorf("%s: extends point %q must be an extension point's name: 1-57 characters of letters, digits, '.', '-', '_'", what, c.Extends.Point)
		}
		if !isOneLine(c.Extends.Version, 64) {
			return fmt.Errorf("%s: extends version must be 1-64 bytes on one line", what)
		}
		for _, needed := range c.Extends.Needs {
			if !IsValidID(needed) || strings.EqualFold(needed, c.ID) {
				return fmt.Errorf("%s: extends needs %q, which must be another extension's id", what, needed)
			}
		}
	}
	return nil
}

func validateServices(what string, c *AppConfig) error {
	services := c.Permissions.Services
	if c.Permissions.Services != nil && len(services) == 0 {
		return fmt.Errorf("%s: services must list at least one, or be left out", what)
	}
	for i, s := range services {
		if !isValidServiceName(s.Name) {
			return fmt.Errorf("%s: service name %q must be 1-32 lowercase letters, digits and '-'", what, s.Name)
		}
		for _, earlier := range services[:i] {
			if earlier.Name == s.Name {
				return fmt.Errorf("%s: two services are called %q", what, s.Name)
			}
		}
		if s.Port == 0 {
			return fmt.Errorf("%s: service %s must have a port from 1 to 65535", what, s.Name)
		}
		for _, earlier := range services[:i] {
			if earlier.Port == s.Port {
				return fmt.Errorf("%s: services %s and %s both have port %d", what, earlier.Name, s.Name, s.Port)
			}
		}
		if s.Command != "" {
			if _, ok := c.Exports[s.Command]; !ok {
				return fmt.Errorf("%s: service %s is served by %q, which isn't one of the app's exports", what, s.Name, s.Command)
			}
		}
	}
	where := "the app's arguments, variables and exports' arguments"
	for _, a := range c.Args {
		if name, ok := undeclaredService(a, services); ok {
			return fmt.Errorf("%s: ${%s} names a service the app doesn't have (%s may name its own)", what, name, where)
		}
	}
	for _, v := range c.Env {
		if name, ok := undeclaredService(v, services); ok {
			return fmt.Errorf("%s: ${%s} names a service the app doesn't have (%s may name its own)", what, name, where)
		}
	}
	for _, e := range c.Exports {
		for _, a := range e.Args {
			if name, ok := undeclaredService(a, services); ok {
				return fmt.Errorf("%s: ${%s} names a service the app doesn't have (%s may name its own)", what, name, where)
			}
		}
	}
	return nil
}

func validateUses(what, owner string, uses []Use, services []Service) error {
	if uses == nil {
		return nil
	}
	if len(uses) == 0 {
		return fmt.Errorf("%s: %suses must list at least one port, or be left out", what, owner)
	}
	for i, u := range uses {
		if !isValidServiceName(u.Name) {
			return fmt.Errorf("%s: %sused port name %q must be 1-32 lowercase letters, digits and '-'", what, owner, u.Name)
		}
		for _, earlier := range uses[:i] {
			if earlier.Name == u.Name {
				return fmt.Errorf("%s: %suses two ports called %q", what, owner, u.Name)
			}
		}
		for _, s := range services {
			if s.Name == u.Name {
				return fmt.Errorf("%s: %q is the name of both a service and a port the app uses", what, u.Name)
			}
		}
		if u.Port == 0 {
			return fmt.Errorf("%s: %sused port %s must be from 1 to 65535", what, owner, u.Name)
		}
		for _, earlier := range uses[:i] {
			if earlier.Port == u.Port {
				return fmt.Errorf("%s: %suses port %d as both %s and %s", what, owner, u.Port, earlier.Name, u.Name)
			}
		}
	}
	return nil
}

func isValidServiceName(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

func validateExtensionPoints(what, owner string, points map[string]ExtensionPoint) error {
	for name, p := range points {
		if !IsValidID(name) {
			return fmt.Errorf("%s: %sextension point %q must be 1-57 characters of letters, digits, '.', '-', '_'", what, owner, name)
		}
		if !isOneLine(p.Version, 64) {
			return fmt.Errorf("%s: %sextension point %s's version must be 1-64 bytes on one line", what, owner, name)
		}
		for _, v := range p.Join {
			if !isValidJoinName(v) {
				return fmt.Errorf("%s: %sextension point %s joins %q, which must be a variable's name, not PATH (extensions' path entries go on PATH anyway)", what, owner, name, v)
			}
		}
	}
	return nil
}

func isValidJoinName(name string) bool {
	if len(name) == 0 || len(name) > 64 || strings.EqualFold(name, "PATH") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '-' || c == '.' || c == '(' || c == ')' {
			continue
		}
		return false
	}
	return true
}

func validateAliases(what, owner string, aliases map[string]Export, runtimes []string) error {
	for name, e := range aliases {
		if !IsValidExportName(name) {
			return fmt.Errorf("%s: %salias name %q must be letters, digits, '.', '-', '_', '+' (and not zigsaw's own)", what, owner, name)
		}
		if !IsValidCommand(e.Command, runtimes) {
			if len(runtimes) == 0 {
				return fmt.Errorf("%s: %salias %s command %q must be a relative path inside the image", what, owner, name, e.Command)
			}
			return fmt.Errorf("%s: %salias %s command %q must be a relative path inside the image, or start with one of its runtimes' ${alias}\\", what, owner, name, e.Command)
		}
		for _, arg := range e.Args {
			if strings.Contains(arg, "${data}") || strings.Contains(arg, "${cache}") {
				return fmt.Errorf("%s: %salias %s argument %q can't use ${data} or ${cache}", what, owner, name, arg)
			}
		}
		for _, d := range e.Drop {
			if len(d) == 0 || strings.ContainsAny(d, "\r\n") || strings.Trim(d, " \t") != d {
				return fmt.Errorf("%s: %salias %s drops %q: arguments to drop must be non-empty, on one line, without spaces or tabs at either end", what, owner, name, d)
			}
		}
	}
	return nil
}

func validateRuntimes(what string, runtimes map[string]Runtime) error {
	seenIDs := make(map[string]string)
	for alias, r := range runtimes {
		if !IsValidAlias(alias) {
			return fmt.Errorf("%s: runtime alias %q must be lowercase letters, digits, '-' or '_', start with a letter, and not be app, data or cache", what, alias)
		}
		if !IsValidID(r.ID) || len(r.Version) == 0 || DigestHex(r.Image) == "" || DigestHex(r.Layer) == "" {
			return fmt.Errorf("%s: runtime %s needs a valid id, version, image digest and layer digest", what, alias)
		}
		for _, p := range r.Path {
			if !isValidAnchoredPath(p, Anchors{App: true, Data: true}, true) {
				return fmt.Errorf("%s: runtime %s's path entry %q must be inside it, or start with ${app}, ${data} or ${cache}", what, alias, p)
			}
		}
		lowerID := strings.ToLower(r.ID)
		if earlier, ok := seenIDs[lowerID]; ok {
			return fmt.Errorf("%s: runtimes %s and %s are both %s", what, earlier, alias, r.ID)
		}
		seenIDs[lowerID] = alias

		owner := fmt.Sprintf("runtime %s's ", alias)
		if len(r.Aliases) > 0 {
			if err := validateAliases(what, owner, r.Aliases, nil); err != nil {
				return err
			}
		}
		if len(r.Extensions) > 0 {
			if err := validateExtensionPoints(what, owner, r.Extensions); err != nil {
				return err
			}
		}
		if err := validateUses(what, owner, r.Uses, nil); err != nil {
			return err
		}
		for _, v := range r.Env {
			if name, ok := undeclaredUse(v, useScope{own: r.Uses}); ok {
				return fmt.Errorf("%s: ${%s} in %svariables names a port it doesn't use", what, name, owner)
			}
		}
	}
	return nil
}

func IsValidAlias(name string) bool {
	if len(name) == 0 || len(name) > 32 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	reserved := []string{"app", "data", "cache", "roots"}
	for _, r := range reserved {
		if name == r {
			return false
		}
	}
	return true
}

func IsValidExportName(name string) bool {
	if len(name) == 0 || len(name) > 64 || name[0] == '.' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '-' || c == '_' || c == '+' {
			continue
		}
		return false
	}
	reserved := []string{"zigsaw", "zigsaw-shim", "zigsaw-shimw"}
	for _, r := range reserved {
		if strings.EqualFold(name, r) {
			return false
		}
	}
	return true
}

func CommandStem(command string) string {
	base := filepath.Base(command)
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext)
}

func IsSafeRelPath(p string) bool {
	if len(p) == 0 || p[0] == '/' || p[0] == '\\' || strings.ContainsRune(p, ':') {
		return false
	}
	parts := strings.FieldsFunc(p, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	for _, part := range parts {
		if part == ".." {
			return false
		}
	}
	return true
}

func isOneLine(s string, maxLen int) bool {
	if len(s) == 0 || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

func isValidHomepage(url string) bool {
	if !strings.HasPrefix(url, "https://") || len(url) <= len("https://") || len(url) > 256 {
		return false
	}
	for i := 0; i < len(url); i++ {
		if url[i] <= 0x20 || url[i] == 0x7f {
			return false
		}
	}
	return true
}

func isValidLicense(l string) bool {
	if len(l) == 0 || len(l) > 128 {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '-' || c == '+' || c == ' ' || c == '(' || c == ')' || c == ':' {
			continue
		}
		return false
	}
	return true
}

func IsValidShortcutName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 32 || c == 127 || c == '<' || c == '>' || c == ':' || c == '"' ||
			c == '/' || c == '\\' || c == '|' || c == '?' || c == '*' {
			return false
		}
	}
	if name[0] == ' ' || name[0] == '.' || name[len(name)-1] == ' ' || name[len(name)-1] == '.' {
		return false
	}
	stem := name
	if dot := strings.IndexByte(name, '.'); dot != -1 {
		stem = name[:dot]
	}
	reserved := []string{"CON", "PRN", "AUX", "NUL"}
	for _, r := range reserved {
		if strings.EqualFold(stem, r) {
			return false
		}
	}
	if len(stem) == 4 && (strings.EqualFold(stem[:3], "COM") || strings.EqualFold(stem[:3], "LPT")) &&
		stem[3] >= '0' && stem[3] <= '9' {
		return false
	}
	return true
}

func IsValidExtension(ext string) bool {
	if len(ext) < 2 || len(ext) > 33 || ext[0] != '.' {
		return false
	}
	for i := 1; i < len(ext); i++ {
		c := ext[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '+' {
			continue
		}
		return false
	}
	return true
}

func IsValidScheme(scheme string) bool {
	if len(scheme) == 0 || len(scheme) > 64 || scheme[0] < 'a' || scheme[0] > 'z' {
		return false
	}
	for i := 0; i < len(scheme); i++ {
		c := scheme[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	reserved := []string{"file", "shell", "javascript", "vbscript", "data", "about", "res", "blob", "mk", "its", "mhtml"}
	for _, r := range reserved {
		if scheme == r {
			return false
		}
	}
	prefixes := []string{"ms-", "microsoft", "windows", "search", "zigsaw"}
	for _, p := range prefixes {
		if strings.HasPrefix(scheme, p) {
			return false
		}
	}
	return true
}

func IsValidRegistryKey(key string) bool {
	if len(key) == 0 || len(key) > 255 {
		return false
	}
	parts := strings.Split(key, "\\")
	if len(parts) < 2 {
		return false
	}
	if !strings.EqualFold(parts[0], "Software") {
		return false
	}
	for _, shared := range []string{"Classes", "Microsoft", "Policies", "Wow6432Node", "AppDataLow", "zigsaw"} {
		if strings.EqualFold(parts[1], shared) {
			return false
		}
	}
	for _, part := range parts {
		if len(part) == 0 || part == "." || part == ".." {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if c < 0x20 || c == 0x7f || c == '/' {
				return false
			}
		}
	}
	return true
}

func registryKeyWithin(outer, inner string) bool {
	if len(inner) < len(outer) || !strings.EqualFold(inner[:len(outer)], outer) {
		return false
	}
	return len(inner) == len(outer) || inner[len(outer)] == '\\'
}

type UserFolder string

const (
	UserFolderDesktop   UserFolder = "desktop"
	UserFolderDocuments UserFolder = "documents"
	UserFolderDownloads UserFolder = "downloads"
	UserFolderMusic     UserFolder = "music"
	UserFolderPictures  UserFolder = "pictures"
	UserFolderVideos    UserFolder = "videos"

	UserFolderList = "desktop, documents, downloads, music, pictures, videos"
)

func ParseUserFolder(name string) (UserFolder, bool) {
	switch strings.ToLower(name) {
	case "desktop":
		return UserFolderDesktop, true
	case "documents":
		return UserFolderDocuments, true
	case "downloads":
		return UserFolderDownloads, true
	case "music":
		return UserFolderMusic, true
	case "pictures":
		return UserFolderPictures, true
	case "videos":
		return UserFolderVideos, true
	default:
		return "", false
	}
}

type FsGrantWhereType int

const (
	FsGrantCwd FsGrantWhereType = iota
	FsGrantPath
	FsGrantFolder
	FsGrantUnknown
)

type FsGrantWhere struct {
	Type   FsGrantWhereType
	Path   string
	Folder UserFolder
}

type FsGrant struct {
	Where    FsGrantWhere
	ReadOnly bool
}

func ParseFsGrant(spec string) *FsGrant {
	s := spec
	readOnly := false
	if strings.HasSuffix(s, ":ro") {
		readOnly = true
		s = s[:len(s)-3]
	} else if strings.HasSuffix(s, ":rw") {
		s = s[:len(s)-3]
	}
	var where FsGrantWhere
	if s == "cwd" {
		where = FsGrantWhere{Type: FsGrantCwd}
	} else if f, ok := ParseUserFolder(s); ok {
		where = FsGrantWhere{Type: FsGrantFolder, Folder: f}
	} else if filepath.IsAbs(s) && len(s) > 2 {
		where = FsGrantWhere{Type: FsGrantPath, Path: s}
	} else if isPermissionName(s) {
		where = FsGrantWhere{Type: FsGrantUnknown, Path: s}
	} else {
		return nil
	}
	return &FsGrant{Where: where, ReadOnly: readOnly}
}

func isPermissionName(s string) bool {
	nameEnd := strings.IndexByte(s, '/')
	if nameEnd == -1 {
		nameEnd = len(s)
	}
	if nameEnd == 0 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 0; i < nameEnd; i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return !strings.ContainsAny(s, "\\:")
}

func JoinAnd(items []string) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) == 1 {
		return items[0]
	}
	return fmt.Sprintf("%s and %s", strings.Join(items[:len(items)-1], ", "), items[len(items)-1])
}

func DescribeServices(services []Service) string {
	parts := make([]string, len(services))
	for i, s := range services {
		parts[i] = fmt.Sprintf("%s on localhost:%d", s.Name, s.Port)
	}
	return JoinAnd(parts)
}

func DescribeUses(uses []Use) string {
	parts := make([]string, len(uses))
	for i, u := range uses {
		parts[i] = fmt.Sprintf("localhost:%d (%s)", u.Port, u.Name)
	}
	return JoinAnd(parts)
}

func DescribeRegistry(keys []string) string {
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = fmt.Sprintf("HKCU\\%s", k)
	}
	return JoinAnd(full)
}

func DescribePermissions(p Permissions) string {
	var parts []string
	if p.Sandbox == SandboxSoft {
		parts = append(parts, "whatever you can write (it asks for the soft sandbox)")
	}
	if p.Commands != nil && *p.Commands {
		parts = append(parts, "the commands of your zigsaw apps")
	}
	if p.Host != nil && *p.Host {
		parts = append(parts, "your own programs, in your real environment (zigsaw-host)")
	}
	if p.Network {
		parts = append(parts, "the network")
	}
	var rwFolders, roFolders []string
	for _, spec := range p.Filesystem {
		g := ParseFsGrant(spec)
		if g == nil {
			continue
		}
		access := ""
		if g.ReadOnly {
			access = " (read-only)"
		}
		switch g.Where.Type {
		case FsGrantCwd:
			parts = append(parts, fmt.Sprintf("the directory you run it from%s", access))
		case FsGrantPath:
			parts = append(parts, fmt.Sprintf("%s%s", g.Where.Path, access))
		case FsGrantFolder:
			folderName := strings.Title(string(g.Where.Folder))
			if g.ReadOnly {
				roFolders = append(roFolders, folderName)
			} else {
				rwFolders = append(rwFolders, folderName)
			}
		case FsGrantUnknown:
			parts = append(parts, fmt.Sprintf("%q, which this zigsaw doesn't know", g.Where.Path))
		}
	}
	if len(rwFolders) > 0 {
		parts = append(parts, fmt.Sprintf("your %s", JoinAnd(rwFolders)))
	}
	if len(roFolders) > 0 {
		parts = append(parts, fmt.Sprintf("your %s (read-only)", JoinAnd(roFolders)))
	}
	if len(parts) == 0 {
		return "nothing outside its own files"
	}
	return strings.Join(parts, "; ")
}

type AccessInfo struct {
	Permissions Permissions
	Uses        []Use
	Registry    []string
}

func (a AccessInfo) IsEmpty() bool {
	return a.Permissions.Sandbox != SandboxSoft &&
		!a.Permissions.Network &&
		len(a.Permissions.Filesystem) == 0 &&
		(a.Permissions.Commands == nil || !*a.Permissions.Commands) &&
		(a.Permissions.Host == nil || !*a.Permissions.Host) &&
		len(a.Uses) == 0 &&
		len(a.Registry) == 0
}

func AccessInfoOf(c AppConfig) AccessInfo {
	return AccessInfo{
		Permissions: c.Permissions,
		Uses:        c.Permissions.Uses,
		Registry:    c.Registry,
	}
}

func AddedAccess(before, after AccessInfo) AccessInfo {
	var uses []Use
	for _, u := range after.Uses {
		had := false
		for _, b := range before.Uses {
			if b.Port == u.Port {
				had = true
				break
			}
		}
		if !had {
			uses = append(uses, u)
		}
	}

	var fs []string
	for _, spec := range after.Permissions.Filesystem {
		want := ParseFsGrant(spec)
		if want == nil {
			continue
		}
		had := false
		for _, b := range before.Permissions.Filesystem {
			g := ParseFsGrant(b)
			if g == nil {
				continue
			}
			if (want.ReadOnly || !g.ReadOnly) && covers(g.Where, want.Where) {
				had = true
				break
			}
		}
		if !had {
			fs = append(fs, spec)
		}
	}

	var reg []string
	for _, k := range after.Registry {
		had := false
		for _, b := range before.Registry {
			if isWithin(k, b) {
				had = true
				break
			}
		}
		if !had {
			reg = append(reg, k)
		}
	}

	var addedSb Sandbox
	if after.Permissions.Sandbox == SandboxSoft && before.Permissions.Sandbox != SandboxSoft {
		addedSb = SandboxSoft
	}
	var addedCmds *bool
	if after.Permissions.Commands != nil && *after.Permissions.Commands &&
		(before.Permissions.Commands == nil || !*before.Permissions.Commands) {
		t := true
		addedCmds = &t
	}
	var addedHost *bool
	if after.Permissions.Host != nil && *after.Permissions.Host &&
		(before.Permissions.Host == nil || !*before.Permissions.Host) {
		t := true
		addedHost = &t
	}

	return AccessInfo{
		Permissions: Permissions{
			Sandbox:    addedSb,
			Network:    after.Permissions.Network && !before.Permissions.Network,
			Filesystem: fs,
			Commands:   addedCmds,
			Host:       addedHost,
		},
		Uses:     uses,
		Registry: reg,
	}
}

func covers(had, want FsGrantWhere) bool {
	if had.Type != want.Type {
		return false
	}
	switch want.Type {
	case FsGrantCwd:
		return true
	case FsGrantFolder:
		return had.Folder == want.Folder
	case FsGrantPath:
		return isWithin(want.Path, had.Path)
	case FsGrantUnknown:
		return had.Path == want.Path
	}
	return false
}

func isWithin(path, dir string) bool {
	p := strings.TrimRight(path, "\\/")
	d := strings.TrimRight(dir, "\\/")
	if len(p) < len(d) || !strings.EqualFold(p[:len(d)], d) {
		return false
	}
	return len(p) == len(d) || p[len(d)] == '\\' || p[len(d)] == '/'
}
