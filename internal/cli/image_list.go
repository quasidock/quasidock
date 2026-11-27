package cli

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/store"
)

const imageListUsage = "usage: quasidock images [-a|--all] [-q|--quiet] [--digests] [--no-trunc] [-f|--filter KEY=VALUE]... [--format table|json|TEMPLATE]"

const imageListHelp = imageListUsage + "\n" +
	"List local images and app defaults. --all includes all cached manifests.\n" +
	"Filter expressions include reference=GLOB, dangling=true|false, label=KEY[=VALUE], before=IMAGE, and since=IMAGE.\n" +
	"Reference globs select local aliases. Short patterns use QUASIDOCK_REGISTRY or the built-in registry.\n" +
	"Dangling images have no local name or app default. dangling=true includes them without --all.\n" +
	"Time filters compare OCI creation timestamps, falling back to first local cache times.\n" +
	"Repeated values for each filter key use OR. Different keys use AND.\n" +
	"--format accepts table, json (JSON Lines), table TEMPLATE, or a Go template.\n" +
	"Fields are Repository, Tag, ID, Digest, Size, CreatedAt, and CreatedSince.\n" +
	"-q prints one full manifest digest per image and overrides --format."

type imageListOptions struct {
	all, quiet, showDigests, noTrunc bool
	format                           string
	filters                          imageListFilters
}

type imageListFilters struct {
	references    []string
	dangling      []bool
	labels        []store.ImageLabelFilter
	before, since []string
}

func (f *imageListFilters) add(raw string) error {
	key, value, ok := strings.Cut(raw, "=")
	if !ok || value == "" {
		return fmt.Errorf("invalid images filter %q: expected KEY=VALUE with a nonempty value", raw)
	}
	switch key {
	case "reference":
		if _, err := path.Match(value, ""); err != nil {
			return fmt.Errorf("invalid images reference filter %q: %w", value, err)
		}
		f.references = append(f.references, value)
	case "dangling":
		dangling, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid images dangling filter %q: expected true or false", value)
		}
		f.dangling = append(f.dangling, dangling)
	case "label":
		name, val, hasValue := strings.Cut(value, "=")
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("invalid images label filter %q: label key is empty", raw)
		}
		f.labels = append(f.labels, store.ImageLabelFilter{Key: name, Value: val, HasValue: hasValue})
	case "before":
		f.before = append(f.before, value)
	case "since":
		f.since = append(f.since, value)
	default:
		return fmt.Errorf("unsupported images filter %q: use reference, dangling, label, before, or since", key)
	}
	return nil
}

func parseImageListOptions(args []string) (imageListOptions, error) {
	var opts imageListOptions
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		switch arg {
		case "-a", "--all":
			opts.all = true
		case "-q", "--quiet":
			opts.quiet = true
		case "-aq", "-qa":
			opts.all, opts.quiet = true, true
		case "--digests":
			opts.showDigests = true
		case "--no-trunc":
			opts.noTrunc = true
		default:
			name, value, hasValue := strings.Cut(arg, "=")
			switch name {
			case "-f", "--filter", "--format":
			default:
				return opts, fmt.Errorf("unknown images option %q", arg)
			}
			if !hasValue && len(args) > 0 {
				value, args = args[0], args[1:]
			}
			if value == "" {
				return opts, fmt.Errorf("images %s needs a nonempty value", name)
			}
			if name == "--format" {
				opts.format = value
			} else if err := opts.filters.add(value); err != nil {
				return opts, err
			}
		}
	}
	return opts, nil
}

type imageListRow struct {
	Repository, Tag, Digest, ID, Size, CreatedAt, CreatedSince string
}

func imageListTime(ref store.Ref, manifest *oci.Manifest) (time.Time, bool) {
	if created, err := parseTimestamp(manifest.Annotations["org.opencontainers.image.created"]); err == nil {
		return created, true
	}
	cached, err := parseTimestamp(ref.CachedAt)
	return cached, err == nil
}

func resolveImageListTarget(idx *store.ImageIndex, envRegistry, target string) (string, error) {
	ref, err := idx.Resolve(envRegistry, target)
	if err == nil {
		return ref.Manifest, nil
	}
	if len(target) != 12 {
		return "", err
	}
	var match string
	for digest := range idx.Images {
		if strings.HasPrefix(oci.DigestHex(digest), target) {
			if match != "" {
				return "", fmt.Errorf("ambiguous image ID %q; use a full manifest digest", target)
			}
			match = digest
		}
	}
	if match != "" {
		return match, nil
	}
	return "", err
}

