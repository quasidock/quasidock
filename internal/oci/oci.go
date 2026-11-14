package oci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// Media types
const (
	MediaTypeManifest            = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeConfig              = "application/vnd.quasidock.app.config.v2+json"
	MediaTypeConfigV1            = "application/vnd.quasidock.app.config.v1+json"
	LegacyMediaTypeConfig        = "application/vnd.zigsaw.app.config.v2+json"
	LegacyMediaTypeConfigV1      = "application/vnd.zigsaw.app.config.v1+json"
	MediaTypeLayerTar            = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeLayerTarGzip        = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeSourcesConfig       = "application/vnd.quasidock.sources.v1+json"
	MediaTypeSource              = "application/vnd.quasidock.source.v1"
	LegacyMediaTypeSourcesConfig = "application/vnd.zigsaw.sources.v1+json"
	LegacyMediaTypeSource        = "application/vnd.zigsaw.source.v1"
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
	Sandbox     Sandbox      `json:"sandbox,omitempty"`
	Network     bool         `json:"network,omitempty"`
	Filesystem  []string     `json:"filesystem,omitempty"`
	Services    []Service    `json:"services,omitempty"`
	Uses        []Use        `json:"uses,omitempty"`
	Unsupported *Unsupported `json:"unsupported,omitempty"`
	Host        *bool        `json:"host,omitempty"`
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

type Runtime struct {
	ID      string            `json:"id"`
	Version string            `json:"version"`
	Image   string            `json:"image"`
	Layer   string            `json:"layer"`
	Path    []string          `json:"path,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Uses    []Use             `json:"uses,omitempty"`
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
	ID          string             `json:"id"`
	Version     string             `json:"version"`
	Name        *string            `json:"name,omitempty"`
	Summary     *string            `json:"summary,omitempty"`
	Homepage    *string            `json:"homepage,omitempty"`
	License     *string            `json:"license,omitempty"`
	Entrypoint  []string           `json:"entrypoint"`
	Cmd         []string           `json:"cmd,omitempty"`
	Path        []string           `json:"path,omitempty"`
	Env         map[string]string  `json:"env,omitempty"`
	Permissions Permissions        `json:"permissions,omitempty"`
	Registry    []string           `json:"registry,omitempty"`
	From        map[string]Runtime `json:"from,omitempty"`
	Build       *Build             `json:"build,omitempty"`
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
	return t == MediaTypeConfig || t == MediaTypeConfigV1 ||
		t == LegacyMediaTypeConfig || t == LegacyMediaTypeConfigV1
}

func ParseConfig(what, configType string, data []byte) (*AppConfig, error) {
	if !IsConfigType(configType) {
		return nil, fmt.Errorf("%s is not a quasidock app (its config type is %s; it may be a container image)", what, configType)
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: its app config can't be read: %w", what, err)
	}
	if (configType == MediaTypeConfigV1 || configType == LegacyMediaTypeConfigV1) && len(cfg.From) > 0 {
		return nil, fmt.Errorf("%s: a v1 config can't have base images", what)
	}
	if err := ValidateConfig(what, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func ValidateLayers(what string, m *Manifest, c *AppConfig) error {
	for _, l := range m.Layers {
		if l.MediaType != MediaTypeLayerTar && l.MediaType != MediaTypeLayerTarGzip {
			return fmt.Errorf("%s has a layer of type %s, which quasidock can't unpack", what, l.MediaType)
		}
	}
	expectedLayers := len(c.From) + 1
	if len(m.Layers) != expectedLayers {
		return fmt.Errorf("%s has %d layer(s), but its config accounts for %d: one per base image, and the app's own", what, len(m.Layers), expectedLayers)
	}
	available := map[string]int{}
	for _, layer := range m.Layers[:len(m.Layers)-1] {
		available[layer.Digest]++
	}
	for alias, r := range c.From {
		if available[r.Layer] == 0 {
			return fmt.Errorf("%s: base image %s's layer %s isn't in its manifest's place for it", what, alias, r.Layer)
		}
		available[r.Layer]--
	}
	return nil
}

// RuntimeDirectories matches deployed layers by digest, including cached images
// whose historical runtime layers were emitted in a different map order.
func RuntimeDirectories(from map[string]Runtime, dirs []string) []DirPlaceholder {
	var result []DirPlaceholder
	for i, alias := range slices.Sorted(maps.Keys(from)) {
		if i >= len(dirs) {
			break
		}
		dir := dirs[i]
		for _, candidate := range dirs {
			if filepath.Base(candidate) == DigestHex(from[alias].Layer) {
				dir = candidate
				break
			}
		}
		result = append(result, DirPlaceholder{Alias: alias, Path: dir})
	}
	return result
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
	if err := validateFrom(what, c.From); err != nil {
		return err
	}
	aliases := make([]string, 0, len(c.From))
	for k := range c.From {
		aliases = append(aliases, k)
	}
	if err := ValidateEntryPoints(what, c, aliases); err != nil {
		return err
	}

	fromList := make([]Runtime, 0, len(c.From))
	for _, r := range c.From {
		fromList = append(fromList, r)
	}
	scope := useScope{own: c.Permissions.Uses, runtimes: fromList}
	where := "the app's entrypoint, cmd and variables"
	for _, a := range c.Entrypoint {
		if name, ok := undeclaredUse(a, scope); ok {
			return fmt.Errorf("%s: ${%s} names a port neither the app nor its base images use (%s may name those)", what, name, where)
		}
	}
	for _, a := range c.Cmd {
		if name, ok := undeclaredUse(a, scope); ok {
			return fmt.Errorf("%s: ${%s} names a port neither the app nor its base images use (%s may name those)", what, name, where)
		}
	}
	for _, v := range c.Env {
		if name, ok := undeclaredUse(v, scope); ok {
			return fmt.Errorf("%s: ${%s} names a port neither the app nor its base images use (%s may name those)", what, name, where)
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
	if len(c.Entrypoint) == 0 {
		return fmt.Errorf("%s: entrypoint must not be empty", what)
	}
	if !IsValidCommand(c.Entrypoint[0], aliases) {
		return fmt.Errorf("%s: entrypoint %q must be a relative path inside the app, or start with a base image's ${alias}\\", what, c.Entrypoint[0])
	}
	for _, p := range c.Path {
		if !IsValidPathEntry(p, aliases) {
			return fmt.Errorf("%s: path entry %q must be a relative path inside the app, or start with ${app}, ${data}, ${cache} or a base image's ${alias}", what, p)
		}
	}
	for _, spec := range c.Permissions.Filesystem {
		if ParseFsGrant(spec) == nil {
			return fmt.Errorf("%s: filesystem permission %q must be \"cwd\" or an absolute path, optionally with \":ro\"", what, spec)
		}
	}
	if err := validateServices(what, c, aliases); err != nil {
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
	if len(c.Registry) > 0 {
		for i, key := range c.Registry {
			if !IsValidRegistryKey(key) {
				return fmt.Errorf("%s: registry key %q must be a key of the app's own in HKEY_CURRENT_USER\\Software, such as \"Software\\\\7-Zip\": no other root, not Software itself, nor Classes, Microsoft, Policies, Wow6432Node, AppDataLow or quasidock in it", what, key)
			}
			for _, earlier := range c.Registry[:i] {
				if registryKeyWithin(earlier, key) || registryKeyWithin(key, earlier) {
					return fmt.Errorf("%s: registry keys %q and %q overlap", what, earlier, key)
				}
			}
		}
	}
	return nil
}

func validateServices(what string, c *AppConfig, aliases []string) error {
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
		if s.Command != "" && !IsValidCommand(s.Command, aliases) {
			return fmt.Errorf("%s: service %s command %q must be a relative path inside the app, or start with a base image's ${alias}\\", what, s.Name, s.Command)
		}
	}
	where := "the app's entrypoint, cmd and variables"
	for _, a := range c.Entrypoint {
		if name, ok := undeclaredService(a, services); ok {
			return fmt.Errorf("%s: ${%s} names a service the app doesn't have (%s may name its own)", what, name, where)
		}
	}
	for _, a := range c.Cmd {
		if name, ok := undeclaredService(a, services); ok {
			return fmt.Errorf("%s: ${%s} names a service the app doesn't have (%s may name its own)", what, name, where)
		}
	}
	for _, v := range c.Env {
		if name, ok := undeclaredService(v, services); ok {
			return fmt.Errorf("%s: ${%s} names a service the app doesn't have (%s may name its own)", what, name, where)
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

func validateFrom(what string, from map[string]Runtime) error {
	seenIDs := make(map[string]string)
	for alias, r := range from {
		if !IsValidAlias(alias) {
			return fmt.Errorf("%s: base image alias %q must be lowercase letters, digits, '-' or '_', start with a letter, and not be app, data or cache", what, alias)
		}
		if !IsValidID(r.ID) || len(r.Version) == 0 || DigestHex(r.Image) == "" || DigestHex(r.Layer) == "" {
			return fmt.Errorf("%s: base image %s needs a valid id, version, image digest and layer digest", what, alias)
		}
		for _, p := range r.Path {
			if !isValidAnchoredPath(p, Anchors{App: true, Data: true}, true) {
				return fmt.Errorf("%s: base image %s's path entry %q must be inside it, or start with ${app}, ${data} or ${cache}", what, alias, p)
			}
		}
		lowerID := strings.ToLower(r.ID)
		if earlier, ok := seenIDs[lowerID]; ok {
			return fmt.Errorf("%s: base images %s and %s are both %s", what, earlier, alias, r.ID)
		}
		seenIDs[lowerID] = alias

		owner := fmt.Sprintf("base image %s's ", alias)
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
	for _, shared := range []string{"Classes", "Microsoft", "Policies", "Wow6432Node", "AppDataLow", "zigsaw", "quasidock"} {
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

type FsGrantWhereType int

const (
	FsGrantCwd FsGrantWhereType = iota
	FsGrantPath
	FsGrantUnknown
)

type FsGrantWhere struct {
	Type FsGrantWhereType
	Path string
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
	if p.Host != nil && *p.Host {
		parts = append(parts, "your own programs, in your real environment (quasidock-host)")
	}
	if p.Network {
		parts = append(parts, "the network")
	}
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
		case FsGrantUnknown:
			parts = append(parts, fmt.Sprintf("%q, which this quasidock doesn't know", g.Where.Path))
		}
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
