package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/registry"
	"github.com/zigsaw-io/zigsaw/internal/remote"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

const (
	MediaType = "application/vnd.zigsaw.catalog.v1+json"
	Name      = "catalog"
)

type Catalog struct {
	Registry string  `json:"registry"`
	Apps     []Entry `json:"apps"`
}

type Entry struct {
	ID           string            `json:"id"`
	Version      string            `json:"version"`
	Manifest     string            `json:"manifest"`
	Download     uint64            `json:"download"`
	Name         *string           `json:"name,omitempty"`
	Summary      *string           `json:"summary,omitempty"`
	Homepage     *string           `json:"homepage,omitempty"`
	License      *string           `json:"license,omitempty"`
	Commands     []string          `json:"commands,omitempty"`
	Shortcuts    map[string]string `json:"shortcuts,omitempty"`
	Associations map[string]string `json:"associations,omitempty"`
	Permissions  oci.Permissions   `json:"permissions,omitempty"`
	Registry     []string          `json:"registry,omitempty"`
	Runtimes     []string          `json:"runtimes,omitempty"`
}

func EntryOf(info remote.Info) Entry {
	cfg := info.Config
	download := uint64(info.Manifest.Config.Size)
	for _, l := range info.Manifest.Layers {
		download += uint64(l.Size)
	}

	e := Entry{
		ID:          cfg.ID,
		Version:     cfg.Version,
		Manifest:    info.ManifestDigest,
		Download:    download,
		Name:        cfg.Name,
		Summary:     cfg.Summary,
		Homepage:    cfg.Homepage,
		License:     cfg.License,
		Permissions: cfg.Permissions,
		Registry:    cfg.Registry,
	}

	for cmd := range cfg.Exports {
		e.Commands = append(e.Commands, cmd)
	}
	sort.Strings(e.Commands)

	if len(cfg.Shortcuts) > 0 {
		e.Shortcuts = make(map[string]string)
		for k, v := range cfg.Shortcuts {
			desc := ""
			if v.Description != nil {
				desc = *v.Description
			}
			e.Shortcuts[k] = desc
		}
	}

	if len(cfg.Associations) > 0 || len(cfg.Schemes) > 0 {
		e.Associations = make(map[string]string)
		for k, v := range cfg.Associations {
			desc := ""
			if v.Description != nil {
				desc = *v.Description
			}
			e.Associations[k] = desc
		}
		for k, v := range cfg.Schemes {
			desc := ""
			if v.Description != nil {
				desc = *v.Description
			}
			e.Associations[k+":"] = desc
		}
	}

	for _, r := range cfg.Runtimes {
		e.Runtimes = append(e.Runtimes, fmt.Sprintf("%s %s", r.ID, r.Version))
	}
	sort.Strings(e.Runtimes)

	return e
}

func (e Entry) Describe() string {
	if e.Name != nil && *e.Name != "" {
		if e.Summary != nil && *e.Summary != "" {
			return fmt.Sprintf("%s: %s", *e.Name, *e.Summary)
		}
		return *e.Name
	}
	if e.Summary != nil && *e.Summary != "" {
		return *e.Summary
	}
	if len(e.Commands) > 0 {
		return "commands: " + strings.Join(e.Commands, ", ")
	}
	return ""
}

// RankOf checks whether all words match the entry.
// Returns (0, true) if all words are in id, name, or commands.
// Returns (1, true) if all words match, but some only in summary, shortcuts, associations.
// Returns (0, false) if any word is not found.
func RankOf(e Entry, words []string) (int, bool) {
	var mainFields []string
	mainFields = append(mainFields, e.ID)
	if e.Name != nil {
		mainFields = append(mainFields, *e.Name)
	}
	mainFields = append(mainFields, e.Commands...)

	var otherFields []string
	if e.Summary != nil {
		otherFields = append(otherFields, *e.Summary)
	}
	for k, v := range e.Shortcuts {
		otherFields = append(otherFields, k, v)
	}
	for k, v := range e.Associations {
		otherFields = append(otherFields, k, v)
	}

	rank := 0
	for _, word := range words {
		if containsSubstr(mainFields, word) {
			continue
		}
		if !containsSubstr(otherFields, word) {
			return 0, false
		}
		rank = 1
	}
	return rank, true
}

func containsSubstr(fields []string, word string) bool {
	wLower := strings.ToLower(word)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), wLower) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Pushing & Loading

func Push(st *store.Store, envRegistry string, ids []string, verbose bool) error {
	seen := make(map[string]bool)
	var entries []Entry

	for _, id := range ids {
		lower := strings.ToLower(id)
		if seen[lower] {
			return fmt.Errorf("%s is named twice", id)
		}
		seen[lower] = true

		info, err := remote.FetchInfo(st, envRegistry, id+":latest", verbose)
		if err != nil {
			return err
		}
		entries = append(entries, EntryOf(info))
	}

	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].ID) < strings.ToLower(entries[j].ID)
	})

	target, err := remote.Resolve(envRegistry, Name+":latest")
	if err != nil {
		return err
	}

	cat := Catalog{
		Registry: registryOf(target),
		Apps:     entries,
	}

	reg, err := registry.New(target.Ref, "pull,push", verbose)
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return err
	}

	configDesc, err := st.PutBlob(data, MediaType)
	if err != nil {
		return err
	}

	blobPath := st.BlobPath(oci.DigestHex(configDesc.Digest))
	if err := remote.UploadIfMissing(st, reg, configDesc, blobPath, "", verbose); err != nil {
		return err
	}

	manifest := oci.Manifest{
		Config: configDesc,
		Layers: []oci.Descriptor{},
		Annotations: map[string]string{
			"org.opencontainers.image.title":       "zigsaw catalog",
			"org.opencontainers.image.description": fmt.Sprintf("The %d apps in %s, for `zigsaw search`", len(entries), cat.Registry),
		},
	}
	mBytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}

	if err := reg.PushManifest("latest", mBytes); err != nil {
		return err
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "pushed the catalog of %d app(s) to %s\n", len(entries), target.Ref)
	}
	return nil
}

