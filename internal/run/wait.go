package run

import (
	stdcontext "context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

const waitProtocol = 1
const waitEnrollmentTimeout = 5 * time.Second

// WaitContainer returns the selected attempt's exit code without following a
// replacement run. Cancellation disconnects the observer without sending signals.
func WaitContainer(cancel stdcontext.Context, ctx *context.Context, target string) (uint32, error) {
	rec, err := ctx.Store.ResolveContainerContext(cancel, target)
	if err != nil {
		return 0, err
	}
	var op *store.ContainerOperation
	for op == nil {
		if err := cancel.Err(); err != nil {
			return 0, err
		}
		op, err = ctx.Store.TryLockContainer(rec.ID)
		if err != nil {
			return 0, err
		}
		if op == nil {
			select {
			case <-cancel.Done():
				return 0, cancel.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	defer op.Release()
	rec, err = ctx.Store.ReadContainerContext(cancel, rec.ID)
	if err != nil {
		return 0, err
	}
	if !rec.State.Running {
		return uint32(rec.State.ExitCode), nil
	}
	if rec.State.WaitProtocol != waitProtocol || rec.State.AttachProtocol != attachProtocol {
		return 0, errors.New("container's supervisor does not support wait; stop and start it with this binary")
	}
	c, code, err := subscribeWait(cancel, ctx, rec)
	if err != nil || c == nil {
		return code, err
	}
	defer func() { _ = c.Close() }()
	closeOnCancel := stdcontext.AfterFunc(cancel, func() { _ = c.Close() })
	defer closeOnCancel()
	// The live session now retains this attempt's result through restart/removal.
	op.Release()
	if ctx.Verbose {
		_, _ = fmt.Fprintf(ctx.Stderr, "waiting for container %s (%s)\n", rec.BareName(), rec.ShortID())
	}
	code, err = receiveWait(c)
	if cancel.Err() != nil {
		return 0, cancel.Err()
	}
	if err == nil {
		return code, nil
	}
	// A broken stream can race with the final state write. Only a result for the
	// same completed attempt is safe; never substitute a replacement run's code.
	current, readErr := ctx.Store.ReadContainerContext(cancel, rec.ID)
	if readErr == nil && current.State.RunID == rec.State.RunID && !current.State.Running {
		return uint32(current.State.ExitCode), nil
	}
	return 0, fmt.Errorf("waiting for container exit: %w", err)
}

// subscribeWait runs under the operation lock, protecting the selected attempt
// from replacement and automatic removal while the handshake completes.
func subscribeWait(cancel stdcontext.Context, ctx *context.Context, rec *store.ContainerRecord) (*framedStream, uint32, error) {
	enroll, stop := stdcontext.WithTimeout(cancel, waitEnrollmentTimeout)
	defer stop()
	deadline, _ := enroll.Deadline()
	for {
		current, err := ctx.Store.ReadContainerContext(enroll, rec.ID)
		if err != nil {
			return nil, 0, err
		}
		if current.State.RunID != rec.State.RunID {
			return nil, 0, errors.New("container run changed while subscribing to wait")
		}
		if !current.State.Running {
			return nil, uint32(current.State.ExitCode), nil
		}
		attempt := minDeadline(deadline, time.Now().Add(100*time.Millisecond))
		file, err := win32.DialPipe(attachPipeName(rec.ID, rec.State.RunID), attempt)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if err != nil {
			return nil, 0, fmt.Errorf("connecting to container wait: %w", err)
		}
		c := &framedStream{File: file}
		retry, err := waitHandshake(enroll, c, rec.State.RunID, deadline)
		if err == nil {
			return c, 0, nil
		}
		_ = c.Close()
		if !retry {
			return nil, 0, err
		}
		// The listener stops accepting before cleanup saves the exit status.
		// Keep the operation lock until that write becomes visible.
		select {
		case <-enroll.Done():
			return nil, 0, enroll.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func minDeadline(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func waitHandshake(cancel stdcontext.Context, c *framedStream, runID string, deadline time.Time) (retry bool, err error) {
	closeOnCancel := stdcontext.AfterFunc(cancel, func() { _ = c.Close() })
	defer closeOnCancel()
	if err := c.SetDeadline(deadline); err != nil {
		return false, err
	}
	data, _ := json.Marshal(attachRequest{Protocol: attachProtocol, RunID: runID, WaitOnly: true})
	if err := c.send(attachHello, data); err != nil {
		return true, err
	}
	kind, data, err := c.receive()
	if err != nil {
		return true, err
	}
	if kind == attachError {
		return string(data) == "container is stopping", errors.New(string(data))
	}
	var response attachResponse
	if kind != attachReady || json.Unmarshal(data, &response) != nil || response.Stdin {
		return false, errors.New("invalid wait acknowledgement")
	}
	return false, c.SetDeadline(time.Time{})
}

func receiveWait(c *framedStream) (uint32, error) {
	kind, data, err := c.receive()
	if err != nil {
		return 0, err
	}
	if kind == attachError {
		return 0, errors.New(string(data))
	}
	if kind != attachExit || len(data) != 4 {
		return 0, errors.New("invalid wait exit status")
	}
	return binary.LittleEndian.Uint32(data), nil
}
