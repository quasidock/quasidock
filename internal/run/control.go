package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

type KillSignal string

const (
	SignalKILL KillSignal = "KILL"
	SignalTERM KillSignal = "TERM"
)

var errForegroundTERM = errors.New("TERM is unsupported for foreground containers; use stop or kill --signal KILL")
var errSignalDelivery = errors.New("signal delivery failed")

const controlProtocol = 1

type controlAction string

const (
	actionPause   controlAction = "pause"
	actionUnpause controlAction = "unpause"
	// Resume is idempotent for shutdown, which can race with attached TERM.
	actionResume controlAction = "resume"
)

type controlRequest struct {
	RunID     string
	RequestID string
	Signal    KillSignal
	Action    controlAction `json:",omitempty"`
}

type controlResult struct {
	RequestID string
	Error     string
}

func controlEventName(id, runID string) string {
	return `Local\quasidock-ctr-control-` + id + "-" + runID
}

func controlAckName(requestID string) string {
	return `Local\quasidock-ctr-ack-` + requestID
}

func writeControlJSON(st *store.Store, path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp := st.TmpPath("container-control")
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return store.RenameRetrying(tmp, path, 5000)
}

func controlPath(st *store.Store, id, runID string) string {
	return st.Path("containers", id, "control-"+runID+".json")
}

func controlResultPath(st *store.Store, id, requestID string) string {
	return st.Path("containers", id, "control-result-"+requestID+".json")
}

func sendControl(ctx *context.Context, rec *store.ContainerRecord, signal KillSignal) error {
	return sendControlRequest(ctx, rec, controlRequest{Signal: signal})
}

func sendControlRequest(ctx *context.Context, rec *store.ContainerRecord, request controlRequest) error {
	if !validRunID(rec.State.RunID) {
		return errors.New("container uses an older supervisor; finish its run with the original binary before upgrading")
	}
	if request.Action != "" && rec.State.ControlProtocol != controlProtocol {
		return errors.New("container's supervisor does not support pause; stop and start it with this binary")
	}
	exited := func() error {
		if request.Action != "" {
			return fmt.Errorf("container %s exited before acknowledging %s", rec.ShortID(), request.Action)
		}
		return nil // Natural exit raced with signal delivery.
	}
	requestID, err := newRunID()
	if err != nil {
		return err
	}
	ack, err := win32.CreateEvent(controlAckName(requestID), true, false)
	if err != nil {
		return err
	}
	defer win32.CloseHandle(ack)
	event, err := win32.OpenEvent(controlEventName(rec.ID, rec.State.RunID), win32.EVENT_MODIFY_STATE)
	if err != nil {
		if !ctx.Store.IsContainerRunning(rec.ID) {
			return exited()
		}
		return fmt.Errorf("opening container control event: %w", err)
	}
	defer win32.CloseHandle(event)
	requestPath := controlPath(ctx.Store, rec.ID, rec.State.RunID)
	resultPath := controlResultPath(ctx.Store, rec.ID, requestID)
	defer os.Remove(requestPath)
	defer os.Remove(resultPath)
	request.RunID, request.RequestID = rec.State.RunID, requestID
	if err := writeControlJSON(ctx.Store, requestPath, request); err != nil {
		return err
	}
	if !win32.SetEvent(event) {
		return errors.New("signaling container control event failed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		switch win32.WaitForSingleObject(ack, 20) {
		case win32.WAIT_OBJECT_0:
			data, err := os.ReadFile(resultPath)
			if err != nil {
				return err
			}
			var result controlResult
			if err := json.Unmarshal(data, &result); err != nil {
				return err
			}
			if result.RequestID != requestID {
				return errors.New("container control response did not match request")
			}
			if result.Error == errForegroundTERM.Error() {
				return errForegroundTERM
			}
			if result.Error != "" {
				return fmt.Errorf("%w: %s", errSignalDelivery, result.Error)
			}
			return nil
		case win32.WAIT_TIMEOUT:
		default:
			return errors.New("waiting for container control acknowledgement failed")
		}
		if !ctx.Store.IsContainerRunning(rec.ID) {
			return exited()
		}
		if !time.Now().Before(deadline) {
			operation := string(request.Signal)
			if request.Action != "" {
				operation = string(request.Action)
			}
			return fmt.Errorf("container %s did not acknowledge %s", rec.ShortID(), operation)
		}
	}
}

