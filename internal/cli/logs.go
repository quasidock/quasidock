package cli

import (
	"bytes"
	stdcontext "context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/quasidock/quasidock/internal/containerlog"
	"github.com/quasidock/quasidock/internal/context"
)

const logsUsage = "usage: quasidock logs [-f|--follow] [-n|--tail LINES] [--since TIME] [--until TIME] [-t|--timestamps] [--details] CONTAINER"
const logTimestampLayout = "2006-01-02T15:04:05.000000000Z"

type logOptions struct {
	target                      string
	tail                        int
	follow, timestamps, details bool
	since, until                *time.Time
}

func parseLogOptions(args []string, now time.Time) (logOptions, error) {
	opts := logOptions{tail: -1}
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
		if strings.HasPrefix(name, "-n") && len(name) > 2 && !hasValue {
			name, value, hasValue = "-n", arg[2:], true
		}
		switch name {
		case "--since", "--until", "-n", "--tail":
			if !hasValue {
				if len(args) == 0 {
					return opts, fmt.Errorf("logs %s needs a value", name)
				}
				value, args = args[0], args[1:]
			}
			if name == "-n" || name == "--tail" {
				if value == "all" {
					opts.tail = -1
				} else {
					n, err := strconv.Atoi(value)
					if err != nil || n < 0 {
						return opts, fmt.Errorf("invalid --tail %q: expected non-negative integer or \"all\"", value)
					}
					opts.tail = n
				}
			} else {
				ts, err := parseCommandTime(value, now)
				if err != nil {
					return opts, err
				}
				if name == "--since" {
					opts.since = &ts
				} else {
					opts.until = &ts
				}
			}
		case "-f", "--follow", "-t", "--timestamps", "--details":
			parsed := true
			if hasValue {
				var err error
				parsed, err = strconv.ParseBool(value)
				if err != nil {
					return opts, fmt.Errorf("invalid logs boolean %q: expected true or false", value)
				}
			}
			switch name {
			case "-f", "--follow":
				opts.follow = parsed
			case "-t", "--timestamps":
				opts.timestamps = parsed
			case "--details":
				opts.details = parsed
			}
		default:
			return opts, fmt.Errorf("unknown logs option %q; %s", arg, logsUsage)
		}
	}
	if len(targets) != 1 || targets[0] == "" {
		return opts, errors.New(logsUsage)
	}
	opts.target = targets[0]
	if opts.since != nil && opts.until != nil && opts.since.After(*opts.until) {
		return opts, errors.New("logs --since must not be later than --until")
	}
	return opts, nil
}

func logsCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := fmt.Fprintln(ctx.Stdout, logsUsage+"\nTimes accept Unix seconds, RFC3339/local dates and Go durations (positive = past).\nSince is inclusive; until is exclusive. Details shows configured attributes (currently none).\nTime flags skip untimestamped history with a warning. -f follows until exit or the until deadline.")
		return 0, err
	}
	opts, err := parseLogOptions(args, time.Now())
	if err != nil {
		return 1, err
	}
	cancel, stop := signal.NotifyContext(stdcontext.Background(), os.Interrupt)
	defer stop()
	rec, err := ctx.Store.ResolveContainerContext(cancel, opts.target)
	if err != nil {
		if cancel.Err() != nil {
			return 130, nil
		}
		return 1, err
	}
	running := func() bool { return ctx.Store.IsContainerRunning(rec.ID) }
	if err := streamLogs(cancel, rec.LogPath, running, opts, ctx.Stdout, ctx.Stderr, 50*time.Millisecond); err != nil {
		if cancel.Err() != nil {
			return 130, nil
		}
		return 1, err
	}
	return 0, nil
}

// logStream retains line state even when the initial tail hides part of a line.
// A timestamp belongs to the first byte of the merged, LF-delimited raw entry.
type logStream struct {
	ctx                  stdcontext.Context
	opts                 logOptions
	out, warnings        io.Writer
	index                *containerlog.IndexReader
	offset, emitStart    int64
	initialized, warned  bool
	inLine, known, allow bool
	prefixed             bool
	lineTime             time.Time
	buffer               [32 * 1024]byte
}

func (s *logStream) timed() bool {
	return s.opts.timestamps || s.opts.since != nil || s.opts.until != nil
}

func (s *logStream) warnUntimed() {
	if !s.warned {
		_, _ = fmt.Fprintln(s.warnings, "warning: skipping container log history without recorded timestamps; restart older supervisors with the current binary to capture timestamps")
		s.warned = true
	}
}

