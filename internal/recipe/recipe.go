package recipe

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/oci"
)

type Kind string

const (
	KindFile  Kind = "file"
	KindZip   Kind = "zip"
	KindTar   Kind = "tar"
	KindTarGz Kind = "tar.gz"
	KindTarXz Kind = "tar.xz"
)

func (k Kind) IsArchive() bool {
	return k != KindFile
}

type Source struct {
	URL    *string  `json:"url,omitempty"`
	Path   *string  `json:"path,omitempty"`
	Image  *string  `json:"image,omitempty"`
	Sha256 *string  `json:"sha256,omitempty"`
	Type   *Kind    `json:"type,omitempty"`
	Dest   *string  `json:"dest,omitempty"`
	Strip  uint32   `json:"strip,omitempty"`
	Skip   []string `json:"skip,omitempty"`
}

func (s Source) Location() string {
	if s.URL != nil {
		return *s.URL
	}
	if s.Path != nil {
		return *s.Path
	}
	return ""
}

func (s Source) FileName() string {
	loc := s.Location()
	end := len(loc)
	for i := 0; i < len(loc); i++ {
		if loc[i] == '?' || loc[i] == '#' {
			end = i
			break
		}
	}
	trimmed := loc[:end]
	start := 0
	for i := len(trimmed) - 1; i >= 0; i-- {
		if trimmed[i] == '/' || trimmed[i] == '\\' {
			start = i + 1
			break
		}
	}
	return trimmed[start:]
}

func (s Source) SourceKind() Kind {
	if s.Type != nil {
		return *s.Type
	}
	name := strings.ToLower(s.FileName())
	suffixes := []struct {
		suffix string
		kind   Kind
	}{
		{".zip", KindZip},
		{".tar", KindTar},
		{".tar.gz", KindTarGz},
		{".tgz", KindTarGz},
		{".tar.xz", KindTarXz},
		{".txz", KindTarXz},
		{".crate", KindTarGz},
	}
	for _, entry := range suffixes {
		if strings.HasSuffix(name, entry.suffix) {
			return entry.kind
		}
	}
	return KindFile
}

type Shell string

const (
	ShellSh  Shell = "sh"
	ShellCmd Shell = "cmd"
)

type Vendor struct {
	Commands []string `json:"commands"`
	Dir      string   `json:"dir"`
	Sha256   *string  `json:"sha256,omitempty"`
}

