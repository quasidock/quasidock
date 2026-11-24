package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
)

func psFixture(t *testing.T) (*context.Context, *bytes.Buffer, map[string]*store.ContainerRecord) {
	t.Helper()
	ctx, out := fixtureCLI(t)
	records := make(map[string]*store.ContainerRecord)
	for i, item := range []struct{ name, status string }{
		{"archive", "exited"}, {"web-created", "created"}, {"web-running", "running"},
		{"web-paused", "paused"}, {"web-exited", "exited"}, {"failed", "exited"},
	} {
		state := store.ContainerState{Status: item.status, ExitCode: 0}
		if item.status != "created" && item.name != "failed" {
			state.StartedAt = "2026-01-02T03:04:06Z"
			state.Launched = true
		}
		state.Running = item.status == "running" || item.status == "paused"
		state.Paused = item.status == "paused"
		if item.name == "archive" {
			state.ExitCode = 7
		}
		c, err := ctx.Store.CreateContainer(item.name, store.ContainerRecord{
			Created:  "2026-01-02T03:04:0" + string(rune('0'+i)) + "Z",
			ImageRef: "example:1", Path: `C:\apps\long-command.exe`,
			Args: []string{"a-very-long-argument-that-must-not-be-truncated"}, State: state,
		})
		if err != nil {
			t.Fatal(err)
		}
		if state.Running {
			lock, err := store.OpenLock(ctx.Store.ContainerRunLockPath(c.ID), true, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(lock.Release)
		}
		records[item.name] = c
	}
	return ctx, out, records
}

func TestParseContainerListOptions(t *testing.T) {
	for _, args := range [][]string{
		{"-aqs", "-n2", "-fname=web", "--no-trunc"},
		{"--all", "--quiet", "--size", "--last=2", "--filter=name=web", "--no-trunc=true"},
		{"-qa", "-s=true", "-n=2", "-f=name=web", "--no-trunc"},
		{"-a", "-q", "-s", "--last", "2", "--filter", "name=web", "--no-trunc"},
		{"-aqsn2", "-f", "name=web", "--no-trunc"},
	} {
		opts, err := parseContainerListOptions(args)
		if err != nil || !opts.all || !opts.quiet || !opts.size || !opts.noTrunc || opts.last != 2 || len(opts.filters.names) != 1 {
			t.Errorf("%v: %+v %v", args, opts, err)
		}
	}
	opts, err := parseContainerListOptions([]string{"-aqs", "--all=false", "--quiet=false", "--size=false", "--no-trunc", "--no-trunc=false", "--"})
	if err != nil || opts.all || opts.quiet || opts.size || opts.noTrunc || opts.last != -1 {
		t.Fatalf("boolean overrides: %+v %v", opts, err)
	}
	for _, args := range [][]string{
		{"--filter"}, {"-f"}, {"--last"}, {"-n"}, {"--last="}, {"-n="},
		{"--last=one"}, {"--last=9999999999999999999999999"},
		{"--size=maybe"}, {"--no-trunc="}, {"-aqs=maybe"},
		{"--filter="}, {"-f", "name"}, {"-f", "name="}, {"-f", "=web"},
		{"-f", "name=["}, {"-f", "status=unknown"}, {"-f", "status=RUNNING"},
		{"-f", "exited=abc"}, {"-f", "exited=9999999999999999999999999"},
		{"-f", "ancestor=image"}, {"--unknown"}, {"--a"}, {"--q"}, {"--s"},
		{"-aqx"}, {"--format=json"}, {"container"}, {"-"}, {"--", "container"},
	} {
		if _, err := parseContainerListOptions(args); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
}

func TestContainerListSelection(t *testing.T) {
	ctx, out, recs := psFixture(t)
	for _, tt := range []struct {
		name string
		args []string
		want []string
	}{
		{"running by default", nil, []string{"web-paused", "web-running"}},
		{"all", []string{"-a"}, []string{"failed", "web-exited", "web-paused", "web-running", "web-created", "archive"}},
		{"positive limit includes all states", []string{"--last=2"}, []string{"failed", "web-exited"}},
		{"zero limit", []string{"-n0"}, []string{"web-paused", "web-running"}},
		{"negative limit", []string{"-n", "-2"}, []string{"web-paused", "web-running"}},
		{"unlimited all", []string{"-a", "-n=-1", "-f", "name=^web-"}, []string{"web-exited", "web-paused", "web-running", "web-created"}},
		{"limit after filters", []string{"-n1", "-f", "name=web"}, []string{"web-exited"}},
		{"status includes stopped", []string{"-f", "status=exited"}, []string{"failed", "web-exited", "archive"}},
		{"running excludes paused", []string{"-fstatus=running"}, []string{"web-running"}},
		{"paused", []string{"-fstatus=paused"}, []string{"web-paused"}},
		{"created", []string{"-fstatus=created"}, []string{"web-created"}},
		{"statuses OR", []string{"-fstatus=created", "-fstatus=paused"}, []string{"web-paused", "web-created"}},
		{"exited alone retains visibility", []string{"-fexited=0"}, nil},
		{"exited excludes failed and unstarted", []string{"-a", "-fexited=0"}, []string{"web-exited"}},
		{"exit codes OR", []string{"-a", "-fexited=0", "-fexited=7"}, []string{"web-exited", "archive"}},
		{"exited with positive limit", []string{"-n1", "-fexited=7"}, []string{"archive"}},
		{"different keys AND", []string{"-fname=web", "-fstatus=exited", "-fexited=7"}, nil},
		{"names OR", []string{"-a", "-fname=archive", "-fname=^/web-created$"}, []string{"web-created", "archive"}},
		{"bare name anchor", []string{"-a", "-fname=^web-created$"}, []string{"web-created"}},
		{"name case", []string{"-a", "-fname=WEB"}, nil},
		{"ID prefix", []string{"-a", "-fid=" + strings.ToUpper(recs["web-exited"].ShortID())}, []string{"web-exited"}},
		{"IDs OR", []string{"-a", "-fid=" + recs["archive"].ID, "-fid=" + recs["web-created"].ID}, []string{"web-created", "archive"}},
		{"before includes only visible", []string{"-fbefore=web-exited"}, []string{"web-paused", "web-running"}},
		{"before by prefix", []string{"-a", "-fbefore=" + recs["web-running"].ShortID()}, []string{"web-created", "archive"}},
		{"since by full ID", []string{"-a", "-fsince=" + recs["web-paused"].ID}, []string{"failed", "web-exited"}},
		{"reference uses full list", []string{"-fstatus=running", "-fbefore=/WEB-EXITED"}, []string{"web-running"}},
		{"before OR", []string{"-a", "-fbefore=web-exited", "-fbefore=web-created"}, []string{"web-paused", "web-running", "web-created", "archive"}},
		{"since OR", []string{"-a", "-fsince=web-paused", "-fsince=web-created"}, []string{"failed", "web-exited", "web-paused", "web-running"}},
		{"bounded range and limit", []string{"-n1", "-fbefore=web-exited", "-fsince=web-created"}, []string{"web-paused"}},
		{"empty range", []string{"-a", "-fbefore=web-running", "-fsince=web-running"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out.Reset()
			args := append(append([]string(nil), tt.args...), "-q", "--no-trunc")
			if err := containerList(ctx, args); err != nil {
				t.Fatal(err)
			}
			var want string
			for _, name := range tt.want {
				want += recs[name].ID + "\n"
			}
			if out.String() != want {
				t.Fatalf("%v: %q, want %q", args, out.String(), want)
			}
		})
	}
}

func TestContainerListAliasesAndRendering(t *testing.T) {
	ctx, out, recs := psFixture(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	for _, prefix := range [][]string{{"ps"}, {"container", "ls"}, {"container", "list"}} {
		out.Reset()
		args := append(prefix, "-aq", "--no-trunc", "-n1", "-fname=web")
		if code, err := Dispatch(ctx, args); code != 0 || err != nil || out.String() != recs["web-exited"].ID+"\n" {
			t.Fatalf("%v: %d %v %q", args, code, err, out.String())
		}
	}
	c := recs["web-exited"]
	if err := os.WriteFile(filepath.Join(c.DataDir, "data"), bytes.Repeat([]byte("x"), 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.LogPath, []byte("excluded log"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An unselected container must not be scanned.
	if err := os.Remove(recs["archive"].DataDir); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := containerList(ctx, []string{"-as", "--no-trunc", "-fname=^web-exited$"}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"SIZE", "2.0 KiB", c.ID, c.Path, c.Args[0]} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("missing %q from %q", text, out.String())
		}
	}
	out.Reset()
	if err := containerList(ctx, []string{"-a", "--size=false", "--no-trunc=false", "-fname=^web-exited$"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SIZE") || strings.Contains(out.String(), c.ID) || strings.Contains(out.String(), c.Path) || !strings.Contains(out.String(), "...") {
		t.Fatalf("default truncation: %q", out.String())
	}
	for _, args := range [][]string{{"-aqs"}, {"-a", "--size=false"}} {
		out.Reset()
		if err := containerList(ctx, args); err != nil {
			t.Fatalf("unneeded size scan for %v: %v", args, err)
		}
	}
	out.Reset()
	if err := containerList(ctx, []string{"-as", "-fname=missing"}); err != nil || len(strings.Split(strings.TrimSpace(out.String()), "\n")) != 1 || !strings.Contains(out.String(), "SIZE") {
		t.Fatalf("empty table: %v %q", err, out.String())
	}
	ctx.Stdout = diffErrorWriter{errors.New("closed")}
	for _, args := range [][]string{nil, {"-q"}} {
		if err := containerList(ctx, args); err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("output error for %v: %v", args, err)
		}
	}
}

func TestContainerListErrorsHaveNoOutput(t *testing.T) {
	ctx, out, recs := psFixture(t)
	for _, args := range [][]string{{"-a", "--unknown"}, {"-fstatus=invalid"}, {"-fbefore=missing"}, {"-fsince=missing"}} {
		out.Reset()
		if err := containerList(ctx, args); err == nil || out.Len() != 0 {
			t.Fatalf("%v: %v %q", args, err, out.String())
		}
	}
	other := *recs["archive"]
	other.ID = other.ID[:12] + strings.Repeat("0", 52)
	if other.ID == recs["archive"].ID {
		other.ID = other.ID[:12] + strings.Repeat("1", 52)
	}
	other.Name = "/ambiguous"
	if err := os.MkdirAll(ctx.Store.ContainerDataDir(other.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctx.Store.ContainerStatePath(other.ID), data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"before", "since"} {
		if err := containerList(ctx, []string{"-f", key + "=" + other.ShortID()}); err == nil || !strings.Contains(err.Error(), "multiple containers") || out.Len() != 0 {
			t.Fatalf("ambiguous %s: %v %q", key, err, out.String())
		}
	}
	// A newer row succeeds before an older row fails, but neither is printed.
	if err := os.Remove(recs["web-created"].DataDir); err != nil {
		t.Fatal(err)
	}
	if err := containerList(ctx, []string{"-as", "-fname=web"}); err == nil || out.Len() != 0 {
		t.Fatalf("partial size output: %v %q", err, out.String())
	}
}

func TestFormatContainerCommand(t *testing.T) {
	for _, tt := range []struct {
		record store.ContainerRecord
		want   string
	}{
		{store.ContainerRecord{Path: `C:\tools\app.exe`, Args: []string{"one", "two"}}, `"C:\tools\app.exe one two"`},
		{store.ContainerRecord{Config: store.ContainerConfig{Entrypoint: []string{"app.exe", "--entry"}, Cmd: []string{"one", "two"}}}, `"app.exe --entry one two"`},
		{store.ContainerRecord{Args: []string{"arg"}, Config: store.ContainerConfig{Entrypoint: []string{"app.exe"}, Cmd: []string{"arg"}}}, `"app.exe arg"`},
		{store.ContainerRecord{Config: store.ContainerConfig{Cmd: []string{"app.exe", "one"}}}, `"app.exe one"`},
	} {
		if got := formatContainerCommand(tt.record, true); got != tt.want {
			t.Errorf("%+v: %q, want %q", tt.record, got, tt.want)
		}
	}
	c := store.ContainerRecord{Path: `C:\tools\app.exe`, Args: []string{strings.Repeat("界", 30)}}
	if got := formatContainerCommand(c, false); !utf8.ValidString(got) || !strings.HasSuffix(got, `..."`) || strings.Contains(got, `C:\tools`) {
		t.Fatalf("short command: %q", got)
	}
	if !reflect.DeepEqual(c.Args, []string{strings.Repeat("界", 30)}) {
		t.Fatal("formatting changed arguments")
	}
}
