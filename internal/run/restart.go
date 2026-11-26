package run

import (
	"errors"
	"fmt"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func controlKey(rec *store.ContainerRecord) string {
	if rec.State.SupervisorID != "" {
		return rec.State.SupervisorID
	}
	return rec.State.RunID
}

func suppressRestart(ctx *context.Context, rec *store.ContainerRecord) (*store.ContainerRecord, error) {
	return ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
		if controlKey(c) != controlKey(rec) {
			return errors.New("container supervisor changed")
		}
		c.State.RestartSuppressed = true
		return nil
	})
}

func shouldRestart(rec *store.ContainerRecord) bool {
	if rec.State.RestartSuppressed || !rec.State.Launched {
		return false
	}
	p := rec.HostConfig.RestartPolicy
	switch p.Name {
	case "always", "unless-stopped":
		return true
	case "on-failure":
		return rec.State.ExitCode != 0 && (p.MaximumRetryCount == 0 || rec.RestartCount < p.MaximumRetryCount)
	default:
		return false
	}
}

func restartDelay(previous, duration time.Duration) time.Duration {
	if previous == 0 || duration >= 10*time.Second {
		return 100 * time.Millisecond
	}
	return min(previous*2, time.Minute)
}

// Supervise owns a container session, including the intervals between payloads.
// Runtime pipes use RunID; its control event uses the stable SupervisorID.
func Supervise(ctx *context.Context, containerID, runID string, attached ...bool) (code uint32, runErr error) {
	if !oci.IsSha256Hex(containerID) || !validRunID(runID) {
		return 1, errors.New("invalid container supervisor arguments")
	}
	ready, _ := win32.OpenEvent(ReadyEventName(containerID, runID), win32.EVENT_MODIFY_STATE)
	if ready != 0 {
		defer win32.CloseHandle(ready)
		defer win32.SetEvent(ready)
	}
	runLock, err := store.OpenLock(ctx.Store.ContainerRunLockPath(containerID), true, false)
	if err != nil {
		return 1, err
	}
	defer runLock.Release()
	rec, err := ctx.Store.ReadContainer(containerID)
	if err != nil {
		return 1, err
	}
	if rec.State.RunID != runID {
		return 1, errors.New("container launch attempt was superseded")
	}
	if rec.State.SupervisorID == "" {
		rec, err = ctx.Store.UpdateContainer(containerID, func(c *store.ContainerRecord) error { c.State.SupervisorID = runID; return nil })
		if err != nil {
			return 1, err
		}
	}
	event, err := win32.CreateEvent(controlEventName(containerID, controlKey(rec)), false, false)
	if err != nil {
		recordLaunchFailure(ctx.Store, containerID, runID, err)
		return 1, err
	}
	defer win32.CloseHandle(event)
	defer func() {
		// Release ownership before auto-removal takes the operation lock.
		runLock.Release()
		if rec != nil && rec.HostConfig.AutoRemove && rec.State.Launched {
			autoRemoveContainer(ctx, rec)
		}
	}()
	first := true
	delay := time.Duration(0)
	var operation *store.ContainerOperation
	for {
		code, runErr = superviseAttempt(ctx, rec, runLock, event, ready, first && len(attached) > 0 && attached[0], operation)
		if operation != nil {
			operation.Release()
			operation = nil
		}
		if first && ready != 0 {
			win32.SetEvent(ready)
		}
		first = false
		ready = 0
		rec, err = ctx.Store.ReadContainer(containerID)
		if err != nil {
			return code, errors.Join(runErr, err)
		}
		if runErr != nil || !shouldRestart(rec) {
			return code, runErr
		}
		started, _ := time.Parse(time.RFC3339Nano, rec.State.StartedAt)
		delay = restartDelay(delay, time.Since(started))
		rec, err = ctx.Store.UpdateContainer(containerID, func(c *store.ContainerRecord) error {
			c.State.Status, c.State.Restarting = "restarting", true
			return nil
		})
		if err != nil {
			return code, err
		}
		operation, rec, err = awaitRestart(ctx, rec, event, delay)
		if err != nil {
			return code, err
		}
		if operation == nil {
			return code, nil
		}
		id, err := newRunID()
		if err != nil {
			operation.Release()
			return code, err
		}
		supervisorID := rec.State.SupervisorID
		rec, err = ctx.Store.UpdateContainer(containerID, func(c *store.ContainerRecord) error {
			c.RestartCount++
			c.State = store.ContainerState{Status: "created", RunID: id, SupervisorID: supervisorID, ControlProtocol: controlProtocol}
			return nil
		})
		if err != nil {
			operation.Release()
			return code, err
		}
	}
}

// awaitRestart does not block on the operation lock: its holder can be waiting
// for us to acknowledge stop/update. Commands also remain available at backoff.
func awaitRestart(ctx *context.Context, rec *store.ContainerRecord, event win32.HANDLE, delay time.Duration) (*store.ContainerOperation, *store.ContainerRecord, error) {
	deadline := time.Now().Add(delay)
	for {
		current, err := ctx.Store.ReadContainer(rec.ID)
		if err != nil {
			return nil, rec, err
		}
		rec = current
		if !shouldRestart(rec) {
			rec, err = ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
				c.State.Restarting = false
				c.State.Status = "exited"
				return nil
			})
			return nil, rec, err
		}
		if !time.Now().Before(deadline) {
			op, err := ctx.Store.TryLockContainer(rec.ID)
			if err != nil {
				return nil, rec, err
			}
			if op != nil {
				// An update may have committed between our read and lock claim.
				current, err := ctx.Store.ReadContainer(rec.ID)
				if err != nil {
					op.Release()
					return nil, rec, err
				}
				if shouldRestart(current) {
					return op, current, nil
				}
				op.Release()
				continue
			}
		}
		switch win32.WaitForSingleObject(event, 20) {
		case win32.WAIT_OBJECT_0:
			serveControlRequest(ctx, rec, func(request controlRequest) error {
				if request.Action != "" && request.Signal != "" {
					return errors.New("control action cannot include a signal")
				}
				if request.Action == actionUpdate && request.Update != nil {
					if request.Update.Empty() {
						return errors.New("update requires at least one setting")
					}
					_, err := saveContainerUpdate(ctx, rec, *request.Update)
					return err
				}
				if request.Action == actionCancelRestart || request.Signal == SignalTERM || request.Signal == SignalKILL {
					_, err := suppressRestart(ctx, rec)
					return err
				}
				return errors.New("container is restarting; wait for a running process")
			})
		case win32.WAIT_TIMEOUT:
		default:
			return nil, rec, fmt.Errorf("waiting for restart control failed")
		}
	}
}