type Module struct {
	Name    string            `json:"name"`
	Sources []Source          `json:"sources,omitempty"`
	Build   []string          `json:"build,omitempty"`
	Shell   Shell             `json:"shell,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Network bool              `json:"network,omitempty"`
	Vendor  *Vendor           `json:"vendor,omitempty"`
}

type LayerKind string

const (
	LayerGzip    LayerKind = "gzip"
	LayerEStargz LayerKind = "estargz"
)

func (l LayerKind) Format() format.Format {
	switch l {
	case LayerEStargz:
		return format.FormatEstargz
	default:
		return format.FormatGzip
	}
}

type Recipe struct {
	ID           string                          `json:"id"`
	Version      string                          `json:"version"`
	Name         *string                         `json:"name,omitempty"`
	Summary      *string                         `json:"summary,omitempty"`
	Homepage     *string                         `json:"homepage,omitempty"`
	License      *string                         `json:"license,omitempty"`
	Command      string                          `json:"command"`
	Args         []string                        `json:"args,omitempty"`
	Path         []string                        `json:"path,omitempty"`
	Env          map[string]string               `json:"env,omitempty"`
	Permissions  oci.Permissions                 `json:"permissions,omitempty"`
	Exports      map[string]oci.Export           `json:"exports,omitempty"`
	Aliases      map[string]oci.Export           `json:"aliases,omitempty"`
	Shortcuts    map[string]oci.Shortcut         `json:"shortcuts,omitempty"`
	Associations map[string]oci.Association      `json:"associations,omitempty"`
	Schemes      map[string]oci.Scheme           `json:"schemes,omitempty"`
	Registry     []string                        `json:"registry,omitempty"`
	Extensions   map[string]oci.ExtensionPoint   `json:"extensions,omitempty"`
	Extends      *oci.Extends                    `json:"extends,omitempty"`
	SDK          map[string]string               `json:"sdk,omitempty"`
	Runtimes     map[string]string               `json:"runtimes,omitempty"`
	Layer        LayerKind                       `json:"layer,omitempty"`
	Prefetch     []string                        `json:"prefetch,omitempty"`
	Cleanup      []string                        `json:"cleanup,omitempty"`
	Host         []string                        `json:"host,omitempty"`
	Modules      []Module                        `json:"modules"`
}

func (r Recipe) HasBuildCommands() bool {
	for _, m := range r.Modules {
		if len(m.Build) > 0 {
			return true
		}
	}
	return false
}

func (r Recipe) AppConfig(runtimes map[string]oci.Runtime, build *oci.Build) (*oci.AppConfig, error) {
	exportsMap := make(map[string]oci.Export)
	if r.Exports != nil {
		exportsMap = r.Exports
	} else {
		stem := oci.CommandStem(r.Command)
		exportsMap[stem] = oci.Export{Command: r.Command, Args: r.Args}
	}

	p := r.Path
	if len(p) == 0 {
		p = []string{"."}
	}

	var b *oci.Build
	if build != nil && (len(build.SDK) > 0 || len(build.Sources) > 0 || len(build.Vendor) > 0 || len(build.Images) > 0 || build.Network || len(build.Host) > 0) {
		b = build
	}

	return &oci.AppConfig{
		ID:           r.ID,
		Version:      r.Version,
		Name:         r.Name,
		Summary:      r.Summary,
		Homepage:     r.Homepage,
		License:      r.License,
		Command:      r.Command,
		Args:         r.Args,
		Path:         p,
		Env:          r.Env,
		Permissions:  r.Permissions,
		Exports:      exportsMap,
		Aliases:      r.Aliases,
		Shortcuts:    r.Shortcuts,
		Associations: r.Associations,
		Schemes:      r.Schemes,
		Registry:     r.Registry,
		Runtimes:     runtimes,
		Extensions:   r.Extensions,
		Extends:      r.Extends,
		Build:        b,
	}, nil
}

func Parse(fileName string, bytes []byte) (*Recipe, error) {
	var r Recipe
	r.Layer = LayerGzip
	r.Path = []string{"."}
	if err := json.Unmarshal(bytes, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", fileName, err)
	}

	if r.Layer != "" && r.Layer != LayerGzip && r.Layer != LayerEStargz {
		return nil, fmt.Errorf("%s: unknown layer format %q", fileName, r.Layer)
	}
	if r.Layer == "" {
		r.Layer = LayerGzip
	}

	if err := validateDependencies(fileName, r); err != nil {
		return nil, err
	}
	if r.Exports == nil && oci.IsPlaceholderPath(r.Command) && !strings.HasPrefix(r.Command, "${app}") {
		return nil, fmt.Errorf("%s: the command %q is in a runtime, so the recipe must list its \"exports\"", fileName, r.Command)
	}

	var runtimeAliases []string
	for k := range r.Runtimes {
		runtimeAliases = append(runtimeAliases, k)
	}
	cfg, err := r.AppConfig(nil, nil)
	if err != nil {
		return nil, err
	}
	if err := oci.ValidateEntryPoints(fileName, cfg, runtimeAliases); err != nil {
		return nil, err
	}
	if err := validateModules(fileName, r.Modules); err != nil {
		return nil, err
	}
	for _, pattern := range r.Cleanup {
		if !isValidCleanupPattern(pattern) {
			return nil, fmt.Errorf("%s: cleanup pattern %q must be \"/<path>\" inside the app, or a file name pattern without '/' or '\\'", fileName, pattern)
		}
	}
	for _, toolchain := range r.Host {
		if toolchain != "msvc" {
			return nil, fmt.Errorf("%s: host toolchain %q isn't one quasidock knows; only \"msvc\" is", fileName, toolchain)
		}
	}
	if len(r.Host) > 0 && !r.HasBuildCommands() {
		return nil, fmt.Errorf("%s: \"host\" toolchains are for build commands, and no module has any", fileName)
	}
	if err := validatePrefetch(fileName, r); err != nil {
		return nil, err
	}
	return &r, nil
}

func validatePrefetch(fileName string, r Recipe) error {
	if len(r.Prefetch) == 0 {
		return nil
	}
	if r.Layer != LayerEStargz {
		return fmt.Errorf("%s: \"prefetch\" puts files first in an eStargz layer, so it needs \"layer\": \"estargz\"", fileName)
	}
	seen := make(map[string]struct{})
	for _, p := range r.Prefetch {
		if len(p) == 0 || p[0] == '/' || strings.ContainsAny(p, "\\:") {
			return fmt.Errorf("%s: prefetch %q must be a '/'-separated path inside the app", fileName, p)
		}
		parts := strings.Split(p, "/")
		for _, part := range parts {
			if part == "" || part == "." || part == ".." {
				return fmt.Errorf("%s: prefetch %q must be a '/'-separated path inside the app", fileName, p)
			}
		}
		lower := strings.ToLower(p)
		if _, ok := seen[lower]; ok {
			return fmt.Errorf("%s: prefetch lists %q twice", fileName, p)
		}
		seen[lower] = struct{}{}
	}
	return nil
}

func validateDependencies(fileName string, r Recipe) error {
	groups := []struct {
		what string
		deps map[string]string
	}{
		{"sdk", r.SDK},
		{"runtimes", r.Runtimes},
	}
	for _, group := range groups {
		for alias := range group.deps {
			if !oci.IsValidAlias(alias) {
				return fmt.Errorf("%s: %s alias %q must be lowercase letters, digits, '-' or '_', start with a letter, and not be app, data or cache", fileName, group.what, alias)
			}
		}
	}
	for alias := range r.SDK {
		if _, ok := r.Runtimes[alias]; ok {
			return fmt.Errorf("%s: %q is both an sdk and a runtime alias", fileName, alias)
		}
	}
	if r.Extends != nil && len(r.Runtimes) > 0 {
		return fmt.Errorf("%s: an extension can't have runtimes, which the runs it joins wouldn't have; its point's image has them", fileName)
	}
	return nil
}

func validateModules(fileName string, modules []Module) error {
	if len(modules) == 0 {
		return fmt.Errorf("%s: at least one module is required", fileName)
	}
	for i, m := range modules {
		if !oci.IsValidID(m.Name) {
			return fmt.Errorf("%s: module name %q must be 1-57 characters of letters, digits, '.', '-', '_'", fileName, m.Name)
		}
		for _, earlier := range modules[:i] {
			if strings.EqualFold(earlier.Name, m.Name) {
				return fmt.Errorf("%s: there are two modules named %s", fileName, m.Name)
			}
		}
		if len(m.Sources) == 0 && len(m.Build) == 0 {
			return fmt.Errorf("%s: module %s has neither sources nor build commands", fileName, m.Name)
		}
		if len(m.Build) == 0 && (m.Network || len(m.Env) > 0) {
			return fmt.Errorf("%s: module %s sets \"network\" or \"env\", which only apply to build commands", fileName, m.Name)
		}
		if m.Vendor != nil {
			if err := validateVendor(fileName, m, *m.Vendor); err != nil {
				return err
			}
		}
		for n, s := range m.Sources {
			if err := validateSource(fileName, m.Name, n+1, s); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateVendor(fileName string, m Module, v Vendor) error {
	if len(m.Build) == 0 {
		return fmt.Errorf("%s: module %s has a \"vendor\" step, but no build commands to use what it fetches", fileName, m.Name)
	}
	if len(v.Commands) == 0 {
		return fmt.Errorf("%s: module %s's \"vendor\" step has no commands", fileName, m.Name)
	}
	if !oci.IsSafeRelPath(v.Dir) || len(strings.Trim(v.Dir, "./\\")) == 0 {
		return fmt.Errorf("%s: module %s's vendor dir %q must be a directory inside the module's", fileName, m.Name, v.Dir)
	}
	if v.Sha256 != nil && !oci.IsSha256Hex(*v.Sha256) {
		return fmt.Errorf("%s: module %s's vendor sha256 must be 64 lowercase hex characters", fileName, m.Name)
	}
	return nil
}

func validateSource(fileName, module string, n int, s Source) error {
	given := 0
	if s.URL != nil {
		given++
	}
	if s.Path != nil {
		given++
	}
	if s.Image != nil {
		given++
	}
	if given != 1 {
		return fmt.Errorf("%s: module %s source %d needs exactly one of \"url\", \"path\" or \"image\"", fileName, module, n)
	}
	if s.Image != nil {
		if s.Sha256 != nil || s.Type != nil || s.Strip != 0 || len(s.Skip) > 0 {
			return fmt.Errorf("%s: module %s source %d is an image, pinned by its digest; \"sha256\", \"type\", \"strip\" and \"skip\" don't apply", fileName, module, n)
		}
		if s.Dest != nil && !oci.IsSafeRelPath(*s.Dest) {
			return fmt.Errorf("%s: module %s source %d dest %q must be a relative path inside the app", fileName, module, n, *s.Dest)
		}
		return nil
	}
	if s.Sha256 != nil && !oci.IsSha256Hex(*s.Sha256) {
		return fmt.Errorf("%s: module %s source %d sha256 must be 64 lowercase hex characters", fileName, module, n)
	}
	if s.Dest != nil && !oci.IsSafeRelPath(*s.Dest) {
		return fmt.Errorf("%s: module %s source %d dest %q must be a relative path inside the app", fileName, module, n, *s.Dest)
	}
	if s.Strip != 0 && !s.SourceKind().IsArchive() {
		return fmt.Errorf("%s: module %s source %d uses \"strip\", which only applies to archives", fileName, module, n)
	}
	if s.URL != nil && s.SourceKind() == KindFile && s.Dest == nil && len(s.FileName()) == 0 {
		return fmt.Errorf("%s: module %s source %d has no file name; set \"dest\"", fileName, module, n)
	}
	if len(s.Skip) > 0 && s.Path == nil {
		return fmt.Errorf("%s: module %s source %d uses \"skip\", which only applies to a directory \"path\"", fileName, module, n)
	}
	for _, pattern := range s.Skip {
		if !isValidCleanupPattern(pattern) {
			return fmt.Errorf("%s: module %s source %d skip pattern %q must be \"/<path>\" inside the directory, or a file name pattern without '/' or '\\'", fileName, module, n, pattern)
		}
	}
	return nil
}

func isValidCleanupPattern(pattern string) bool {
	if len(pattern) == 0 {
		return false
	}
	if pattern[0] == '/' {
		return oci.IsSafeRelPath(pattern[1:])
	}
	return !strings.ContainsAny(pattern, "/\\:")
}

func CleanupMatches(patterns []string, path string) bool {
	for _, pattern := range patterns {
		if len(pattern) > 0 && pattern[0] == '/' {
			want := strings.Trim(pattern, "/")
			if startsWithComponents(path, want) {
				return true
			}
		} else {
			parts := strings.Split(path, "/")
			for _, part := range parts {
				if GlobMatch(pattern, part) {
					return true
				}
			}
		}
	}
	return false
}

func startsWithComponents(path, prefix string) bool {
	wantParts := strings.FieldsFunc(prefix, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	haveParts := strings.Split(path, "/")
	for i, w := range wantParts {
		if i >= len(haveParts) {
			return false
		}
		if !strings.EqualFold(w, haveParts[i]) {
			return false
		}
	}
	return true
}

func GlobMatch(pattern, name string) bool {
	p := 0
	n := 0
	star := -1
	starN := 0
	for n < len(name) {
		if p < len(pattern) && (pattern[p] == '?' || toLower(pattern[p]) == toLower(name[n])) {
			p++
			n++
		} else if p < len(pattern) && pattern[p] == '*' {
			star = p
			starN = n
			p++
		} else if star != -1 {
			p = star + 1
			starN++
			n = starN
		} else {
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

func toLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
