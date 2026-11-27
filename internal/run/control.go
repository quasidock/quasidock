package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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

func ParseKillSignal(raw string) (KillSignal, error) {
	switch strings.ToUpper(raw) {
	case "KILL", "SIGKILL", "9":
		return SignalKILL, nil
	case "TERM", "SIGTERM", "15":
		return SignalTERM, nil
	default:
		return "", fmt.Errorf("unsupported signal %q: use KILL or TERM", raw)
	}
}

func (s KillSignal) valid() bool { return s == SignalTERM || s == SignalKILL }

var errForegroundTERM = errors.New("TERM is unsupported for foreground containers; use stop or kill --signal KILL")
var errSignalDelivery = errors.New("signal delivery failed")

const controlProtocol = 4

type controlAction string

const (
	actionUpdate        controlAction = "update"
	actionCancelRestart controlAction = "cancel-restart"
	actionPause         controlAction = "pause"
	actionUnpause       controlAction = "unpause"
	actionNetwork       controlAction = "network"
	actionUnlink        controlAction = "unlink"
	// Resume is idempotent for shutdown, which can race with attached TERM.
	actionResume controlAction = "resume"
)

type controlRequest struct {
	Update    *UpdateOptions `json:",omitempty"`
	Network   *networkChange `json:",omitempty"`
	LinkAlias string         `json:",omitempty"`
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
	defer func() { _ = os.Remove(tmp) }()
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
	if rec.State.ControlProtocol >= 2 {
		if err := sendControlRequest(ctx, rec, controlRequest{Action: actionCancelRestart}); err != nil && ctx.Store.IsContainerRunning(rec.ID) {
			return err
		}
	}
	return sendControlRequest(ctx, rec, controlRequest{Signal: signal})
}

func sendControlRequest(ctx *context.Context, rec *store.ContainerRecord, request controlRequest) error {
	if !validRunID(controlKey(rec)) {
		return errors.New("container uses an older supervisor; finish its run with the original binary before upgrading")
	}
	if request.Action != "" && rec.State.ControlProtocol < 1 {
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
	event, err := win32.OpenEvent(controlEventName(rec.ID, controlKey(rec)), win32.EVENT_MODIFY_STATE)
	if err != nil {
		if !ctx.Store.IsContainerRunning(rec.ID) {
			return exited()
		}
		return fmt.Errorf("opening container control event: %w", err)
	}
	defer win32.CloseHandle(event)
	requestPath := controlPath(ctx.Store, rec.ID, controlKey(rec))
	resultPath := controlResultPath(ctx.Store, rec.ID, requestID)
	defer func() { _ = os.Remove(requestPath) }()
	defer func() { _ = os.Remove(resultPath) }()
	request.RunID, request.RequestID = controlKey(rec), requestID
	if err := writeControlJSON(ctx.Store, requestPath, request); err != nil {
		return err
	}
	if !win32.SetEvent(event) {
		return errors.New("signaling container control event failed")
	}
	timeout := 5 * time.Second
	if request.Action == actionNetwork {
		timeout += 3 * networkRequestTimeout
	}
	deadline := time.Now().Add(timeout)
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
				if request.Action == actionNetwork || request.Action == actionUnlink {
					return errors.New(result.Error)
				}
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

func serveControlRequest(ctx *context.Context, rec *store.ContainerRecord, handle func(controlRequest) error) {
	data, err := os.ReadFile(controlPath(ctx.Store, rec.ID, controlKey(rec)))
	if err != nil {
		return
	}
	var request controlRequest
	if json.Unmarshal(data, &request) != nil || request.RunID != controlKey(rec) || !validRunID(request.RequestID) {
		return
	}
	result := controlResult{RequestID: request.RequestID}
	if err := handle(request); err != nil {
		result.Error = err.Error()
	}
	if err := writeControlJSON(ctx.Store, controlResultPath(ctx.Store, rec.ID, request.RequestID), result); err != nil {
		ctx.Note("warning: writing container control response: %v", err)
		return
	}
	if ack, err := win32.OpenEvent(controlAckName(request.RequestID), win32.EVENT_MODIFY_STATE); err == nil {
		win32.SetEvent(ack)
		win32.CloseHandle(ack)
	}
}

func waitControlled(ctx *context.Context, rec *store.ContainerRecord, child *process.Child, detached bool, event win32.HANDLE, live *execRuntime, attachment *attachRuntime) (uint32, error) {
	defer child.Close()
	defer live.Close()
	if attachment != nil {
		defer attachment.stopAccepting()
	}
	handles := []win32.HANDLE{child.Process, event}
	if live.network != nil {
		handles = append(handles, live.network.process)
	}
	for {
		switch win32.WaitForMultipleObjects(handles, false, win32.INFINITE) {
		case win32.WAIT_OBJECT_0:
			if attachment != nil {
				attachment.stopAccepting()
			}
			live.Close()
			code, err := child.Wait()
			live.mu.Lock()
			failure := live.updateFailure
			live.mu.Unlock()
			return code, errors.Join(err, failure)
		case win32.WAIT_OBJECT_0 + 1:
			serveControlRequest(ctx, rec, func(request controlRequest) error {
				return live.control(ctx, rec, request, detached)
			})
		case win32.WAIT_OBJECT_0 + 2:
			process.EndJob(child.Job)
			return 1, errors.New("the Windows network helper exited during the run")
		default:
			if live.network != nil {
				process.EndJob(child.Job)
			}
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
	if request.Action == actionCancelRestart {
		if request.Signal != "" || request.Update != nil {
			return errors.New("invalid cancel-restart request")
		}
		_, err := suppressRestart(ctx, rec)
		return err
	}
	if r.closing || win32.WaitForSingleObject(r.primary, 0) != win32.WAIT_TIMEOUT {
		return errors.New("container is not running")
	}
	if r.capturing {
		return errors.New("container commit capture is in progress")
	}
	if request.Action != "" {
		if request.Signal != "" {
			return errors.New("control action cannot include a signal")
		}
		switch request.Action {
		case actionUpdate:
			if request.Update == nil || request.Update.Empty() {
				return errors.New("update requires at least one setting")
			}
			return r.updateLocked(ctx, rec, *request.Update)
		case actionNetwork:
			if request.Network == nil || request.Update != nil {
				return errors.New("invalid network control request")
			}
			return r.changeNetworkLocked(ctx, rec, *request.Network)
		case actionUnlink:
			if request.LinkAlias == "" || request.Update != nil || request.Network != nil {
				return errors.New("invalid unlink control request")
			}
			return r.unlinkLocked(ctx, rec, request.LinkAlias)
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
	if request.Signal == SignalTERM && !detached {
		return errForegroundTERM
	}
	if (rec.State.SupervisorID != "" || rec.State.ControlProtocol >= 2) && (request.Signal == SignalTERM || request.Signal == SignalKILL) {
		if _, err := suppressRestart(ctx, rec); err != nil {
			return err
		}
	}
	switch request.Signal {
	case SignalTERM:
		if r.paused {
			if err := r.setPausedLocked(ctx, rec, false); err != nil {
				return err
			}
		}
		if err := win32.SendConsoleBreak(win32.GetProcessId(r.primary)); err != nil {
			return err
		}
		ctx.Store.RecordLiveContainerEvent(rec, "kill", map[string]string{"signal": "15"})
		return nil
	case SignalKILL:
		r.stopLocked()
		if !win32.TerminateJobObject(r.job, 1) {
			return errors.New("TerminateJobObject failed")
		}
		ctx.Store.RecordLiveContainerEvent(rec, "kill", map[string]string{"signal": "9"})
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
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
