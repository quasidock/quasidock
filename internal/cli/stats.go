package cli

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

const statsUsage = "usage: quasidock stats [-a|--all] [--no-stream] [--no-trunc] [--format table|json|TEMPLATE] [CONTAINER...]"
const defaultStatsTable = "{{.ID}}\t{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.IO}}\t{{.PIDs}}"

type statsOptions struct {
	all, noStream, noTrunc bool
	format                 string
	targets                []string
}

func parseStatsOptions(args []string) (statsOptions, error) {
	var opts statsOptions
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		if arg == "--" {
			opts.targets = append(opts.targets, args...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			opts.targets = append(opts.targets, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if name == "--format" {
			if !hasValue {
				if len(args) == 0 {
					return opts, errors.New("stats --format needs a value")
				}
				value, args = args[0], args[1:]
			}
			opts.format = value
			continue
		}
		var dest *bool
		switch name {
		case "-a", "--all":
			dest = &opts.all
		case "--no-stream":
			dest = &opts.noStream
		case "--no-trunc":
			dest = &opts.noTrunc
		default:
			return opts, fmt.Errorf("unknown stats option %q; %s", arg, statsUsage)
		}
		if !hasValue {
			*dest = true
		} else {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return opts, fmt.Errorf("invalid stats boolean %q: expected true or false", value)
			}
			*dest = parsed
		}
	}
	return opts, nil
}

// statsRow is the public template/JSON presentation contract. Keep fields as
// strings so table headings use the same templates as data rows.
type statsRow struct {
	Container, ID, Name, CPUPerc, MemUsage, IO, PIDs string
}

type statsRenderer struct {
	table bool
	json  bool
	tmpl  *template.Template
}

func newStatsRenderer(format string) (*statsRenderer, error) {
	r := &statsRenderer{}
	switch {
	case format == "json":
		r.json = true
		return r, nil
	case format == "" || format == "table":
		r.table, format = true, defaultStatsTable
	case strings.HasPrefix(format, "table "):
		r.table, format = true, strings.TrimPrefix(format, "table ")
	}
	var err error
	r.tmpl, err = template.New("stats").Funcs(template.FuncMap{"json": func(value any) (string, error) {
		data, err := json.Marshal(value)
		return string(data), err
	}}).Parse(format)
	if err != nil {
		return nil, err
	}
	// Validate field names even when no containers match.
	if err := r.tmpl.Execute(io.Discard, statsRow{}); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *statsRenderer) render(out io.Writer, rows []statsRow, redraw bool) error {
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
			headings := statsRow{Container: "CONTAINER", ID: "CONTAINER ID", Name: "NAME", CPUPerc: "CPU %", MemUsage: "PRIVATE MEMORY", IO: "I/O", PIDs: "PIDS"}
			if err := r.tmpl.Execute(writer, headings); err != nil {
				return err
			}
			fmt.Fprintln(writer)
		}
		for _, row := range rows {
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
	if redraw && r.table {
		if _, err := io.WriteString(out, "\x1b[H\x1b[2J"); err != nil {
			return err
		}
	}
	_, err := out.Write(frame.Bytes())
	return err
}

type statsTarget struct{ id, input string }
type statsContainer struct {
	record store.ContainerRecord
	input  string
}

func resolveStatsTargets(st *store.Store, names []string) ([]statsTarget, error) {
	var targets []statsTarget
	seen := make(map[string]bool)
	for _, name := range names {
		rec, err := st.ResolveContainer(name)
		if err != nil {
			return nil, err
		}
		if !seen[rec.ID] {
			seen[rec.ID] = true
			targets = append(targets, statsTarget{rec.ID, name})
		}
	}
	return targets, nil
}

func selectStatsContainers(st *store.Store, opts statsOptions, targets []statsTarget) ([]statsContainer, error) {
	all, err := st.ListContainers(true)
	if err != nil {
		return nil, err
	}
	var selected []statsContainer
	if len(opts.targets) == 0 {
		for _, rec := range all {
			if opts.all || rec.State.Running || rec.State.Restarting {
				selected = append(selected, statsContainer{record: rec})
			}
		}
	} else {
		byID := make(map[string]store.ContainerRecord, len(all))
		for _, rec := range all {
			byID[rec.ID] = rec
		}
		for _, target := range targets {
			if rec, ok := byID[target.id]; ok {
				selected = append(selected, statsContainer{rec, target.input})
			}
		}
	}
	return selected, nil
}

func statsCPU(previous, current run.StatsSample, processors uint32) string {
	elapsed := current.SampledAt.Sub(previous.SampledAt)
	if previous.RunID != current.RunID || previous.SampledAt.IsZero() || elapsed <= 0 || processors == 0 || current.CPUTime < previous.CPUTime {
		return "--"
	}
	percent := float64(current.CPUTime-previous.CPUTime) * 100 / (elapsed.Seconds() * 1e7 * float64(processors))
	return fmt.Sprintf("%.2f%%", math.Min(100, percent))
}

type statsSampler func(stdcontext.Context, *context.Context, *store.ContainerRecord) (run.StatsSample, error)

func collectStats(cancel stdcontext.Context, ctx *context.Context, selected []statsContainer, opts statsOptions, previous map[string]run.StatsSample, processors uint32, sample statsSampler) ([]statsRow, map[string]run.StatsSample, error) {
	rows := make([]statsRow, 0, len(selected))
	next := make(map[string]run.StatsSample, len(selected))
	for _, container := range selected {
		if err := cancel.Err(); err != nil {
			return nil, nil, err
		}
		rec := container.record
		id := rec.ShortID()
		if opts.noTrunc {
			id = rec.ID
		}
		input := container.input
		if input == "" {
			input = id
		}
		row := statsRow{Container: input, ID: id, Name: rec.BareName(), CPUPerc: "0.00%", MemUsage: "0 B", IO: "0 B / 0 B", PIDs: "0"}
		if rec.State.Running {
			current, err := sample(cancel, ctx, &rec)
			if err != nil {
				// Exit, removal and restart can race any stage of a request.
				latest, readErr := ctx.Store.ReadContainer(rec.ID)
				if errors.Is(err, run.ErrStatsRunChanged) || errors.Is(readErr, store.ErrContainerNotFound) || (readErr == nil && (!latest.State.Running || latest.State.RunID != rec.State.RunID)) {
					continue
				}
				return nil, nil, fmt.Errorf("stats for %s: %w", rec.BareName(), err)
			}
			next[rec.ID] = current
			row.CPUPerc = statsCPU(previous[rec.ID], current, processors)
			row.MemUsage = lifecycle.FormatBytes(current.PrivateMemory)
			row.IO = lifecycle.FormatBytes(current.ReadBytes) + " / " + lifecycle.FormatBytes(current.WriteBytes)
			row.PIDs = strconv.FormatUint(uint64(current.PIDs), 10)
		}
		rows = append(rows, row)
	}
	return rows, next, nil
}

func streamStats(cancel stdcontext.Context, ctx *context.Context, opts statsOptions, renderer *statsRenderer, interval time.Duration, sample statsSampler, processors uint32, redraw bool) error {
	targets, err := resolveStatsTargets(ctx.Store, opts.targets)
	if err != nil {
		return err
	}
	selected, err := selectStatsContainers(ctx.Store, opts, targets)
	if err != nil {
		return err
	}
	_, previous, err := collectStats(cancel, ctx, selected, opts, nil, processors, sample)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-cancel.Done():
			return cancel.Err()
		case <-ticker.C:
		}
		selected, err = selectStatsContainers(ctx.Store, opts, targets)
		if err != nil {
			return err
		}
		var rows []statsRow
		rows, previous, err = collectStats(cancel, ctx, selected, opts, previous, processors, sample)
		if err != nil {
			return err
		}
		if err := renderer.render(ctx.Stdout, rows, redraw); err != nil {
			return err
		}
		if opts.noStream || (len(opts.targets) > 0 && len(selected) == 0) {
			return nil
		}
	}
}

