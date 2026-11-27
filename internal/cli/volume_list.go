package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
)

const volumeListUsage = "usage: quasidock volume ls [-q|--quiet] [-f|--filter KEY=VALUE]... [--format table|json|TEMPLATE]"

const volumeListHelp = volumeListUsage + "\n" +
	"The alias is volume list.\n" +
	"Filter by name=REGEXP (case-sensitive), driver=DRIVER, dangling=true|false, or label=KEY[=VALUE].\n" +
	"Dangling volumes have no saved container references. Repeated values for one key use OR. Different keys use AND.\n" +
	"--format accepts table, json (JSON Lines), table TEMPLATE, or a Go template.\n" +
	"Template fields are Name, Driver, Scope, Mountpoint, CreatedAt, Anonymous, and Labels. Use {{.Label \"key\"}} to look up a label.\n" +
	"-q prints volume names and overrides --format."

type volumeListOptions struct {
	quiet    bool
	format   string
	names    []*regexp.Regexp
	drivers  []string
	dangling []bool
	labels   []volumeLabelFilter
}

type volumeLabelFilter struct {
	key, value string
	hasValue   bool
}

func (opts *volumeListOptions) addFilter(raw string) error {
	key, value, ok := strings.Cut(raw, "=")
	if !ok || key == "" || value == "" {
		return fmt.Errorf("invalid volume ls filter %q: expected KEY=VALUE with a nonempty value", raw)
	}
	switch key {
	case "name":
		pattern, err := regexp.Compile(value)
		if err != nil {
			return fmt.Errorf("invalid volume name filter %q: %w", value, err)
		}
		opts.names = append(opts.names, pattern)
	case "driver":
		opts.drivers = append(opts.drivers, value)
	case "dangling":
		dangling, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid volume dangling filter %q: expected true or false", value)
		}
		opts.dangling = append(opts.dangling, dangling)
	case "label":
		if !validMetadataKeyValue(value) {
			return fmt.Errorf("invalid volume label filter %q: expected KEY[=VALUE] with a nonempty key and no whitespace in the key or NUL characters", value)
		}
		name, label, hasValue := strings.Cut(value, "=")
		opts.labels = append(opts.labels, volumeLabelFilter{key: name, value: label, hasValue: hasValue})
	default:
		return fmt.Errorf("unsupported volume ls filter %q: use name, driver, dangling, or label", key)
	}
	return nil
}

func parseVolumeListOptions(args []string) (volumeListOptions, error) {
	var opts volumeListOptions
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		switch arg {
		case "-q", "--quiet":
			opts.quiet = true
			continue
		}
		option, value, assigned := strings.Cut(arg, "=")
		switch option {
		case "-f", "--filter", "--format":
		default:
			return opts, fmt.Errorf("unknown volume ls option %q", arg)
		}
		if !assigned && len(args) > 0 {
			value, args = args[0], args[1:]
		}
		if strings.TrimSpace(value) == "" {
			return opts, fmt.Errorf("volume ls %s needs a nonempty value", option)
		}
		if option == "--format" {
			opts.format = value
		} else if err := opts.addFilter(value); err != nil {
			return opts, err
		}
	}
	return opts, nil
}

func (opts volumeListOptions) matches(v store.Volume) bool {
	if len(opts.drivers) > 0 && !slices.Contains(opts.drivers, v.Driver) {
		return false
	}
	if len(opts.names) > 0 && !slices.ContainsFunc(opts.names, func(name *regexp.Regexp) bool {
		return name.MatchString(v.Name)
	}) {
		return false
	}
	return len(opts.labels) == 0 || slices.ContainsFunc(opts.labels, func(filter volumeLabelFilter) bool {
		value, present := v.Labels[filter.key]
		return present && (!filter.hasValue || value == filter.value)
	})
}

type volumeListRow struct {
	Name, Driver, Scope, Mountpoint, CreatedAt string
	Anonymous                                  bool
	Labels                                     map[string]string
}

func (r volumeListRow) Label(key string) string { return r.Labels[key] }

func newVolumeListRow(v store.Volume) volumeListRow {
	labels := v.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return volumeListRow{
		Name: v.Name, Driver: v.Driver, Scope: "local", Mountpoint: v.Mountpoint,
		CreatedAt: v.CreatedAt, Anonymous: v.Anonymous, Labels: labels,
	}
}

type volumeListHeadings struct {
	Name, Driver, Scope, Mountpoint, CreatedAt, Anonymous, Labels string
}

func (volumeListHeadings) Label(key string) string { return key }

func parseVolumeListTemplate(format string) (*template.Template, bool, error) {
	table := false
	switch {
	case format == "" || format == "table":
		format, table = "{{.Driver}}\t{{.Name}}", true
	case strings.HasPrefix(format, "table "):
		format, table = strings.TrimPrefix(format, "table "), true
		if strings.TrimSpace(format) == "" {
			return nil, false, errors.New("volume ls table format needs a template")
		}
		format = strings.ReplaceAll(format, "\\t", "\t")
	}
	tmpl, err := template.New("volume ls").Funcs(template.FuncMap{
		"json": func(value any) (string, error) {
			data, err := json.Marshal(value)
			return string(data), err
		},
	}).Parse(format)
	return tmpl, table, err
}

func writeVolumeList(out io.Writer, opts volumeListOptions, tmpl *template.Template, table bool, volumes []store.Volume) error {
	var output bytes.Buffer
	switch {
	case opts.quiet:
		for _, v := range volumes {
			fmt.Fprintln(&output, v.Name)
		}
	case opts.format == "json":
		encoder := json.NewEncoder(&output)
		for _, v := range volumes {
			if err := encoder.Encode(newVolumeListRow(v)); err != nil {
				return err
			}
		}
	default:
		var writer io.Writer = &output
		var tw *tabwriter.Writer
		if table {
			tw = tabwriter.NewWriter(&output, 0, 0, 3, ' ', 0)
			writer = tw
			headings := volumeListHeadings{
				Name: "VOLUME NAME", Driver: "DRIVER", Scope: "SCOPE",
				Mountpoint: "MOUNTPOINT", CreatedAt: "CREATED AT", Anonymous: "ANONYMOUS", Labels: "LABELS",
			}
			if err := tmpl.Execute(writer, headings); err != nil {
				return err
			}
			fmt.Fprintln(writer)
		}
		for _, v := range volumes {
			if err := tmpl.Execute(writer, newVolumeListRow(v)); err != nil {
				return err
			}
			fmt.Fprintln(writer)
		}
		if tw != nil {
			if err := tw.Flush(); err != nil {
				return err
			}
		}
	}
	_, err := output.WriteTo(out)
	return err
}

func volumeList(ctx *context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := fmt.Fprintln(ctx.Stdout, volumeListHelp)
		return err
	}
	opts, err := parseVolumeListOptions(args)
	if err != nil {
		return err
	}
	var tmpl *template.Template
	var table bool
	if !opts.quiet && opts.format != "json" {
		tmpl, table, err = parseVolumeListTemplate(opts.format)
		if err != nil {
			return err
		}
	}
	var volumes []store.Volume
	wantDangling := slices.Contains(opts.dangling, true)
	wantReferenced := slices.Contains(opts.dangling, false)
	if wantDangling != wantReferenced {
		volumes, err = ctx.Store.ListVolumesByDangling(wantDangling)
	} else {
		volumes, err = ctx.Store.ListVolumes()
	}
	if err != nil {
		return err
	}
	volumes = slices.DeleteFunc(volumes, func(v store.Volume) bool { return !opts.matches(v) })
	return writeVolumeList(ctx.Stdout, opts, tmpl, table, volumes)
}