func selectImageListAliases(names, patterns []string, registryPrefix string) ([]registry.Reference, error) {
	var aliases []registry.Reference
	for _, name := range names {
		ref, err := registry.ParseReference(name)
		if err != nil {
			return nil, err
		}
		repository := ref.Host + "/" + ref.Repository
		tagged := repository
		if ref.Tag != "" {
			tagged += ":" + ref.Tag
		}
		matches := len(patterns) == 0 || slices.ContainsFunc(patterns, func(pattern string) bool {
			for _, candidate := range []string{repository, tagged, name} {
				if !strings.ContainsRune(pattern, '/') {
					var withinRegistry bool
					candidate, withinRegistry = strings.CutPrefix(candidate, registryPrefix)
					if !withinRegistry {
						return false
					}
				}
				globMatch, _ := path.Match(pattern, candidate)
				if globMatch || pattern == candidate {
					return true
				}
			}
			return false
		})
		if matches {
			aliases = append(aliases, ref)
		}
	}
	return aliases, nil
}

func collectImageList(ctx *context.Context, idx *store.ImageIndex, opts imageListOptions) ([]imageListRow, error) {
	envRegistry := ctx.Env["QUASIDOCK_REGISTRY"]
	registryPrefix := envRegistry
	if registryPrefix == "" {
		registryPrefix = store.DefaultImageRegistry
	}
	registryPrefix = strings.TrimRight(registryPrefix, "/") + "/"

	cutoffImages := make(map[string]*store.LoadedImage)
	load := func(digest string) (*store.LoadedImage, error) {
		if img := cutoffImages[digest]; img != nil {
			return img, nil
		}
		return ctx.Store.ReadImage(idx.Images[digest].ID, digest)
	}
	resolveDates := func(key string, targets []string) ([]time.Time, error) {
		var dates []time.Time
		for _, target := range targets {
			digest, err := resolveImageListTarget(idx, envRegistry, target)
			if err != nil {
				return nil, fmt.Errorf("images %s=%s: %w", key, target, err)
			}
			img, err := load(digest)
			if err != nil {
				return nil, fmt.Errorf("images %s=%s: %w", key, target, err)
			}
			cutoffImages[digest] = img
			date, ok := imageListTime(idx.Images[digest], &img.Manifest)
			if !ok {
				return nil, fmt.Errorf("images %s=%s: image has no creation or local cache timestamp", key, target)
			}
			dates = append(dates, date)
		}
		return dates, nil
	}
	filters := opts.filters
	before, err := resolveDates("before", filters.before)
	if err != nil {
		return nil, err
	}
	since, err := resolveDates("since", filters.since)
	if err != nil {
		return nil, err
	}
	filterDates := len(before) > 0 || len(since) > 0
	showDangling := opts.all || slices.Contains(filters.dangling, true)

	namesByDigest := make(map[string][]string)
	for name, digest := range idx.Names {
		namesByDigest[digest] = append(namesByDigest[digest], name)
	}
	now := ctx.Now()
	var rows []imageListRow
	for _, digest := range slices.Sorted(maps.Keys(idx.Images)) {
		ref := idx.Images[digest]
		names := namesByDigest[digest]
		dangling := len(names) == 0 && idx.Defaults[strings.ToLower(ref.ID)] != digest
		if dangling && !showDangling || len(filters.dangling) > 0 && !slices.Contains(filters.dangling, dangling) {
			continue
		}
		var aliases []registry.Reference
		if !opts.quiet || len(filters.references) > 0 {
			aliases, err = selectImageListAliases(names, filters.references, registryPrefix)
			if err != nil {
				return nil, err
			}
			if len(filters.references) > 0 && len(aliases) == 0 {
				continue
			}
		}

		row := imageListRow{Digest: digest}
		if !opts.quiet || filterDates || len(filters.labels) > 0 {
			img, err := load(digest)
			if err != nil {
				return nil, err
			}
			if len(filters.labels) > 0 && !slices.ContainsFunc(filters.labels, func(filter store.ImageLabelFilter) bool {
				return filter.Matches(img.Config.Labels)
			}) {
				continue
			}
			date, hasDate := imageListTime(ref, &img.Manifest)
			if filterDates && (!hasDate ||
				len(before) > 0 && !slices.ContainsFunc(before, date.Before) ||
				len(since) > 0 && !slices.ContainsFunc(since, date.After)) {
				continue
			}
			if !opts.quiet {
				row.ID = digest
				if !opts.noTrunc {
					row.ID = oci.DigestHex(digest)[:12]
				}
				row.Size = fmt.Sprintf("%d B", imageManifestSize(&img.Manifest))
				row.CreatedAt, row.CreatedSince = "--", "--"
				if hasDate {
					row.CreatedAt = date.UTC().Format(time.RFC3339Nano)
					row.CreatedSince = humanDuration(now.Sub(date)) + " ago"
				}
			}
		}
		if opts.quiet {
			rows = append(rows, row)
		} else if len(aliases) == 0 {
			row.Repository, row.Tag = "<none>", "<none>"
			rows = append(rows, row)
		} else {
			for _, alias := range aliases {
				row.Repository = alias.Host + "/" + alias.Repository
				row.Tag = cmp.Or(alias.Tag, "<none>")
				rows = append(rows, row)
			}
		}
	}
	if !opts.quiet {
		slices.SortFunc(rows, func(a, b imageListRow) int {
			return cmp.Or(cmp.Compare(a.Repository, b.Repository), cmp.Compare(a.Tag, b.Tag), cmp.Compare(a.Digest, b.Digest))
		})
	}
	return rows, nil
}

