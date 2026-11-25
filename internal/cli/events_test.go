package cli

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/store"
)

func TestParseEventOptionsAndTimes(t *testing.T) {
	zone := time.FixedZone("Bangkok", 7*60*60)
	now := time.Date(2026, 11, 25, 12, 30, 0, 0, zone)
	for _, tc := range []struct {
		input string
		want  time.Time
	}{
		{"0", time.Unix(0, 0)},
		{"123.000000007", time.Unix(123, 7)},
		{"-1.5", time.Unix(-2, 500000000)},
		{"2026-11-25T05:30:00.123Z", time.Date(2026, 11, 25, 5, 30, 0, 123000000, time.UTC)},
		{"2026-11-25T12:30:00+07:00", now},
		{"2026-11-25T12:30:00.000000001", now.Add(time.Nanosecond)},
		{"2026-11-25T12:30", now},
		{"2026-11-25", time.Date(2026, 11, 25, 0, 0, 0, 0, zone)},
		{"1h30m", now.Add(-90 * time.Minute)},
		{"-10s", now.Add(10 * time.Second)},
	} {
		got, err := parseEventTime(tc.input, now)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("%q: %v %v, want %v", tc.input, got, err, tc.want)
		}
	}
	opts, err := parseEventOptions([]string{"--since", "1h", "--until=10m", "-f", "event=start", "--filter=event=die", "--filter", "container=/web", "--format=json", "--"}, now)
	if err != nil || !opts.since.Equal(now.Add(-time.Hour)) || !opts.until.Equal(now.Add(-10*time.Minute)) || opts.format != "json" || len(opts.filters["event"]) != 2 {
		t.Fatalf("parse: %+v %v", opts, err)
	}
	for _, args := range [][]string{
		{"--since"}, {"--until="}, {"--since", "NaN"}, {"--since", "1."},
		{"--since", "1.1234567890"}, {"--since", "1.-2"}, {"--since", "2026-02-30"},
		{"--since=5m", "--until=10m"}, {"--filter"}, {"--filter", "event"},
		{"--filter", "event="}, {"--filter", "label=a"}, {"--filter", "type=image"},
		{"--format"}, {"--follow"}, {"--", "web"}, {"web"},
	} {
		if _, err := parseEventOptions(args, now); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestEventFiltersAndTimeBoundaries(t *testing.T) {
	event := store.Event{Type: "container", Action: "die", TimeNano: 10000000000,
		Actor: store.EventActor{ID: strings.Repeat("ab", 32), Attributes: map[string]string{"name": "Web", "image": "example:1", "imageID": "sha256:abcdef"}}}
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, true},
		{[]string{"-f", "event=start", "-f", "event=die", "-f", "container=/web", "-f", "type=container"}, true},
		{[]string{"-f", "container=ABAB", "-f", "image=sha256:abcdef"}, true},
		{[]string{"-f", "container=a"}, false},
		{[]string{"-f", "container=ab-other"}, false},
		{[]string{"-f", "event=start", "-f", "container=web"}, false},
		{[]string{"-f", "image=example"}, false},
		{[]string{"-f", "image=example:1"}, true},
		{[]string{"--since=10", "--until=10.000000001"}, true},
		{[]string{"--since=10.000000001"}, false},
		{[]string{"--until=10"}, false},
	} {
		opts, err := parseEventOptions(tc.args, time.Now())
		if err != nil || opts.matches(event) != tc.want {
			t.Errorf("%v: matches=%t %v, want %t", tc.args, opts.matches(event), err, tc.want)
		}
	}
}

type failedEventWriter struct{}

