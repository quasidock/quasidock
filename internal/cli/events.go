package cli

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
)

const eventsUsage = "usage: quasidock events [--since TIME] [--until TIME] [-f|--filter KEY=VALUE] [--format json|TEMPLATE]"

type eventOptions struct {
	since, until *time.Time
	format       string
	filters      map[string][]string
}

func parseEventTime(raw string, now time.Time) (time.Time, error) {
	parts := strings.Split(raw, ".")
	if len(parts) <= 2 {
		seconds, err := strconv.ParseInt(parts[0], 10, 64)
		if err == nil {
			var nanos int64
			valid := true
			if len(parts) == 2 {
				fraction := parts[1]
				valid = len(fraction) > 0 && len(fraction) <= 9
				for _, ch := range fraction {
					valid = valid && ch >= '0' && ch <= '9'
				}
				if valid {
					nanos, _ = strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
					if strings.HasPrefix(raw, "-") {
						nanos = -nanos
					}
				}
			}
			if valid {
				return time.Unix(seconds, nanos), nil
			}
		}
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04", "2006-01-02"} {
		if ts, err := time.ParseInLocation(layout, raw, now.Location()); err == nil {
			return ts, nil
		}
	}
	if duration, err := time.ParseDuration(raw); err == nil {
		// Avoid negating the minimum representable duration.
		if duration == time.Duration(-1<<63) {
			return now.Add(time.Duration(1<<63 - 1)).Add(time.Nanosecond), nil
		}
		return now.Add(-duration), nil
	}
	return time.Time{}, fmt.Errorf("invalid events time %q: use a Unix timestamp, RFC3339/local date-time, or Go duration", raw)
}

func parseEventOptions(args []string, now time.Time) (eventOptions, error) {
	opts := eventOptions{filters: make(map[string][]string)}
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		if arg == "--" && len(args) == 0 {
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--since", "--until", "--format", "--filter", "-f":
		default:
			return opts, fmt.Errorf("unknown events option %q; %s", arg, eventsUsage)
		}
		if !hasValue {
			if len(args) == 0 {
				return opts, fmt.Errorf("events %s needs a value", name)
			}
			value, args = args[0], args[1:]
		}
		switch name {
		case "--since", "--until":
			ts, err := parseEventTime(value, now)
			if err != nil {
				return opts, err
			}
			if name == "--since" {
				opts.since = &ts
			} else {
				opts.until = &ts
			}
		case "--format":
			opts.format = value
		case "--filter", "-f":
			key, val, ok := strings.Cut(value, "=")
			if !ok || val == "" {
				return opts, fmt.Errorf("invalid events filter %q: expected KEY=VALUE with a nonempty value", value)
			}
			switch key {
			case "event", "container", "image":
			case "type":
				if val != "container" {
					return opts, errors.New("events supports only type=container")
				}
			default:
				return opts, fmt.Errorf("unsupported events filter %q: use event, container, image, or type", key)
			}
			opts.filters[key] = append(opts.filters[key], val)
		}
	}
	if opts.since != nil && opts.until != nil && opts.since.After(*opts.until) {
		return opts, errors.New("events --since must not be later than --until")
	}
	return opts, nil
}