// statsConsole enables VT only for a real output console. Files, pipes and
// injected writers never receive screen-control sequences.
func statsConsole(out io.Writer) (bool, func()) {
	file, ok := out.(*os.File)
	if !ok {
		return false, func() {}
	}
	handle := win32.HANDLE(file.Fd())
	var mode win32.DWORD
	if !win32.GetConsoleMode(handle, &mode) || !win32.SetConsoleMode(handle, mode|4) {
		return false, func() {}
	}
	return true, func() { win32.SetConsoleMode(handle, mode) }
}

func statsCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := fmt.Fprintln(ctx.Stdout, statsUsage+"\nSamples every second. CPU uses whole-host capacity; memory is private committed bytes.\nI/O is cumulative read/write traffic; PIDS counts processes. --format accepts table TEMPLATE or Go templates.")
		return 0, err
	}
	opts, err := parseStatsOptions(args)
	if err != nil {
		return 1, err
	}
	renderer, err := newStatsRenderer(opts.format)
	if err != nil {
		return 1, err
	}
	processors, err := win32.ActiveProcessorCount()
	if err != nil {
		return 1, err
	}
	redraw := false
	if !opts.noStream && renderer.table {
		var restore func()
		redraw, restore = statsConsole(ctx.Stdout)
		defer restore()
	}
	cancel, stop := signal.NotifyContext(stdcontext.Background(), os.Interrupt)
	defer stop()
	if err := streamStats(cancel, ctx, opts, renderer, time.Second, run.ContainerStats, processors, redraw); err != nil {
		if cancel.Err() != nil {
			return 130, nil
		}
		return 1, err
	}
	return 0, nil
}
