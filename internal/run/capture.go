package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

const commitProtocol = 1

type captureRequest struct {
	Protocol int
	RunID    string
}

// CaptureLease belongs to the supervisor. Closing its connection also releases
// the freeze when the caller exits unexpectedly, without an unpause request.
type CaptureLease struct{ stream *execStream }

func BeginCapture(ctx *context.Context, rec *store.ContainerRecord) (*CaptureLease, error) {
	if rec.State.CommitProtocol != commitProtocol {
		return nil, errors.New("container's supervisor does not support commit capture; stop and start it with this binary, or use --pause=false")
	}
	deadline := time.Now().Add(5 * time.Second)
	f, err := win32.DialPipe(execPipeName(rec.ID, rec.State.RunID), deadline)
	if err != nil {
		return nil, err
	}
	c := &execStream{File: f}
	fail := func(err error) (*CaptureLease, error) { _ = c.Close(); return nil, err }
	if err := c.SetDeadline(deadline); err != nil {
		return fail(err)
	}
	data, _ := json.Marshal(captureRequest{commitProtocol, rec.State.RunID})
	if err := c.send(commitAcquire, data); err != nil {
		return fail(err)
	}
	kind, data, err := c.receive()
	if err != nil {
		return fail(err)
	}
	if kind == execError {
		return fail(errors.New(string(data)))
	}
	if kind != commitAcquired {
		return fail(errors.New("invalid capture acknowledgement"))
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	return &CaptureLease{c}, nil
}

func (l *CaptureLease) Close() error {
	if l == nil || l.stream == nil {
		return nil
	}
	c := l.stream
	l.stream = nil
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := c.send(commitRelease, nil); err != nil {
		return err
	}
	kind, data, err := c.receive()
	if err != nil {
		return err
	}
	if kind == execError {
		return errors.New(string(data))
	}
	if kind != commitReleased {
		return errors.New("invalid capture release acknowledgement")
	}
	return nil
}

func (r *execRuntime) serveCapture(c *execStream, data []byte) {
	var req captureRequest
	r.mu.Lock()
	err := error(nil)
	if json.Unmarshal(data, &req) != nil || req.Protocol != commitProtocol || req.RunID != r.runID {
		err = errors.New("capture protocol or container run changed")
	} else if r.commitContext == nil || r.commitRecord == nil || r.closing || win32.WaitForSingleObject(r.primary, 0) != win32.WAIT_TIMEOUT {
		err = errors.New("container is not running")
	} else if r.capturing {
		err = errors.New("container commit capture is already in progress")
	}
	wasPaused := r.paused
	if err == nil && !wasPaused {
		err = r.setPausedLocked(r.commitContext, r.commitRecord, true)
	}
	if err == nil {
		r.capturing = true
	}
	r.mu.Unlock()
	if err != nil {
		_ = c.send(execError, []byte(err.Error()))
		return
	}
	released := false
	release := func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if released {
			return nil
		}
		released = true
		r.capturing = false
		if !wasPaused && !r.closing {
			if err := r.setPausedLocked(r.commitContext, r.commitRecord, false); err != nil {
				// Never leave a silently frozen job after losing its capture owner.
				r.stopLocked()
				win32.TerminateJobObject(r.job, 1)
				return fmt.Errorf("restoring container after capture: %w", err)
			}
		}
		return nil
	}
	defer func() { _ = release() }()
	if err := c.send(commitAcquired, nil); err != nil {
		return
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		return
	}
	kind, _, err := c.receive()
	releaseErr := release()
	if err != nil || kind != commitRelease {
		return
	}
	if releaseErr != nil {
		_ = c.send(execError, []byte(releaseErr.Error()))
		return
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_ = c.send(commitReleased, nil)
}
