package cli

import (
	"bytes"
	stdcontext "context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/quasidock/quasidock/internal/containercopy"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/win32"
)

const cpUsage = `usage: quasidock cp [--] CONTAINER:SOURCE HOST_DESTINATION|-
       quasidock cp [--] HOST_SOURCE|- CONTAINER:DESTINATION

Copy regular files and directories. Container / means ${data}, including declared
mounts. Use SOURCE/. to copy directory contents. - reads/writes a plain tar
archive; tar input requires an existing destination directory. Running containers
stay running. Links, ownership flags, and progress reporting are unsupported.`

func cpCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		_, err := fmt.Fprintln(ctx.Stdout, cpUsage)
		return 0, err
	}
	escaped := len(args) > 0 && args[0] == "--"
	if escaped {
		args = args[1:]
	}
	if len(args) != 2 {
		return 1, errors.New(cpUsage)
	}
	if !escaped {
		for _, arg := range args {
			if arg != "-" && strings.HasPrefix(arg, "-") {
				return 1, fmt.Errorf("unknown cp option %q; %s", arg, cpUsage)
			}
		}
	}
	signals, stop := signal.NotifyContext(stdcontext.Background(), os.Interrupt)
	defer stop()
	cancel, interrupt := stdcontext.WithCancel(signals)
	defer interrupt()
	streams := &copyStreams{ctx: cancel, interrupt: interrupt, in: os.Stdin, out: ctx.Stdout}
	var mode win32.DWORD
	input := win32.HANDLE(os.Stdin.Fd())
	if args[0] == "-" && win32.GetConsoleMode(input, &mode) {
		// Process Ctrl+C as input. With processed input, Windows can end the
		// read before delivering the console event, racing signal.Stop on exit.
		if !win32.SetConsoleMode(input, mode&^(win32.ENABLE_LINE_INPUT|win32.ENABLE_ECHO_INPUT|win32.ENABLE_PROCESSED_INPUT)) {
			return 1, errors.New("setting copy console mode failed")
		}
		defer win32.SetConsoleMode(input, mode)
		streams.console = true
	}
	return runTransfer(cancel, streams, func() error {
		return containercopy.Copy(cancel, ctx.Store, args[0], args[1], streams, streams)
	})
}

// runTransfer joins the I/O thread before returning, so cancellation cannot
// target a reused thread or outlive a transfer's cleanup.
func runTransfer(cancel stdcontext.Context, streams *copyStreams, transfer func() error) (uint32, error) {
	// Synchronous Windows console/pipe I/O cannot always be interrupted by
	// closing an os.File. Run the transfer on a dedicated cancellation thread,
	// following the attachment reader's cancellation pattern.
	ready := make(chan win32.HANDLE)
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Retire this thread on exit; it must not be reused while cancellation
		// can still target it. The caller closes its thread handle after joining.
		thread, err := win32.OpenCurrentThreadForCancellation()
		streams.thread = thread
		ready <- thread
		if err != nil {
			result <- err
			return
		}
		result <- transfer()
	}()
	thread := <-ready
	if thread != 0 {
		defer win32.CloseHandle(thread)
	}
	var err error
	select {
	case err = <-result:
	case <-cancel.Done():
		for {
			streams.cancelIO()
			select {
			case <-result:
				return 130, nil
			case <-time.After(time.Millisecond):
			}
		}
	}
	if cancel.Err() != nil {
		return 130, nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// Only stream I/O may be aborted. Cancelling every syscall on the transfer's
// thread can also abort the staging cleanup or lock release after a read ends.
type copyStreams struct {
	ctx       stdcontext.Context
	interrupt stdcontext.CancelFunc
	in        io.Reader
	out       io.Writer
	console   bool
	thread    win32.HANDLE
	mu        sync.Mutex
	active    bool
}

func (s *copyStreams) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.active = true
	return nil
}

func (s *copyStreams) end() {
	s.mu.Lock()
	s.active = false
	s.mu.Unlock()
}

func (s *copyStreams) cancelIO() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		win32.CancelSynchronousIo(s.thread)
	}
}

func (s *copyStreams) Read(p []byte) (int, error) {
	if err := s.begin(); err != nil {
		return 0, err
	}
	n, err := s.in.Read(p)
	s.end()
	if s.console && bytes.IndexByte(p[:n], 3) >= 0 {
		s.interrupt()
		return 0, s.ctx.Err()
	}
	return n, err
}

func (s *copyStreams) Write(p []byte) (int, error) {
	if err := s.begin(); err != nil {
		return 0, err
	}
	n, err := s.out.Write(p)
	s.end()
	return n, err
}