func (failedEventWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestEventRendering(t *testing.T) {
	event := store.Event{Type: "container", Action: "die", Scope: "local", Time: 10, TimeNano: 10000000001,
		Actor: store.EventActor{ID: "full-id", Attributes: map[string]string{"name": "web", "exitCode": "37"}}}
	var out bytes.Buffer
	render, err := eventRenderer(&out, "")
	if err != nil || render(event) != nil {
		t.Fatal(err)
	}
	if want := "1970-01-01T00:00:10.000000001Z container die full-id (exitCode=37, name=web)\n"; out.String() != want {
		t.Fatalf("text: %q, want %q", out.String(), want)
	}
	for _, format := range []string{"json", "{{json .}}"} {
		out.Reset()
		render, err := eventRenderer(&out, format)
		if err != nil || render(event) != nil {
			t.Fatal(err)
		}
		var got store.Event
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || !reflect.DeepEqual(got, event) || strings.Contains(out.String(), "sequence") {
			t.Fatalf("JSON: %s %v", out.String(), err)
		}
	}
	out.Reset()
	render, err = eventRenderer(&out, "{{.Action}} {{.Actor.ID}} {{index .Actor.Attributes \"exitCode\"}}")
	if err != nil || render(event) != nil || out.String() != "die full-id 37\n" {
		t.Fatalf("template: %s %v", out.String(), err)
	}
	if _, err := eventRenderer(&out, "{{"); err == nil {
		t.Fatal("accepted invalid template")
	}
	render, _ = eventRenderer(&out, "{{.MissingField}}")
	if err := render(event); err == nil {
		t.Fatal("ignored template execution error")
	}
	for _, format := range []string{"", "json", "{{json .}}"} {
		render, _ = eventRenderer(failedEventWriter{}, format)
		if err := render(event); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("ignored output error (%q): %v", format, err)
		}
	}
}

type eventGateWriter struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	lines   chan string
}

func (w *eventGateWriter) Write(data []byte) (int, error) {
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	w.lines <- string(data)
	return len(data), nil
}

func TestEventsReplayToLiveAndOverrun(t *testing.T) {
	for _, overrun := range []bool{false, true} {
		t.Run(strconv.FormatBool(overrun), func(t *testing.T) {
			ctx, _ := fixtureCLI(t)
			if err := ctx.Store.AppendEvent(store.Event{Action: "initial"}); err != nil {
				t.Fatal(err)
			}
			writer := &eventGateWriter{entered: make(chan struct{}), release: make(chan struct{}), lines: make(chan string, 300)}
			var warnings bytes.Buffer
			cancelCtx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 20*time.Second)
			defer cancel()
			opts, _ := parseEventOptions([]string{"--since=0", "--format={{.Action}}"}, time.Now())
			done := make(chan error, 1)
			go func() { done <- streamEvents(cancelCtx, ctx.Store, opts, writer, &warnings, time.Millisecond) }()
			select {
			case <-writer.entered:
			case err := <-done:
				t.Fatalf("initial replay: %v", err)
			case <-cancelCtx.Done():
				t.Fatal("no initial replay")
			}
			// While output is blocked, another producer must still acquire the
			// journal lock and publish events; this also exercises the handoff.
			count := 1
			if overrun {
				count = store.EventHistoryLimit + 2
			}
			for i := range count {
				if err := ctx.Store.AppendEvent(store.Event{Action: strconv.Itoa(i)}); err != nil {
					close(writer.release)
					t.Fatalf("slow output blocked producer: %v", err)
				}
			}
			close(writer.release)
			want := []string{"initial\n"}
			first := 0
			if overrun {
				first = 2
			}
			for i := first; i < count; i++ {
				want = append(want, strconv.Itoa(i)+"\n")
			}
			for _, expected := range want {
				select {
				case line := <-writer.lines:
					if line != expected {
						t.Fatalf("handoff duplicated/lost event: %q, want %q", line, expected)
					}
				case err := <-done:
					t.Fatalf("stream ended: %v", err)
				case <-cancelCtx.Done():
					t.Fatal("missing live event")
				}
			}
			cancel()
			if err := <-done; !errors.Is(err, stdcontext.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
			if len(writer.lines) != 0 || strings.Contains(warnings.String(), "fell behind") != overrun {
				t.Fatalf("unexpected extra events or warnings: %d %s", len(writer.lines), warnings.String())
			}
		})
	}
}