var imageListHeadings = imageListRow{
	Repository: "REPOSITORY", Tag: "TAG", Digest: "DIGEST", ID: "IMAGE ID", Size: "SIZE",
	CreatedAt: "CREATED AT", CreatedSince: "CREATED",
}

func writeImageListDefaultTable(out *bytes.Buffer, opts imageListOptions, rows []imageListRow) {
	const fullDigestWidth = len("sha256:") + 64
	idWidth := 12
	if opts.noTrunc {
		idWidth = fullDigestWidth
	}
	writeRow := func(row imageListRow) {
		fmt.Fprintf(out, "%-54s %-24s ", row.Repository, row.Tag)
		if opts.showDigests {
			fmt.Fprintf(out, "%-*s ", fullDigestWidth, row.Digest)
		}
		fmt.Fprintf(out, "%-*s %s\n", idWidth, row.ID, row.Size)
	}
	writeRow(imageListHeadings)
	for _, row := range rows {
		writeRow(row)
	}
}

func writeImageList(out io.Writer, opts imageListOptions, tmpl *template.Template, customTable bool, rows []imageListRow) error {
	var output bytes.Buffer
	switch {
	case opts.quiet:
		for _, row := range rows {
			fmt.Fprintln(&output, row.Digest)
		}
	case opts.format == "json":
		encoder := json.NewEncoder(&output)
		for _, row := range rows {
			if err := encoder.Encode(row); err != nil {
				return err
			}
		}
	case tmpl != nil:
		var writer io.Writer = &output
		var table *tabwriter.Writer
		if customTable {
			table = tabwriter.NewWriter(&output, 0, 0, 3, ' ', 0)
			writer = table
			if err := tmpl.Execute(writer, imageListHeadings); err != nil {
				return err
			}
			fmt.Fprintln(writer)
		}
		for _, row := range rows {
			if err := tmpl.Execute(writer, row); err != nil {
				return err
			}
			fmt.Fprintln(writer)
		}
		if table != nil {
			if err := table.Flush(); err != nil {
				return err
			}
		}
	default:
		writeImageListDefaultTable(&output, opts, rows)
	}
	_, err := output.WriteTo(out)
	return err
}

func imageList(ctx *context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := fmt.Fprintln(ctx.Stdout, imageListHelp)
		return err
	}
	opts, err := parseImageListOptions(args)
	if err != nil {
		return err
	}
	var tmpl *template.Template
	var customTable bool
	if !opts.quiet && opts.format != "" && opts.format != "table" && opts.format != "json" {
		format := opts.format
		format, customTable = strings.CutPrefix(format, "table ")
		if customTable {
			if strings.TrimSpace(format) == "" {
				return errors.New("images table format needs a template")
			}
			format = strings.ReplaceAll(format, "\\t", "\t")
		}
		tmpl, err = template.New("images").Funcs(template.FuncMap{"json": func(value any) (string, error) {
			data, err := json.Marshal(value)
			return string(data), err
		}}).Parse(format)
		if err != nil {
			return err
		}
	}
	idx, err := ctx.Store.ImageCatalog()
	if err != nil {
		return err
	}
	rows, err := collectImageList(ctx, idx, opts)
	if err != nil {
		return err
	}
	return writeImageList(ctx.Stdout, opts, tmpl, customTable, rows)
}