func (s *logStream) consume(path string, final bool) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		s.initialized = true
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var size int64
	var batch *containerlog.Batch
	if s.timed() {
		batch, err = s.index.Read(f)
		if err != nil {
			return err
		}
		defer func() { _ = batch.Close() }()
		size = batch.Size
	} else {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		size = info.Size()
	}
	if !s.initialized {
		s.emitStart, err = logTailStart(f, size, s.opts.tail)
		if err != nil {
			return err
		}
		s.offset = s.emitStart
		// Recover the first-byte timestamp when --tail=0 starts mid-entry.
		if s.timed() && s.opts.tail == 0 {
			s.offset, err = logTailStart(f, size, 1)
			if err != nil {
				return err
			}
		}
		s.initialized = true
	}
	if size < s.offset {
		return errors.New("container log was truncated while following")
	}
	if !s.timed() {
		return s.copyRange(f, size, nil)
	}
	for {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		span, err := batch.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if span.Offset > s.offset {
			if err := s.copyRange(f, span.Offset, nil); err != nil {
				return err
			}
		}
		if end := span.Offset + span.Length; end > s.offset {
			if err := s.copyRange(f, end, &span.Time); err != nil {
				return err
			}
		}
	}
	// While live, an unindexed suffix may still be in the writer's raw/index
	// publication window. Leave its cursor untouched until metadata arrives.
	if final {
		return s.copyRange(f, size, nil)
	}
	if !batch.Present && size > max(s.offset, s.emitStart) {
		s.warnUntimed()
	}
	return nil
}

func (s *logStream) copyRange(f *os.File, end int64, ts *time.Time) error {
	for s.offset < end {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		n, err := f.ReadAt(s.buffer[:min(int64(len(s.buffer)), end-s.offset)], s.offset)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
		data := s.buffer[:n]
		if !s.timed() {
			if err := writeLogBytes(s.out, data); err != nil {
				return err
			}
			s.offset += int64(n)
			continue
		}
		for len(data) > 0 {
			if !s.inLine {
				s.inLine, s.known, s.prefixed = true, ts != nil, false
				if ts != nil {
					s.lineTime = *ts
				}
				s.allow = ts != nil && (s.opts.since == nil || !ts.Before(*s.opts.since)) && (s.opts.until == nil || ts.Before(*s.opts.until))
			}
			length := len(data)
			if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
				length = newline + 1
			}
			visible := data[:length]
			if skip := s.emitStart - s.offset; skip > 0 {
				visible = visible[min(int64(length), skip):]
			}
			if len(visible) > 0 {
				if !s.known {
					s.warnUntimed()
				} else if s.allow {
					if s.opts.timestamps && !s.prefixed {
						if err := writeLogBytes(s.out, []byte(s.lineTime.UTC().Format(logTimestampLayout)+" ")); err != nil {
							return err
						}
						s.prefixed = true
					}
					if err := writeLogBytes(s.out, visible); err != nil {
						return err
					}
				}
			}
			s.offset += int64(length)
			s.inLine = data[length-1] != '\n'
			data = data[length:]
		}
	}
	return nil
}

func writeLogBytes(out io.Writer, data []byte) error {
	n, err := out.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

// logTailStart finds a byte boundary without buffering the whole log or line.
func logTailStart(f *os.File, size int64, lines int) (int64, error) {
	if lines < 0 || size == 0 {
		return 0, nil
	}
	if lines == 0 {
		return size, nil
	}
	var buffer [32 * 1024]byte
	for end := size; end > 0; {
		start := max(int64(0), end-int64(len(buffer)))
		n, err := f.ReadAt(buffer[:end-start], start)
		if err != nil {
			return 0, err
		}
		for i := n - 1; i >= 0; i-- {
			if buffer[i] == '\n' && start+int64(i) != size-1 {
				lines--
				if lines == 0 {
					return start + int64(i) + 1, nil
				}
			}
		}
		end = start
	}
	return 0, nil
}

func streamLogs(ctx stdcontext.Context, path string, running func() bool, opts logOptions, out, warnings io.Writer, interval time.Duration) error {
	if opts.since != nil && opts.until != nil && opts.since.Equal(*opts.until) {
		return nil
	}
	if warnings == nil {
		warnings = io.Discard
	}
	s := logStream{ctx: ctx, opts: opts, out: out, warnings: warnings, index: containerlog.NewIndexReader(path)}
	finished := func() bool { return !opts.follow || !running() || opts.until != nil && !time.Now().Before(*opts.until) }
	final := finished()
	if err := s.consume(path, final); err != nil {
		return err
	}
	if final {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var until <-chan time.Time
	if opts.until != nil {
		timer := time.NewTimer(max(time.Duration(0), time.Until(*opts.until)))
		defer timer.Stop()
		until = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-until:
		}
		final = finished()
		if err := s.consume(path, final); err != nil {
			return err
		}
		if final {
			return nil
		}
	}
}
