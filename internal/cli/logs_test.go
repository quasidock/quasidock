package cli

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/containerlog"
	"github.com/quasidock/quasidock/internal/store"
)

func TestParseLogOptions(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.FixedZone("Bangkok", 7*60*60))
	opts, err := parseLogOptions([]string{"--since", "1h", "web", "--until=10m", "-f", "-t", "--details", "-n3"}, now)
	if err != nil || opts.target != "web" || opts.tail != 3 || !opts.follow || !opts.timestamps || !opts.details || !opts.since.Equal(now.Add(-time.Hour)) || !opts.until.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("options: %+v %v", opts, err)
	}
	for _, args := range [][]string{
		{"-n", "3"}, {"-n=3"}, {"--tail", "3"}, {"--tail=3"},
	} {
		opts, err := parseLogOptions(append(args, "web"), now)
		if err != nil || opts.tail != 3 {
			t.Errorf("tail %v: %+v %v", args, opts, err)
		}
	}
	opts, err = parseLogOptions([]string{"--tail=all", "--timestamps=false", "--details=false", "--follow=false", "--", "web"}, now)
	if err != nil || opts.tail != -1 || opts.timestamps || opts.details || opts.follow {
		t.Fatalf("boolean options: %+v %v", opts, err)
	}
	for _, value := range []string{"0", "123.000000007", "2026-11-26T05:00:00Z", "2026-11-26T12:00:00", "2026-11-26", "-1h"} {
		want, _ := parseCommandTime(value, now)
		opts, err := parseLogOptions([]string{"--since=" + value, "web"}, now)
		if err != nil || !opts.since.Equal(want) {
			t.Errorf("time %q: %+v %v", value, opts, err)
		}
	}
	for _, args := range [][]string{
		nil, {""}, {"a", "b"}, {"web", "--since"}, {"web", "--until="},
		{"web", "--since=NaN"}, {"web", "--since=1.1234567890"},
		{"web", "--since=5m", "--until=10m"}, {"web", "--timestamps=maybe"},
		{"web", "--details="}, {"web", "--unknown"}, {"web", "--tail=-1"}, {"web", "--tail=1.5"},
	} {
		if _, err := parseLogOptions(args, now); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func writeTimedLog(t *testing.T, path string, data string, spans []containerlog.Span) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var metadata bytes.Buffer
	metadata.WriteString("{\"version\":1}\n")
	for _, span := range spans {
		if err := json.NewEncoder(&metadata).Encode(span); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(containerlog.IndexPath(path), metadata.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLogsFilteringFormattingAndAliases(t *testing.T) {
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	var warnings bytes.Buffer
	ctx.Stderr = &warnings
	rec, err := ctx.Store.CreateContainer("log-options", store.ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	data := "old\nfirst\r\nsplit-end\nlast"
	stamp := time.Unix(10, 7)
	writeTimedLog(t, rec.LogPath, data, []containerlog.Span{
		{Offset: 4, Length: 7, Time: stamp},
		{Offset: 11, Length: 6, Time: stamp.Add(time.Second)},
		{Offset: 17, Length: 4, Time: stamp.Add(2 * time.Second)},
		{Offset: 21, Length: 4, Time: stamp.Add(3 * time.Second)},
	})
	for _, prefix := range [][]string{{"logs"}, {"container", "logs"}} {
		for _, tc := range []struct {
			args []string
			want string
			warn bool
		}{
			{nil, data, false},
			{[]string{"--details"}, data, false},
			{[]string{"--timestamps=false"}, data, false},
			{[]string{"-t"}, "1970-01-01T00:00:10.000000007Z first\r\n1970-01-01T00:00:11.000000007Z split-end\n1970-01-01T00:00:13.000000007Z last", true},
			{[]string{"--since=10.000000007", "--until=11.000000007"}, "first\r\n", true},
			{[]string{"--since=11.000000007", "--until=13.000000007"}, "split-end\n", true},
			{[]string{"--since=12"}, "last", true},
			{[]string{"--tail=2", "--until=13.000000007"}, "split-end\n", false},
			{[]string{"--tail=1", "--until=13.000000007"}, "", false},
			{[]string{"--tail=0", "-t"}, "", false},
			{[]string{"--since=0", "--until=0", "-f"}, "", false},
		} {
			out.Reset()
			warnings.Reset()
			args := append(append(append([]string{}, prefix...), tc.args...), rec.BareName())
			code, err := Dispatch(ctx, args)
			if err != nil || code != 0 || out.String() != tc.want || (warnings.Len() > 0) != tc.warn || strings.Count(warnings.String(), "warning:") > 1 {
				t.Fatalf("%v: code=%d output=%q warnings=%q err=%v; want %q warn=%t", args, code, out, &warnings, err, tc.want, tc.warn)
			}
		}
	}
}

func TestLogsLegacyCorruptAndMissingMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.log")
	if err := os.WriteFile(path, []byte("legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, metadata := range []string{"", "{\"version\":99}\n", "{\"version\":1}\nbroken\n", "{\"version\":1}\n{\"offset\":"} {
		if metadata != "" {
			if err := os.WriteFile(containerlog.IndexPath(path), []byte(metadata), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, timed := range []bool{false, true} {
			var out, warnings bytes.Buffer
			opts := logOptions{tail: -1, timestamps: timed}
			err := streamLogs(stdcontext.Background(), path, func() bool { return false }, opts, &out, &warnings, time.Millisecond)
			corrupt := metadata != "" && !strings.HasSuffix(metadata, ":")
			if timed && corrupt {
				if err == nil {
					t.Fatalf("accepted corrupt metadata %q", metadata)
				}
			} else if err != nil || timed && (out.Len() != 0 || warnings.Len() == 0) || !timed && (out.String() != "legacy\n" || warnings.Len() > 0) {
				t.Fatalf("metadata=%q timed=%t: output=%q warnings=%q err=%v", metadata, timed, &out, &warnings, err)
			}
		}
	}
	var out bytes.Buffer
	if err := streamLogs(stdcontext.Background(), path+"-absent", func() bool { return false }, logOptions{tail: -1, timestamps: true}, &out, io.Discard, time.Millisecond); err != nil || out.Len() > 0 {
		t.Fatalf("unstarted container logs: %q %v", &out, err)
	}
}

func TestLogsFollowPartialLinesAndPublication(t *testing.T) {
	for _, tail := range []int{-1, 0} {
		t.Run(strconv.Itoa(tail), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "container.log")
			writeTimedLog(t, path, "first-", []containerlog.Span{{Offset: 0, Length: 6, Time: time.Unix(10, 1)}})
			var out, warnings bytes.Buffer
			s := logStream{ctx: stdcontext.Background(), opts: logOptions{tail: tail, timestamps: true}, out: &out, warnings: &warnings, index: containerlog.NewIndexReader(path)}
			if err := s.consume(path, false); err != nil {
				t.Fatal(err)
			}
			prefix := "1970-01-01T00:00:10.000000001Z "
			want := prefix + "first-"
			if tail == 0 {
				want = ""
			}
			if out.String() != want {
				t.Fatalf("partial replay: %q", &out)
			}
			// A raw append must not advance the timed reader before its index is
			// published. Complete the index record in two separate polls.
			appendLogFile(t, path, []byte("end\nnext"))
			if err := s.consume(path, false); err != nil || out.String() != want {
				t.Fatalf("unpublished bytes: %q %v", &out, err)
			}
			record, _ := json.Marshal(containerlog.Span{Offset: 6, Length: 8, Time: time.Unix(20, 2)})
			appendLogFile(t, containerlog.IndexPath(path), record[:len(record)/2])
			if err := s.consume(path, false); err != nil || out.String() != want {
				t.Fatalf("partial index: %q %v", &out, err)
			}
			appendLogFile(t, containerlog.IndexPath(path), append(record[len(record)/2:], '\n'))
			if err := s.consume(path, false); err != nil {
				t.Fatal(err)
			}
			want = prefix + "first-end\n1970-01-01T00:00:20.000000002Z next"
			if tail == 0 {
				want = prefix + "end\n1970-01-01T00:00:20.000000002Z next"
			}
			if out.String() != want || warnings.Len() > 0 {
				t.Fatalf("follow duplicated/lost data: output=%q warnings=%q", &out, &warnings)
			}
		})
	}
}

func appendLogFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}

func TestLogsUntimedFragmentAndInitiallyMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.log")
	stamp := time.Unix(10, 0)
	writeTimedLog(t, path, "legacy-end\nnew\n", []containerlog.Span{{Offset: 7, Length: 8, Time: stamp}})
	var out, warnings bytes.Buffer
	if err := streamLogs(stdcontext.Background(), path, func() bool { return false }, logOptions{tail: -1, timestamps: true}, &out, &warnings, time.Millisecond); err != nil || out.String() != "1970-01-01T00:00:10.000000000Z new\n" || warnings.Len() == 0 {
		t.Fatalf("untimed fragment received a timestamp: output=%q warning=%q err=%v", &out, &warnings, err)
	}
	out.Reset()
	warnings.Reset()
	missing := path + "-new"
	s := logStream{ctx: stdcontext.Background(), opts: logOptions{tail: 0, timestamps: true}, out: &out, warnings: &warnings, index: containerlog.NewIndexReader(missing)}
	if err := s.consume(missing, false); err != nil {
		t.Fatal(err)
	}
	writeTimedLog(t, missing, "new\n", []containerlog.Span{{Offset: 0, Length: 4, Time: stamp}})
	if err := s.consume(missing, false); err != nil || out.String() != "1970-01-01T00:00:10.000000000Z new\n" || warnings.Len() > 0 {
		t.Fatalf("tail=0 lost first live output: output=%q warning=%q err=%v", &out, &warnings, err)
	}
}

func TestLogsLongLinesAndBytePreservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.log")
	data := strings.Repeat("a", 100_000) + "\x00\xff\r\n日本語"
	stamp := time.Unix(1, 0)
	writeTimedLog(t, path, data, []containerlog.Span{{Offset: 0, Length: int64(len(data)), Time: stamp}})
	var out bytes.Buffer
	if err := streamLogs(stdcontext.Background(), path, func() bool { return false }, logOptions{tail: -1, timestamps: true}, &out, io.Discard, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	prefix := "1970-01-01T00:00:01.000000000Z "
	if out.String() != prefix+strings.ReplaceAll(data, "\n", "\n"+prefix) {
		t.Fatal("timestamp rendering changed bytes or duplicated long-line prefixes")
	}
	out.Reset()
	if err := streamLogs(stdcontext.Background(), path, func() bool { return false }, logOptions{tail: 1}, &out, io.Discard, time.Millisecond); err != nil || out.String() != "日本語" {
		t.Fatalf("long line tail: %q %v", &out, err)
	}
}

func TestLogsDeadlineCancellationAndErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.log")
	writer, err := containerlog.Open(path, func(err error) { t.Errorf("warning: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	deadline := time.Now().Add(30 * time.Millisecond)
	if err := streamLogs(stdcontext.Background(), path, func() bool { return true }, logOptions{tail: 0, follow: true, until: &deadline}, io.Discard, io.Discard, time.Millisecond); err != nil || time.Now().Before(deadline) {
		t.Fatalf("idle deadline: %v", err)
	}
	// A future cutoff without --follow must return its current snapshot.
	future := time.Now().Add(time.Hour)
	if err := streamLogs(stdcontext.Background(), path, func() bool { return true }, logOptions{tail: -1, until: &future}, io.Discard, io.Discard, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()
	if err := streamLogs(cancelCtx, path, func() bool { return true }, logOptions{tail: -1, follow: true}, io.Discard, io.Discard, time.Millisecond); !errors.Is(err, stdcontext.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := writer.Write([]byte("output\n")); err != nil {
		t.Fatal(err)
	}
	for _, timed := range []bool{false, true} {
		if err := streamLogs(stdcontext.Background(), path, func() bool { return false }, logOptions{tail: -1, timestamps: timed}, failedEventWriter{}, io.Discard, time.Millisecond); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("ignored output error (timed=%t): %v", timed, err)
		}
	}
}

func TestLogsReplayToFollowFinalDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.log")
	writer, err := containerlog.Open(path, func(err error) { t.Errorf("warning: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Write([]byte("replay\n")); err != nil {
		t.Fatal(err)
	}
	out := &eventGateWriter{entered: make(chan struct{}), release: make(chan struct{}), lines: make(chan string, 4)}
	var running atomic.Bool
	running.Store(true)
	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	since := time.Unix(0, 0)
	go func() {
		done <- streamLogs(ctx, path, running.Load, logOptions{tail: -1, follow: true, since: &since}, out, io.Discard, time.Millisecond)
	}()
	select {
	case <-out.entered:
	case <-ctx.Done():
		t.Fatal("replay did not start")
	}
	if _, err := writer.Write([]byte("live\n")); err != nil {
		close(out.release)
		t.Fatal(err)
	}
	running.Store(false)
	close(out.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	close(out.lines)
	var got string
	for data := range out.lines {
		got += data
	}
	if got != "replay\nlive\n" {
		t.Fatalf("replay/follow lost or duplicated output: %q", got)
	}
}

func TestLogsDeadlineDrainsBlockedReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.log")
	writer, err := containerlog.Open(path, func(err error) { t.Errorf("warning: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Write([]byte("replay\n")); err != nil {
		t.Fatal(err)
	}
	out := &eventGateWriter{entered: make(chan struct{}), release: make(chan struct{}), lines: make(chan string, 4)}
	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 10*time.Second)
	defer cancel()
	since := time.Unix(0, 0)
	deadline := time.Now().Add(2 * time.Second)
	done := make(chan error, 1)
	go func() {
		done <- streamLogs(ctx, path, func() bool { return true }, logOptions{tail: -1, follow: true, since: &since, until: &deadline}, out, io.Discard, time.Millisecond)
	}()
	select {
	case <-out.entered:
	case <-ctx.Done():
		t.Fatal("replay did not start")
	}
	if _, err := writer.Write([]byte("before-deadline\n")); err != nil {
		close(out.release)
		t.Fatal(err)
	}
	<-time.After(time.Until(deadline))
	close(out.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	close(out.lines)
	var got string
	for data := range out.lines {
		got += data
	}
	if got != "replay\nbefore-deadline\n" {
		t.Fatalf("deadline lost output published during replay: %q", got)
	}
}
