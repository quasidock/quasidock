package context

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/quasidock/quasidock/internal/store"
)

// ErrFailed indicates an error that has already been reported to the user.
var ErrFailed = errors.New("command failed")

// FormatDuration formats a millisecond duration according to quasidock rules.
func FormatDuration(ms uint64) string {
	s := ms / 1000
	if ms < 1000 {
		return fmt.Sprintf("%d ms", ms)
	} else if s < 60 {
		return fmt.Sprintf("%d.%d s", s, (ms%1000)/100)
	} else if s < 3600 {
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %02dm", s/3600, (s%3600)/60)
}

// PrintStderr writes formatted text to stderr, ignoring write failures (e.g. broken pipe).
func PrintStderr(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format, args...)
}

// Fail prints an error message to stderr and returns ErrFailed.
func Fail(format string, args ...any) error {
	PrintStderr("error: "+format+"\n", args...)
	return ErrFailed
}

// Note prints a progress or diagnostic line to stderr.
func Note(format string, args ...any) {
	PrintStderr(format+"\n", args...)
}

// Context holds process-wide context passed down through commands.
type Context struct {
	Store   *store.Store
	Env     map[string]string
	Verbose bool
	Stdout  io.Writer
	Stderr  io.Writer
}

// New creates a new Context populated from os.Environ().
func New() *Context {
	envMap := make(map[string]string)
	for _, env := range os.Environ() {
		for i := 0; i < len(env); i++ {
			if env[i] == '=' {
				k := env[:i]
				v := env[i+1:]
				envMap[k] = v
				break
			}
		}
	}
	return &Context{
		Env:    envMap,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// GetEnv retrieves an environment variable from the context's environment map.
func (c *Context) GetEnv(key string) (string, bool) {
	v, ok := c.Env[key]
	return v, ok
}

// SetEnv sets an environment variable in the context's environment map.
func (c *Context) SetEnv(key, value string) {
	c.Env[key] = value
}

// Fail logs an error and returns ErrFailed.
func (c *Context) Fail(format string, args ...any) error {
	return Fail(format, args...)
}

// Note logs a line to stderr.
func (c *Context) Note(format string, args ...any) {
	Note(format, args...)
}

func (c *Context) Now() time.Time {
	return time.Now()
}

func (c *Context) Since(start time.Time) time.Duration {
	return time.Since(start)
}

func (c *Context) Timed(start time.Time, format string, args ...any) {
	ReportTime(c.Verbose, start, format, args...)
}

// ReportTime reports elapsed time if verbose is true.
func ReportTime(verbose bool, start time.Time, format string, args ...any) {
	if !verbose {
		return
	}
	ms := time.Since(start).Milliseconds()
	msg := fmt.Sprintf(format, args...)
	PrintStderr("%7d ms  %s\n", ms, msg)
}
