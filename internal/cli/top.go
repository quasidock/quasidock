package cli

import (
	"bytes"
	"cmp"
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"
	"unicode"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/win32"
)

const topUsage = "usage: quasidock top [--format table|json|TEMPLATE] CONTAINER"
const defaultTopTable = "{{.PID}}\t{{.PPID}}\t{{.Time}}\t{{.Command}}"

type topOptions struct {
	container, format string
}

func parseTopOptions(args []string) (topOptions, error) {
	var opts topOptions
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
		if name != "--format" {
			return opts, fmt.Errorf("unknown top option %q (Linux ps arguments are unsupported); %s", arg, topUsage)
		}
		if !hasValue {
			if len(args) == 0 {
				return opts, errors.New("top --format needs a value")
			}
			value, args = args[0], args[1:]
		}
		opts.format = value
	}
	if len(targets) != 1 || targets[0] == "" {
		return opts, errors.New(topUsage)
	}
	opts.container = targets[0]
	return opts, nil
}

// topRow is the template/JSON contract, following stats' string-valued fields.
type topRow struct {
	PID, PPID, Time, Command string
}

func topRows(processes []win32.ProcessInfo) []topRow {
	processes = slices.Clone(processes)
	slices.SortFunc(processes, func(a, b win32.ProcessInfo) int { return cmp.Compare(a.PID, b.PID) })
	rows := make([]topRow, 0, len(processes))
	for _, process := range processes {
		row := topRow{PID: strconv.FormatUint(uint64(process.PID), 10), PPID: "--", Time: "--", Command: process.Command}
		if process.PPID != nil {
			row.PPID = strconv.FormatUint(uint64(*process.PPID), 10)
		}
		if process.CPUTime != nil {
			ms := *process.CPUTime / 10000
			row.Time = fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
		}
		rows = append(rows, row)
	}
	return rows
}

type topRenderer struct {
	table bool
	json  bool
	tmpl  *template.Template
}

func newTopRenderer(format string) (*topRenderer, error) {
	r := &topRenderer{}
	switch {
	case format == "json":
		r.json = true
		return r, nil
	case format == "" || format == "table":
		r.table, format = true, defaultTopTable
	case strings.HasPrefix(format, "table "):
		r.table, format = true, strings.TrimPrefix(format, "table ")
	}
	var err error
	r.tmpl, err = template.New("top").Funcs(template.FuncMap{"json": func(value any) (string, error) {
		data, err := json.Marshal(value)
		return string(data), err
	}}).Parse(format)
	if err != nil {
		return nil, err
	}
	if err := r.tmpl.Execute(io.Discard, topRow{}); err != nil {
		return nil, err
	}
	return r, nil
}

func escapeTableText(command string) string {
	var escaped strings.Builder
	for _, r := range command {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			quoted := strconv.QuoteRune(r)
			escaped.WriteString(quoted[1 : len(quoted)-1])
		} else {
			escaped.WriteRune(r)
		}
	}
	return escaped.String()
}

func (r *topRenderer) render(out io.Writer, rows []topRow) error {
	var frame bytes.Buffer
	if r.json {
		encoder := json.NewEncoder(&frame)
		for _, row := range rows {
			if err := encoder.Encode(row); err != nil {
				return err
			}
		}
	} else {
		var writer io.Writer = &frame
		var table *tabwriter.Writer
		if r.table {
			table = tabwriter.NewWriter(&frame, 0, 0, 3, ' ', 0)
			writer = table
			if err := r.tmpl.Execute(writer, topRow{PID: "PID", PPID: "PPID", Time: "TIME", Command: "COMMAND"}); err != nil {
				return err
			}
			fmt.Fprintln(writer)
		}
		for _, row := range rows {
			if r.table {
				row.Command = escapeTableText(row.Command)
			}
			if err := r.tmpl.Execute(writer, row); err != nil {
				return err
			}
			fmt.Fprintln(writer)
		}
		if table != nil {
			if err := table.Flush(); err != nil {
				return err
			}
		}
	}
	_, err := out.Write(frame.Bytes())
	return err
}

func topCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := fmt.Fprintln(ctx.Stdout, topUsage+"\nList a running or paused container's processes once, including exec commands.\nTIME is cumulative user/kernel CPU time; COMMAND is not truncated.\n--format accepts json, table TEMPLATE, or Go templates with PID, PPID, Time, Command.\nLinux ps arguments are unsupported.")
		return 0, err
	}
	opts, err := parseTopOptions(args)
	if err != nil {
		return 1, err
	}
	renderer, err := newTopRenderer(opts.format)
	if err != nil {
		return 1, err
	}
	rec, err := ctx.Store.ResolveContainer(opts.container)
	if err != nil {
		return 1, err
	}
	cancel, stop := signal.NotifyContext(stdcontext.Background(), os.Interrupt)
	defer stop()
	snapshot, err := run.ContainerTop(cancel, ctx, rec)
	if cancel.Err() != nil {
		return 130, nil
	}
	if err != nil {
		return 1, fmt.Errorf("top for %s: %w", rec.BareName(), err)
	}
	if err := renderer.render(ctx.Stdout, topRows(snapshot.Processes)); err != nil {
		return 1, err
	}
	return 0, nil
}