type Loaded struct {
	Catalog Catalog
	Source  string
}

func Load(st *store.Store, envRegistry string, verbose bool) (Loaded, error) {
	target, err := remote.Resolve(envRegistry, Name+":latest")
	if err != nil {
		return Loaded{}, err
	}

	cacheDir := filepath.Join(st.Root, "cache", "catalog")
	_ = os.MkdirAll(cacheDir, 0755)

	baseName := sanitizeFileName(target.Text)
	cachedPath := filepath.Join(cacheDir, baseName+".json")
	digestPath := filepath.Join(cacheDir, baseName+".digest")

	cachedDigest := readSmall(digestPath)

	fresh, err := fetchCatalog(st, target, cachedDigest, cachedPath, digestPath, verbose)
	if err != nil {
		if cachedDigest != "" {
			if data, errRead := os.ReadFile(cachedPath); errRead == nil {
				if verbose {
					fmt.Fprintf(os.Stderr, "using the catalog of %s fetched %s instead\n", registryOf(target), fetchedWhen(cachedDigest))
				}
				cat, errParse := parseCatalog(target.Text, data)
				if errParse == nil {
					return Loaded{Catalog: cat, Source: target.Text}, nil
				}
			}
		}
		return Loaded{}, err
	}

	cat, err := parseCatalog(target.Text, fresh)
	if err != nil {
		return Loaded{}, err
	}
	return Loaded{Catalog: cat, Source: target.Text}, nil
}

func fetchCatalog(st *store.Store, target remote.Target, cachedDigest string, cachedPath, digestPath string, verbose bool) ([]byte, error) {
	reg, err := registry.New(target.Ref, "pull", verbose)
	if err != nil {
		return nil, err
	}

	m, err := reg.FetchManifest()
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "%s has no catalog of its apps, or it can't be read; `zigsaw push --catalog` makes one\n", registryOf(target))
		}
		return nil, err
	}

	cls, err := remote.Classify(target.Ref, m.Bytes)
	if err != nil {
		return nil, err
	}
	if cls.IsIndex || cls.Image == nil {
		return nil, fmt.Errorf("%s is not a single image", target.Ref)
	}
	if cls.Image.Config.MediaType != MediaType {
		return nil, fmt.Errorf("%s isn't a zigsaw catalog (its config type is %s)", target.Ref, cls.Image.Config.MediaType)
	}

	nowSec := time.Now().Unix()
	if cachedDigest != "" && strings.HasPrefix(cachedDigest, cls.Image.Config.Digest) {
		if data, err := os.ReadFile(cachedPath); err == nil {
			_ = os.WriteFile(digestPath, []byte(fmt.Sprintf("%s %d", cls.Image.Config.Digest, nowSec)), 0644)
			return data, nil
		}
	}

	tmpPath := st.TmpPath("catalog")
	defer func() { _ = os.Remove(tmpPath) }()

	_, err = reg.DownloadBlob(cls.Image.Config, tmpPath)
	if err != nil {
		return nil, err
	}

	bytes, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, err
	}

	_ = os.WriteFile(cachedPath, bytes, 0644)
	_ = os.WriteFile(digestPath, []byte(fmt.Sprintf("%s %d", cls.Image.Config.Digest, nowSec)), 0644)
	return bytes, nil
}

func parseCatalog(source string, data []byte) (Catalog, error) {
	var cat Catalog
	if err := json.Unmarshal(data, &cat); err != nil {
		return Catalog{}, fmt.Errorf("%s: the catalog can't be read: %w", source, err)
	}
	return cat, nil
}

func registryOf(target remote.Target) string {
	text := target.Text
	if idx := strings.LastIndexByte(text, '/'); idx != -1 {
		return text[:idx]
	}
	return text
}

func sanitizeFileName(text string) string {
	var sb strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
			sb.WriteByte(c)
		case c == '.' || c == '-':
			sb.WriteByte(c)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

func readSmall(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func fetchedWhen(digestLine string) string {
	fields := strings.Fields(digestLine)
	if len(fields) < 2 {
		return "earlier"
	}
	sec, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return "earlier"
	}
	t := time.Unix(sec, 0).UTC()
	return "on " + t.Format("2006-01-02")
}

// ---------------------------------------------------------------------------
// Search

type Match struct {
	Entry Entry
	Rank  int
}

func Search(st *store.Store, envRegistry string, words []string, verbose bool) ([]Match, error) {
	loaded, err := Load(st, envRegistry, verbose)
	if err != nil {
		return nil, err
	}

	var matches []Match
	for _, e := range loaded.Catalog.Apps {
		if rank, ok := RankOf(e, words); ok {
			matches = append(matches, Match{Entry: e, Rank: rank})
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Rank != matches[j].Rank {
			return matches[i].Rank < matches[j].Rank
		}
		return strings.ToLower(matches[i].Entry.ID) < strings.ToLower(matches[j].Entry.ID)
	})

	return matches, nil
}
