package cli

import (
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
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
	cancel, stop := signal.NotifyContext(stdcontext.Background(), os.Interrupt)
	defer stop()
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
		ready <- thread
		if err != nil {
			result <- err
			return
		}
		result <- containercopy.Copy(cancel, ctx.Store, args[0], args[1], os.Stdin, ctx.Stdout)
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
			win32.CancelSynchronousIo(thread)
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
