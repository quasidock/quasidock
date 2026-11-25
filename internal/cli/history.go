package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

const historyUsage = "usage: quasidock image history [-q|--quiet] [--no-trunc] [-H|--human[=true|false]] [--format table|json|TEMPLATE] IMAGE"
const defaultHistoryTable = "{{.ID}}\t{{.CreatedSince}}\t{{.CreatedBy}}\t{{.Size}}\t{{.Comment}}"

type historyOptions struct {
	image, format         string
	quiet, noTrunc, human bool
}

func parseHistoryOptions(args []string) (historyOptions, error) {
	opts := historyOptions{human: true}
	var targets []string
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		if arg == "--" {
			targets = append(targets, args...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			targets = append(targets, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if name == "--format" {
			if !hasValue {
				if len(args) == 0 {
					return opts, errors.New("history --format needs a value")
				}
				value, args = args[0], args[1:]
			}
			if value == "" {
				return opts, errors.New("history --format needs a nonempty value")
			}
			opts.format = value
			continue
		}
		var dest *bool
		switch name {
		case "-q", "--quiet":
			dest = &opts.quiet
		case "--no-trunc":
			dest = &opts.noTrunc
		case "-H", "--human":
			dest = &opts.human
		default:
			return opts, fmt.Errorf("unknown history option %q; %s", arg, historyUsage)
		}
		if !hasValue {
			value = "true"
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return opts, fmt.Errorf("history %s expects true or false, got %q", name, value)
		}
		*dest = parsed
	}
	if len(targets) != 1 || targets[0] == "" {
		return opts, errors.New(historyUsage)
	}
	opts.image = targets[0]
	return opts, nil
}

// historyRow is the string-valued JSON/template contract. ID is always a
// manifest digest (or <missing>); LayerDigest identifies the actual layer.
type historyRow struct {
	ID, CreatedAt, CreatedSince, CreatedBy, Size, Comment string
	LayerDigest, Author                                   string
}

func imageHistoryRows(img *store.LoadedImage, digest string, opts historyOptions, now time.Time) ([]historyRow, error) {
	m, cfg := &img.Manifest, &img.Config
	rows := make([]historyRow, 0, len(m.Layers))
	add := func(layer oci.Descriptor, id, description string) error {
		if oci.DigestHex(layer.Digest) == "" || layer.Size < 0 {
			return fmt.Errorf("history: invalid layer descriptor %q", layer.Digest)
		}
		row := historyRow{ID: id, CreatedAt: "--", CreatedSince: "--", CreatedBy: description,
			Size: strconv.FormatInt(layer.Size, 10), LayerDigest: layer.Digest}
		if opts.human {
			row.Size = lifecycle.FormatBytes(uint64(layer.Size))
		}
		if !opts.noTrunc {
			if hex := oci.DigestHex(row.ID); hex != "" {
				row.ID = hex[:12]
			}
			if runes := []rune(row.CreatedBy); len(runes) > 45 {
				row.CreatedBy = string(runes[:42]) + "..."
			}
		}
		rows = append(rows, row)
		return nil
	}

	appID := digest
	if cfg.DataLayer != "" {
		description := "DATA snapshot"
		if parent := m.Annotations["io.quasidock.commit.parent"]; parent != "" {
			description = "COMMIT " + parent
		}
		if err := add(oci.DataDescriptor(m, cfg), digest, description); err != nil {
			return nil, err
		}
		// The parent may itself be a commit, so its digest does not identify
		// the image that originally produced the reused app layer.
		appID = "<missing>"
	}
	if err := add(oci.OwnLayer(m), appID, "APP "+cfg.ID+":"+cfg.Version); err != nil {
		return nil, err
	}
	rows[0].Author = m.Annotations["org.opencontainers.image.authors"]
	rows[0].Comment = m.Annotations["io.quasidock.commit.message"]
	if created, err := parseTimestamp(m.Annotations["org.opencontainers.image.created"]); err == nil {
		rows[0].CreatedAt = created.UTC().Format(time.RFC3339Nano)
		rows[0].CreatedSince = rows[0].CreatedAt
		if opts.human {
			rows[0].CreatedSince = formatRelativeTime(rows[0].CreatedAt, now)
		}
	}

	// Historical manifests can order runtimes differently from the config's
	// aliases. Match by digest, assigning identical occurrences deterministically.
	aliases := make(map[string][]string)
	for _, alias := range slices.Sorted(maps.Keys(cfg.From)) {
		layer := cfg.From[alias].Layer
		aliases[layer] = append(aliases[layer], alias)
	}
	ordered := make([]string, len(cfg.From))
	for i, layer := range m.Layers[:len(cfg.From)] {
		matches := aliases[layer.Digest]
		if len(matches) == 0 {
			return nil, fmt.Errorf("history: runtime layer %s has no matching alias", layer.Digest)
		}
		ordered[i] = matches[0]
		aliases[layer.Digest] = matches[1:]
	}
	for i := len(ordered) - 1; i >= 0; i-- {
		alias := ordered[i]
		runtime := cfg.From[alias]
		if err := add(m.Layers[i], runtime.Image, "FROM "+alias+" "+runtime.ID+":"+runtime.Version); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

type historyRenderer struct {
	quiet, table, json bool
	tmpl               *template.Template
}

func newHistoryRenderer(opts historyOptions) (*historyRenderer, error) {
	r := &historyRenderer{quiet: opts.quiet}
	if r.quiet {
		return r, nil
	}
	format := opts.format
	switch {
	case format == "json":
		r.json = true
		return r, nil
	case format == "" || format == "table":
		r.table, format = true, defaultHistoryTable
	case strings.HasPrefix(format, "table "):
		r.table, format = true, strings.TrimPrefix(format, "table ")
	}
	var err error
	r.tmpl, err = template.New("history").Funcs(template.FuncMap{"json": func(value any) (string, error) {
		data, err := json.Marshal(value)
		return string(data), err
	}}).Parse(format)
	if err != nil {
		return nil, err
	}
	if err := r.tmpl.Execute(io.Discard, historyRow{}); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *historyRenderer) render(out io.Writer, rows []historyRow) error {
	var frame bytes.Buffer
	var writer io.Writer = &frame
	var table *tabwriter.Writer
	if r.table {
		table = tabwriter.NewWriter(&frame, 0, 0, 3, ' ', 0)
		writer = table
		headings := historyRow{ID: "IMAGE", CreatedAt: "CREATED AT", CreatedSince: "CREATED", CreatedBy: "CREATED BY",
			Size: "SIZE", Comment: "COMMENT", LayerDigest: "LAYER DIGEST", Author: "AUTHOR"}
		if err := r.tmpl.Execute(writer, headings); err != nil {
			return err
		}
		fmt.Fprintln(writer)
	}
	encoder := json.NewEncoder(&frame)
	for _, row := range rows {
		switch {
		case r.quiet:
			fmt.Fprintln(&frame, row.ID)
		case r.json:
			if err := encoder.Encode(row); err != nil {
				return err
			}
		default:
			if r.table {
				row.CreatedBy = escapeTableText(row.CreatedBy)
				row.Comment = escapeTableText(row.Comment)
				row.Author = escapeTableText(row.Author)
			}
			if err := r.tmpl.Execute(writer, row); err != nil {
				return err
			}
			fmt.Fprintln(writer)
		}
	}
	if table != nil {
		if err := table.Flush(); err != nil {
			return err
		}
	}
	n, err := out.Write(frame.Bytes())
	if err == nil && n != frame.Len() {
		err = io.ErrShortWrite
	}
	return err
}

// imageCommand holds the shared store lock while history reads and renders.
func historyCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := fmt.Fprintln(ctx.Stdout, historyUsage+"\nShow local image layers: current data snapshot, app, then runtimes in reverse manifest order.\nCREATED BY contains synthesized layer descriptions, not recorded build commands.\nSIZE counts stored layer bytes, excluding the config; unknown dates are --.\nImage IDs are manifests; <missing> means the original app image is unknown.\n--format accepts json (JSON Lines), table TEMPLATE, or Go templates with ID, CreatedAt, CreatedSince, CreatedBy, Size, Comment, LayerDigest, Author.\n-q overrides --format; --no-trunc preserves full IDs and descriptions.\n--human=false prints UTC RFC3339 timestamps and decimal byte counts.\nHistory works offline, including with lazy images; parent images are not traversed.")
		return 0, err
	}
	opts, err := parseHistoryOptions(args)
	if err != nil {
		return 1, err
	}
	renderer, err := newHistoryRenderer(opts)
	if err != nil {
		return 1, err
	}
	ref, err := ctx.Store.ResolveLocalImage(ctx.Env["QUASIDOCK_REGISTRY"], opts.image)
	if err != nil {
		return 1, err
	}
	img, err := ctx.Store.ReadImage(opts.image, ref.Manifest)
	if err != nil {
		return 1, err
	}
	rows, err := imageHistoryRows(img, ref.Manifest, opts, ctx.Now())
	if err != nil {
		return 1, err
	}
	if err := renderer.render(ctx.Stdout, rows); err != nil {
		return 1, err
	}
	return 0, nil
}