func (opts eventOptions) matches(event store.Event) bool {
	ts := time.Unix(0, event.TimeNano)
	if opts.since != nil && ts.Before(*opts.since) || opts.until != nil && !ts.Before(*opts.until) {
		return false
	}
	for key, values := range opts.filters {
		matched := false
		for _, value := range values {
			switch key {
			case "event":
				matched = matched || value == event.Action
			case "type":
				matched = matched || value == event.Type
			case "image":
				matched = matched || value == event.Actor.Attributes["image"] || value == event.Actor.Attributes["imageID"]
			case "container":
				lower := strings.ToLower(value)
				hex := len(lower) >= 2 && len(lower) <= 64
				for _, ch := range lower {
					hex = hex && (ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f')
				}
				matched = matched || strings.EqualFold(strings.TrimPrefix(value, "/"), event.Actor.Attributes["name"]) ||
					hex && strings.HasPrefix(strings.ToLower(event.Actor.ID), lower)
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func eventRenderer(out io.Writer, format string) (func(store.Event) error, error) {
	if format == "json" {
		encoder := json.NewEncoder(out)
		return func(event store.Event) error { return encoder.Encode(event) }, nil
	}
	var tmpl *template.Template
	if format != "" {
		var err error
		tmpl, err = template.New("events").Funcs(template.FuncMap{
			"json": func(value any) (string, error) {
				data, err := json.Marshal(value)
				return string(data), err
			},
		}).Parse(format)
		if err != nil {
			return nil, err
		}
	}
	return func(event store.Event) error {
		var buf bytes.Buffer
		if tmpl != nil {
			if err := tmpl.Execute(&buf, event); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(&buf, "%s %s %s %s", time.Unix(0, event.TimeNano).UTC().Format(time.RFC3339Nano), event.Type, event.Action, event.Actor.ID)
			keys := make([]string, 0, len(event.Actor.Attributes))
			for key := range event.Actor.Attributes {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if len(keys) > 0 {
				buf.WriteString(" (")
				for i, key := range keys {
					if i > 0 {
						buf.WriteString(", ")
					}
					fmt.Fprintf(&buf, "%s=%s", key, event.Actor.Attributes[key])
				}
				buf.WriteByte(')')
			}
		}
		buf.WriteByte('\n')
		_, err := out.Write(buf.Bytes())
		return err
	}, nil
}

func streamEvents(ctx stdcontext.Context, st *store.Store, opts eventOptions, out, warnings io.Writer, interval time.Duration) error {
	render, err := eventRenderer(out, opts.format)
	if err != nil {
		return err
	}
	snapshot, err := st.ReadEventSnapshot(ctx)
	if err != nil {
		return err
	}
	var cursor uint64
	if opts.since == nil {
		cursor = snapshot.LastSequence
	}
	consume := func(snapshot store.EventSnapshot, initial bool) error {
		if snapshot.LastSequence < cursor {
			return errors.New("event journal was reset while following")
		}
		if !initial && len(snapshot.Events) > 0 && snapshot.Events[0].Sequence-1 > cursor {
			_, _ = fmt.Fprintln(warnings, "warning: events follower fell behind retained history; continuing with the oldest available event")
		}
		for _, entry := range snapshot.Events {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Sequence > cursor && opts.matches(entry.Event) {
				if err := render(entry.Event); err != nil {
					return err
				}
			}
		}
		cursor = snapshot.LastSequence
		return nil
	}
	if err := consume(snapshot, true); err != nil {
		return err
	}
	if opts.until != nil && !time.Now().Before(*opts.until) {
		// Output may have blocked while more in-range events were published.
		// Take a final snapshot even when the initial replay passed the deadline.
		snapshot, err := st.ReadEventSnapshot(ctx)
		if err != nil {
			return err
		}
		return consume(snapshot, false)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var until <-chan time.Time
	if opts.until != nil {
		timer := time.NewTimer(time.Until(*opts.until))
		defer timer.Stop()
		until = timer.C
	}
	for {
		finished := false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-until:
			finished = true
		}
		snapshot, err := st.ReadEventSnapshot(ctx)
		if err != nil {
			return err
		}
		if err := consume(snapshot, false); err != nil {
			return err
		}
		if finished {
			return nil
		}
	}
}

func eventsCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := fmt.Fprintln(ctx.Stdout, eventsUsage+"\nFilters: event, container, image, type=container. Retains the latest 256 events.\nWithout --since, follows new events. Positive durations refer to the past.")
		return 0, err
	}
	opts, err := parseEventOptions(args, time.Now())
	if err != nil {
		return 1, err
	}
	cancel, stop := signal.NotifyContext(stdcontext.Background(), os.Interrupt)
	defer stop()
	if err := streamEvents(cancel, ctx.Store, opts, ctx.Stdout, ctx.Stderr, 100*time.Millisecond); err != nil {
		if cancel.Err() != nil {
			return 130, nil
		}
		return 1, err
	}
	return 0, nil
}
