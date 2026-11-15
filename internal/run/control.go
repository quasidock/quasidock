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

type controlRequest struct {
	RunID     string
	RequestID string
	Signal    KillSignal
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
	if !validRunID(rec.State.RunID) {
		return errors.New("container uses an older supervisor; finish its run with the original binary before upgrading")
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
			return nil
		}
		return fmt.Errorf("opening container control event: %w", err)
	}
	defer win32.CloseHandle(event)
	requestPath := controlPath(ctx.Store, rec.ID, rec.State.RunID)
	resultPath := controlResultPath(ctx.Store, rec.ID, requestID)
	defer os.Remove(requestPath)
	defer os.Remove(resultPath)
	if err := writeControlJSON(ctx.Store, requestPath, controlRequest{rec.State.RunID, requestID, signal}); err != nil {
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
			return nil // Natural exit raced with signal delivery.
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("container %s did not acknowledge %s", rec.ShortID(), signal)
		}
	}
}

func waitControlled(ctx *context.Context, rec *store.ContainerRecord, child *process.Child, detached bool, event win32.HANDLE) (uint32, error) {
	defer child.Close()
	for {
		switch win32.WaitForMultipleObjects([]win32.HANDLE{child.Process, event}, false, win32.INFINITE) {
		case win32.WAIT_OBJECT_0:
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
			var signalErr error
			switch request.Signal {
			case SignalTERM:
				if !detached {
					signalErr = errForegroundTERM
				} else {
					signalErr = win32.SendConsoleBreak(win32.GetProcessId(child.Process))
				}
			case SignalKILL:
				if !win32.TerminateJobObject(child.Job, 1) {
					signalErr = errors.New("TerminateJobObject failed")
				}
			default:
				signalErr = fmt.Errorf("unsupported signal %q", request.Signal)
			}
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