func waitControlled(ctx *context.Context, rec *store.ContainerRecord, child *process.Child, detached bool, event win32.HANDLE, live *execRuntime, attachment *attachRuntime) (uint32, error) {
	defer child.Close()
	defer live.Close()
	if attachment != nil {
		defer attachment.stopAccepting()
	}
	for {
		switch win32.WaitForMultipleObjects([]win32.HANDLE{child.Process, event}, false, win32.INFINITE) {
		case win32.WAIT_OBJECT_0:
			if attachment != nil {
				attachment.stopAccepting()
			}
			live.Close()
			return child.Wait()
		case win32.WAIT_OBJECT_0 + 1:
			data, err := os.ReadFile(controlPath(ctx.Store, rec.ID, rec.State.RunID))
			if err != nil {
				continue
			}
			var request controlRequest
			if json.Unmarshal(data, &request) != nil || request.RunID != rec.State.RunID || !validRunID(request.RequestID) {
				continue
			}
			signalErr := live.control(ctx, rec, request, detached)
			result := controlResult{RequestID: request.RequestID}
			if signalErr != nil {
				result.Error = signalErr.Error()
			}
			if err := writeControlJSON(ctx.Store, controlResultPath(ctx.Store, rec.ID, request.RequestID), result); err != nil {
				ctx.Note("warning: writing container control response: %v", err)
				continue
			}
			if ack, err := win32.OpenEvent(controlAckName(request.RequestID), win32.EVENT_MODIFY_STATE); err == nil {
				win32.SetEvent(ack)
				win32.CloseHandle(ack)
			}
		default:
			return 1, errors.New("waiting for container process or control event failed")
		}
	}
}

// control serializes job state and signal delivery with exec admission. It never
// takes the operation lock: its caller holds that lock while awaiting the ack.
func (r *execRuntime) control(ctx *context.Context, rec *store.ContainerRecord, request controlRequest, detached bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runID != rec.State.RunID {
		return errors.New("container run changed")
	}
	if r.closing || win32.WaitForSingleObject(r.primary, 0) != win32.WAIT_TIMEOUT {
		return errors.New("container is not running")
	}
	if request.Action != "" {
		if request.Signal != "" {
			return errors.New("control action cannot include a signal")
		}
		switch request.Action {
		case actionPause:
			if r.paused {
				return errors.New("container is already paused")
			}
			return r.setPausedLocked(ctx, rec, true)
		case actionUnpause:
			if !r.paused {
				return errors.New("container is not paused")
			}
			return r.setPausedLocked(ctx, rec, false)
		case actionResume:
			if r.paused {
				return r.setPausedLocked(ctx, rec, false)
			}
			return nil
		default:
			return fmt.Errorf("unsupported control action %q", request.Action)
		}
	}
	switch request.Signal {
	case SignalTERM:
		if !detached {
			return errForegroundTERM
		}
		if r.paused {
			if err := r.setPausedLocked(ctx, rec, false); err != nil {
				return err
			}
		}
		return win32.SendConsoleBreak(win32.GetProcessId(r.primary))
	case SignalKILL:
		r.stopLocked()
		if !win32.TerminateJobObject(r.job, 1) {
			return errors.New("TerminateJobObject failed")
		}
		return nil
	default:
		return fmt.Errorf("unsupported signal %q", request.Signal)
	}
}

func (r *execRuntime) setPausedLocked(ctx *context.Context, rec *store.ContainerRecord, paused bool) error {
	if err := win32.SetJobFrozen(r.job, paused); err != nil {
		return fmt.Errorf("changing container pause state: %w", err)
	}
	_, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
		if c.State.RunID != r.runID || !c.State.Running {
			return errors.New("container run changed while updating pause state")
		}
		c.State.Paused = paused
		c.State.Status = "running"
		if paused {
			c.State.Status = "paused"
		}
		return nil
	})
	if err != nil {
		if rollbackErr := win32.SetJobFrozen(r.job, r.paused); rollbackErr != nil {
			r.stopLocked()
			var killErr error
			if !win32.TerminateJobObject(r.job, 1) {
				killErr = errors.New("TerminateJobObject failed after pause rollback failed")
			}
			return errors.Join(err, fmt.Errorf("rolling back pause state: %w", rollbackErr), killErr)
		}
		return fmt.Errorf("saving container pause state: %w", err)
	}
	r.paused = paused
	return nil
}

func (r *execRuntime) attachAllowed() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing || win32.WaitForSingleObject(r.primary, 0) != win32.WAIT_TIMEOUT {
		return errors.New("container is stopping")
	}
	if r.paused {
		return errors.New("container is paused; unpause it first")
	}
	return nil
}

func validRunID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
