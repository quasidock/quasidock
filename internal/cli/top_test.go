package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestParseTopOptions(t *testing.T) {
	for _, args := range [][]string{{"worker", "--format=json"}, {"--format", "json", "worker"}, {"--format=json", "--", "worker"}} {
		opts, err := parseTopOptions(args)
		if err != nil || opts.container != "worker" || opts.format != "json" {
			t.Fatalf("%v: %+v %v", args, opts, err)
		}
	}
	for _, args := range [][]string{nil, {""}, {"one", "two"}, {"--format"}, {"--format=json"}, {"worker", "-ef"}, {"--all", "worker"}, {"worker", "--no-stream"}} {
		if _, err := parseTopOptions(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestTopRows(t *testing.T) {
	parent, ticks := uint32(0), uint64((123*3600+4*60+5)*10000000+6789999)
	input := []win32.ProcessInfo{{PID: 100, Command: "fallback.exe"}, {PID: 9, PPID: &parent, CPUTime: &ticks, Command: `"app.exe" "full command 雪"`}}
	rows := topRows(input)
	if len(rows) != 2 || rows[0].PID != "9" || rows[0].PPID != "0" || rows[0].Time != "123:04:05.678" || rows[0].Command != input[1].Command || rows[1].PPID != "--" || rows[1].Time != "--" {
		t.Fatalf("rows: %+v", rows)
	}
	if input[0].PID != 100 {
		t.Fatal("reordered caller's process list")
	}
	zero, maximum := uint64(0), uint64(math.MaxUint64)
	rows = topRows([]win32.ProcessInfo{{PID: 1, CPUTime: &zero}, {PID: 2, CPUTime: &maximum}})
	if rows[0].Time != "00:00:00.000" || rows[1].Time != "512409557:36:10.955" {
		t.Fatalf("CPU time limits: %+v", rows)
	}
}

func TestTopRendering(t *testing.T) {
	row := topRow{PID: "12", PPID: "1", Time: "00:00:01.234", Command: "app.exe \"雪\"\tline\r\n\x1b[2J\u0085\u2028"}
	for _, format := range []string{"", "table", "table {{.PID}}\t{{.Command}}", "{{.PID}}:{{.Command}}", "json", "{{json .}}"} {
		renderer, err := newTopRenderer(format)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := renderer.render(&out, []topRow{row}); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if renderer.table {
			if strings.Count(text, "\n") != 2 || strings.ContainsAny(text, "\r\x1b\u0085\u2028") || !strings.Contains(text, `\tline\r\n\x1b[2J`) || !strings.Contains(text, "COMMAND") {
				t.Fatalf("unsafe table %q: %q", format, text)
			}
		} else if format == "json" || format == "{{json .}}" {
			var decoded topRow
			if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded != row {
				t.Fatalf("JSON round trip: %+v %v", decoded, err)
			}
		} else if !strings.Contains(text, row.Command) {
			t.Fatalf("plain template changed command: %q", text)
		}
		if err := renderer.render(statsFailWriter{}, []topRow{row}); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("writer error: %v", err)
		}
	}
	for _, format := range []string{"{{", "{{.Name}}", "{{.CPU}}", "{{.Missing}}"} {
		if _, err := newTopRenderer(format); err == nil {
			t.Errorf("accepted %q", format)
		}
	}
	for _, format := range []string{"table", "json", "{{.PID}}"} {
		renderer, _ := newTopRenderer(format)
		var out bytes.Buffer
		if err := renderer.render(&out, nil); err != nil {
			t.Fatal(err)
		}
		if (format == "table") != strings.Contains(out.String(), "PID") {
			t.Fatalf("empty %s: %q", format, out.String())
		}
	}
}

func TestTopCommandValidationAndHelp(t *testing.T) {
	ctx := context.New()
	var output bytes.Buffer
	ctx.Stdout = &output
	// A nil store proves invalid templates are rejected before resolution or I/O.
	if code, err := topCommand(ctx, []string{"missing", "--format={{.Missing}}"}); code != 1 || err == nil || output.Len() != 0 {
		t.Fatalf("invalid template: %d %v %s", code, err, &output)
	}
	for _, args := range [][]string{{"top", "--help"}, {"container", "top", "-h"}} {
		ctx, _ := fixtureCLI(t)
		t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
		ctx.Stdout = &output
		if code, err := Dispatch(ctx, args); code != 0 || err != nil || !strings.Contains(output.String(), topUsage) {
			t.Fatalf("help %v: %d %v %s", args, code, err, &output)
		}
		output.Reset()
	}
	ctx, _, _ = psFixture(t)
	ctx.Stdout = &output
	for _, name := range []string{"web-created", "archive", "missing"} {
		if code, err := topCommand(ctx, []string{name}); code != 1 || err == nil || output.Len() != 0 {
			t.Fatalf("state %s: %d %v %s", name, code, err, &output)
		}
	}
}