func TestEventsHistoryDeadlineAndErrors(t *testing.T) {
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	rec, err := ctx.Store.CreateContainer("gone", store.ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ctx.Store.RemoveContainer(rec.ID); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"events", "--since=0", "--until", time.Now().Format(time.RFC3339Nano), "-f", "container=/GONE", "--format={{.Action}}"},
		{"system", "events", "--since=0", "--until", time.Now().Format(time.RFC3339Nano), "-f", "container=" + rec.ID[:12], "--format={{.Action}}"},
	} {
		out.Reset()
		code, err := Dispatch(ctx, args)
		if err != nil || code != 0 || out.String() != "create\ndestroy\n" {
			t.Fatalf("removed container history %v: %d %q %v", args, code, out.String(), err)
		}
	}
	out.Reset()
	if code, err := eventsCommand(ctx, []string{"--until=0"}); err != nil || code != 0 || out.Len() != 0 {
		t.Fatalf("default replayed history: %d %s %v", code, out.String(), err)
	}
	deadline := time.Now().Add(40 * time.Millisecond)
	opts := eventOptions{until: &deadline}
	if err := streamEvents(stdcontext.Background(), ctx.Store, opts, out, io.Discard, time.Millisecond); err != nil || time.Now().Before(deadline) || out.Len() != 0 {
		t.Fatalf("deadline: %s %v", out.String(), err)
	}
	opts.format = "{{"
	if err := streamEvents(stdcontext.Background(), ctx.Store, opts, out, io.Discard, time.Millisecond); err == nil {
		t.Fatal("invalid template followed forever")
	}
	opts, _ = parseEventOptions([]string{"--since=0", "--until", time.Now().Format(time.RFC3339Nano)}, time.Now())
	if err := streamEvents(stdcontext.Background(), ctx.Store, opts, failedEventWriter{}, io.Discard, time.Millisecond); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("stream output error: %v", err)
	}
}

func TestEventsDeadlineDrainsBlockedReplay(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	if err := ctx.Store.AppendEvent(store.Event{Action: "first"}); err != nil {
		t.Fatal(err)
	}
	writer := &eventGateWriter{entered: make(chan struct{}), release: make(chan struct{}), lines: make(chan string, 3)}
	deadline := time.Now().Add(2 * time.Second)
	since := time.Unix(0, 0)
	opts := eventOptions{since: &since, until: &deadline, format: "{{.Action}}"}
	streamCtx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- streamEvents(streamCtx, ctx.Store, opts, writer, io.Discard, time.Millisecond) }()
	select {
	case <-writer.entered:
	case <-streamCtx.Done():
		t.Fatal("initial replay did not begin")
	}
	if err := ctx.Store.AppendEvent(store.Event{Action: "before-deadline"}); err != nil {
		close(writer.release)
		t.Fatal(err)
	}
	<-time.After(time.Until(deadline))
	close(writer.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(writer.lines) != 2 || <-writer.lines != "first\n" || <-writer.lines != "before-deadline\n" {
		t.Fatal("missed an event published before the deadline while replay was blocked")
	}
}

func TestEventsFollowerRejectsCorruptionAndReset(t *testing.T) {
	for _, data := range []string{"{", `{"version":1,"lastSequence":0,"events":[]}`} {
		t.Run(data, func(t *testing.T) {
			ctx, _ := fixtureCLI(t)
			if err := ctx.Store.AppendEvent(store.Event{Action: "first"}); err != nil {
				t.Fatal(err)
			}
			writer := &eventGateWriter{entered: make(chan struct{}), release: make(chan struct{}), lines: make(chan string, 3)}
			since := time.Unix(0, 0)
			opts := eventOptions{since: &since}
			streamCtx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- streamEvents(streamCtx, ctx.Store, opts, writer, io.Discard, time.Millisecond) }()
			select {
			case <-writer.entered:
			case <-streamCtx.Done():
				t.Fatal("initial replay did not begin")
			}
			if err := os.WriteFile(ctx.Store.Path("events.json"), []byte(data), 0o600); err != nil {
				close(writer.release)
				t.Fatal(err)
			}
			close(writer.release)
			if err := <-done; err == nil || errors.Is(err, stdcontext.DeadlineExceeded) {
				t.Fatalf("follower ignored corrupt/reset journal: %v", err)
			}
		})
	}
}
