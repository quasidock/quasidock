package cli

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestParseStatsOptions(t *testing.T) {
	opts, err := parseStatsOptions([]string{"worker", "-a", "--no-stream=true", "--no-trunc", "--format=json", "other"})
	if err != nil || !opts.all || !opts.noStream || !opts.noTrunc || opts.format != "json" || strings.Join(opts.targets, ",") != "worker,other" {
		t.Fatalf("%+v %v", opts, err)
	}
	opts, err = parseStatsOptions([]string{"--all=false", "--no-stream=false", "--no-trunc=false", "--format", "{{.Name}}", "--", "-worker"})
	if err != nil || opts.all || opts.noStream || opts.noTrunc || opts.format != "{{.Name}}" || opts.targets[0] != "-worker" {
		t.Fatalf("%+v %v", opts, err)
	}
	for _, args := range [][]string{{"--format"}, {"--bogus"}, {"--all=maybe"}, {"-q"}, {"--no-stream="}} {
		if _, err := parseStatsOptions(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestStatsSelection(t *testing.T) {
	ctx, _, records := psFixture(t)
	selected, err := selectStatsContainers(ctx.Store, statsOptions{}, nil)
	if err != nil || len(selected) != 2 || selected[0].record.BareName() != "web-paused" || selected[1].record.BareName() != "web-running" {
		t.Fatalf("default selection: %+v %v", selected, err)
	}
	selected, err = selectStatsContainers(ctx.Store, statsOptions{all: true}, nil)
	if err != nil || len(selected) != len(records) {
		t.Fatalf("all: %+v %v", selected, err)
	}
	names := []string{"archive", records["archive"].ID[:12], "WEB-CREATED", "web-paused"}
	targets, err := resolveStatsTargets(ctx.Store, names)
	if err != nil || len(targets) != 3 {
		t.Fatalf("resolve: %+v %v", targets, err)
	}
	selected, err = selectStatsContainers(ctx.Store, statsOptions{targets: names}, targets)
	if err != nil || len(selected) != 3 || selected[0].input != "archive" || selected[1].input != "WEB-CREATED" {
		t.Fatalf("explicit selection: %+v %v", selected, err)
	}
	if err := ctx.Store.RemoveContainer(records["archive"].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ctx.Store.CreateContainer("archive", store.ContainerRecord{}); err != nil {
		t.Fatal(err)
	}
	selected, err = selectStatsContainers(ctx.Store, statsOptions{targets: names}, targets)
	if err != nil || len(selected) != 2 || selected[0].input != "WEB-CREATED" {
		t.Fatalf("removed target: %+v %v", selected, err)
	}
	if _, err := resolveStatsTargets(ctx.Store, []string{"missing"}); !errors.Is(err, store.ErrContainerNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestStatsCPU(t *testing.T) {
	previous := run.StatsSample{RunID: "one", SampledAt: time.Now(), JobStats: win32.JobStats{CPUTime: 100}}
	current := run.StatsSample{RunID: "one", SampledAt: previous.SampledAt.Add(time.Second), JobStats: win32.JobStats{CPUTime: 10000000 + 100}}
	if got := statsCPU(previous, current, 8); got != "12.50%" {
		t.Fatalf("host scale: %s", got)
	}
	if got := statsCPU(previous, current, 1); got != "100.00%" {
		t.Fatalf("one core: %s", got)
	}
	for _, tc := range []struct {
		before, after run.StatsSample
		processors    uint32
	}{
		{run.StatsSample{}, current, 8},
		{previous, run.StatsSample{RunID: "two", SampledAt: current.SampledAt}, 8},
		{previous, previous, 8},
		{current, previous, 8},
		{previous, current, 0},
		{previous, run.StatsSample{RunID: "one", SampledAt: current.SampledAt, JobStats: win32.JobStats{CPUTime: 99}}, 8},
	} {
		if got := statsCPU(tc.before, tc.after, tc.processors); got != "--" {
			t.Errorf("invalid baseline produced %s", got)
		}
	}
}

func TestStatsRendering(t *testing.T) {
	row := statsRow{Container: "worker", ID: "abcdef", Name: "worker", CPUPerc: "12.50%", MemUsage: "32.0 MiB", IO: "1 B / 2 B", PIDs: "3"}
	for _, format := range []string{"", "table", "table {{.Name}}\t{{.CPUPerc}}", "{{.Name}}:{{.CPUPerc}}", "json", "{{json .}}"} {
		render, err := newStatsRenderer(format)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := render.render(&out, []statsRow{row}, false); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if !strings.Contains(text, "worker") || !strings.Contains(text, "12.50%") || strings.Contains(text, "\x1b") {
			t.Fatalf("%s: %s", format, text)
		}
		if render.table && !strings.Contains(text, "CPU %") {
			t.Fatalf("missing headings: %s", text)
		}
		if format == "json" || format == "{{json .}}" {
			var decoded statsRow
			if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded != row {
				t.Fatalf("JSON: %+v %v", decoded, err)
			}
		}
	}
	for _, format := range []string{"{{", "{{.NetIO}}", "{{.MemPerc}}", "{{.Missing}}"} {
		if _, err := newStatsRenderer(format); err == nil {
			t.Errorf("accepted format %s", format)
		}
	}
	render, _ := newStatsRenderer("")
	var out bytes.Buffer
	if err := render.render(&out, nil, true); err != nil || !strings.HasPrefix(out.String(), "\x1b[H\x1b[2J") {
		t.Fatalf("redraw: %q %v", out.String(), err)
	}
	if err := render.render(statsFailWriter{}, []statsRow{row}, false); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write error: %v", err)
	}
	if terminal, restore := statsConsole(&out); terminal {
		restore()
		t.Fatal("buffer recognized as a console")
	} else {
		restore()
	}
}

type statsFailWriter struct{}

func (statsFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestStatsCollectionAndRunChanges(t *testing.T) {
	ctx, _, records := psFixture(t)
	rec := *records["web-paused"]
	selected := []statsContainer{{rec, "chosen"}, {*records["web-created"], ""}}
	when := time.Now()
	current := run.StatsSample{RunID: "first", SampledAt: when, JobStats: win32.JobStats{CPUTime: 100, PrivateMemory: 4096, ReadBytes: 1024, WriteBytes: 2048, PIDs: 2}}
	sample := func(stdcontext.Context, *context.Context, *store.ContainerRecord) (run.StatsSample, error) {
		return current, nil
	}
	rows, baseline, err := collectStats(stdcontext.Background(), ctx, selected, statsOptions{}, nil, 8, sample)
	if err != nil || len(rows) != 2 || rows[0].Container != "chosen" || rows[0].ID != rec.ShortID() || rows[0].CPUPerc != "--" || rows[0].MemUsage != "4.0 KiB" || rows[0].IO != "1.0 KiB / 2.0 KiB" || rows[1].PIDs != "0" {
		t.Fatalf("first sample: %+v %v", rows, err)
	}
	current.SampledAt = when.Add(time.Second)
	current.CPUTime += 10000000
	rows, baseline, err = collectStats(stdcontext.Background(), ctx, selected, statsOptions{noTrunc: true}, baseline, 8, sample)
	if err != nil || rows[0].CPUPerc != "12.50%" || rows[0].ID != rec.ID {
		t.Fatalf("second sample: %+v %v", rows, err)
	}
	current.RunID, current.CPUTime = "replacement", 1
	current.SampledAt = when.Add(2 * time.Second)
	rows, _, err = collectStats(stdcontext.Background(), ctx, selected, statsOptions{}, baseline, 8, sample)
	if err != nil || rows[0].CPUPerc != "--" {
		t.Fatalf("restart: %+v %v", rows, err)
	}
	failed := func(stdcontext.Context, *context.Context, *store.ContainerRecord) (run.StatsSample, error) {
		return run.StatsSample{}, run.ErrStatsRunChanged
	}
	rows, baseline, err = collectStats(stdcontext.Background(), ctx, selected, statsOptions{}, baseline, 8, failed)
	if err != nil || len(rows) != 1 || len(baseline) != 0 {
		t.Fatalf("raced exit: %+v %v", rows, err)
	}
	failed = func(stdcontext.Context, *context.Context, *store.ContainerRecord) (run.StatsSample, error) {
		return run.StatsSample{}, io.ErrUnexpectedEOF
	}
	if _, _, err := collectStats(stdcontext.Background(), ctx, selected, statsOptions{}, nil, 8, failed); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("query failure: %v", err)
	}
}

func TestStatsSnapshotAndCancellation(t *testing.T) {
	ctx, out, _ := psFixture(t)
	renderer, _ := newStatsRenderer("json")
	calls := 0
	sample := func(stdcontext.Context, *context.Context, *store.ContainerRecord) (run.StatsSample, error) {
		calls++
		return run.StatsSample{RunID: "one", SampledAt: time.Now(), JobStats: win32.JobStats{CPUTime: uint64(calls), PIDs: 1}}, nil
	}
	opts := statsOptions{noStream: true, targets: []string{"web-paused"}}
	if err := streamStats(stdcontext.Background(), ctx, opts, renderer, time.Millisecond, sample, 8, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || bytes.Count(out.Bytes(), []byte("\n")) != 1 || strings.Contains(out.String(), `"CPUPerc":"--"`) {
		t.Fatalf("snapshot calls=%d output=%s", calls, out)
	}
	cancel, stop := stdcontext.WithCancel(stdcontext.Background())
	stop()
	if err := streamStats(cancel, ctx, statsOptions{}, renderer, time.Second, sample, 8, false); !errors.Is(err, stdcontext.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}
