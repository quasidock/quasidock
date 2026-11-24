package run

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func pauseTestRuntime(t *testing.T) (*context.Context, *store.ContainerRecord, *execRuntime) {
	t.Helper()
	ctx := context.New()
	var err error
	ctx.Store, err = store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	runID, err := newRunID()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := ctx.Store.CreateContainer("pause", store.ContainerRecord{State: store.ContainerState{
		Status: "running", Running: true, Launched: true, RunID: runID,
		ControlProtocol: controlProtocol, StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}})
	if err != nil {
		t.Fatal(err)
	}
	lk, err := store.OpenLock(ctx.Store.ContainerRunLockPath(rec.ID), true, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lk.Release)
	job, err := process.NewJob()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { win32.CloseHandle(job) })
	return ctx, rec, &execRuntime{primary: win32.GetCurrentProcess(), job: job, runID: runID}
}

func TestPauseControlState(t *testing.T) {
	ctx, rec, live := pauseTestRuntime(t)
	for _, action := range []controlAction{actionPause, actionUnpause} {
		if err := live.control(ctx, rec, controlRequest{Action: action}, true); err != nil {
			t.Fatal(err)
		}
		paused := action == actionPause
		current, err := ctx.Store.ReadContainer(rec.ID)
		if err != nil || current.State.Paused != paused || !current.State.Running || current.State.StartedAt != rec.State.StartedAt || current.State.RunID != rec.State.RunID {
			t.Fatalf("%s: %+v %v", action, current, err)
		}
		status := "running"
		if paused {
			status = "paused"
		}
		if current.State.Status != status {
			t.Fatalf("status: %s", current.State.Status)
		}
		if frozen, err := win32.IsJobFrozen(live.job); err != nil || frozen != paused || live.paused != paused {
			t.Fatalf("job and recorded state disagree: %t %t %v", frozen, live.paused, err)
		}
		if err := live.control(ctx, rec, controlRequest{Action: action}, true); err == nil {
			t.Fatalf("repeated %s succeeded", action)
		}
		if err := live.attachAllowed(); (err != nil) != paused {
			t.Fatalf("attach paused=%t: %v", paused, err)
		}
		if paused {
			if err := live.control(ctx, rec, controlRequest{Signal: SignalTERM}, false); !errors.Is(err, errForegroundTERM) || !live.paused {
				t.Fatalf("foreground TERM changed pause state: %v", err)
			}
		}
	}
	if err := live.control(ctx, rec, controlRequest{Action: actionResume}, true); err != nil {
		t.Fatalf("shutdown resume should be idempotent: %v", err)
	}
	for _, request := range []controlRequest{{Action: "unknown"}, {Action: actionPause, Signal: SignalKILL}} {
		if err := live.control(ctx, rec, request, true); err == nil || live.paused {
			t.Fatalf("accepted invalid control: %+v %v", request, err)
		}
	}
	stale := *rec
	stale.State.RunID = strings.Repeat("a", 32)
	if err := live.control(ctx, &stale, controlRequest{Action: actionPause}, true); err == nil || live.paused {
		t.Fatal("accepted a stale run")
	}
	job := live.job
	live.job = 0
	if err := live.control(ctx, rec, controlRequest{Action: actionPause}, true); err == nil || live.paused {
		t.Fatal("failed Windows operation changed runtime state")
	}
	live.job = job
}

func TestPausePersistenceRollback(t *testing.T) {
	for _, initiallyPaused := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause", true: "unpause"}[initiallyPaused], func(t *testing.T) {
			ctx, rec, live := pauseTestRuntime(t)
			if initiallyPaused {
				if err := live.control(ctx, rec, controlRequest{Action: actionPause}, true); err != nil {
					t.Fatal(err)
				}
			}
			// Make atomic state writes fail without preventing reads or locking.
			tmp := ctx.Store.Path("tmp")
			backup := tmp + "-saved"
			if err := os.Rename(tmp, backup); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Remove(tmp); err != nil {
					t.Error(err)
				}
				if err := os.Rename(backup, tmp); err != nil {
					t.Error(err)
				}
			})
			if err := os.WriteFile(tmp, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			action := actionPause
			if initiallyPaused {
				action = actionUnpause
			}
			if err := live.control(ctx, rec, controlRequest{Action: action}, true); err == nil {
				t.Fatal("pause transition ignored persistence failure")
			}
			current, err := ctx.Store.ReadContainer(rec.ID)
			if err != nil || current.State.Paused != initiallyPaused || live.paused != initiallyPaused {
				t.Fatalf("failure changed saved/runtime state: %+v %v", current, err)
			}
			if frozen, err := win32.IsJobFrozen(live.job); err != nil || frozen != initiallyPaused {
				t.Fatalf("Windows state was not rolled back: %t %v", frozen, err)
			}
		})
	}
}

func TestPauseRejectsSupersededState(t *testing.T) {
	ctx, rec, live := pauseTestRuntime(t)
	_, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
		c.State.RunID = "replacement"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := live.control(ctx, rec, controlRequest{Action: actionPause}, true); err == nil {
		t.Fatal("pause overwrote replacement state")
	}
	if frozen, err := win32.IsJobFrozen(live.job); err != nil || frozen {
		t.Fatalf("superseded pause left a frozen job: %t %v", frozen, err)
	}
}

func TestPauseAttachAdmission(t *testing.T) {
	ctx, rec, live := pauseTestRuntime(t)
	attachment, id, runID := attachTestRuntime(t, false)
	attachment.mu.Lock()
	attachment.admit = live.attachAllowed
	attachment.mu.Unlock()
	attachment.markStarted(func() error { return nil })
	if err := live.control(ctx, rec, controlRequest{Action: actionPause}, true); err != nil {
		t.Fatal(err)
	}
	_, kind, data := attachTestDial(t, id, runID, false)
	if kind != attachError || !strings.Contains(string(data), "paused") {
		t.Fatalf("owner admitted a new paused attachment: %d %q", kind, data)
	}
	if err := live.control(ctx, rec, controlRequest{Action: actionUnpause}, true); err != nil {
		t.Fatal(err)
	}
	c, kind, data := attachTestDial(t, id, runID, false)
	if kind != attachReady {
		t.Fatalf("owner rejected attachment after unpause: %d %q", kind, data)
	}
	expectAttachFrame(t, c, attachStarted)
	_ = c.Close()
}

func TestPauseRequiresAcknowledgementAndCapability(t *testing.T) {
	ctx, rec, _ := pauseTestRuntime(t)
	older := *rec
	older.State.ControlProtocol = 0
	if err := sendControlRequest(ctx, &older, controlRequest{Action: actionPause}); err == nil || !strings.Contains(err.Error(), "stop and start") {
		t.Fatalf("older supervisor: %v", err)
	}
	// A missing supervisor must yield an error for pause/unpause, while existing
	// signal delivery tolerates natural exit.
	exited, err := ctx.Store.CreateContainer("exited", store.ContainerRecord{State: rec.State})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []controlAction{actionPause, actionUnpause} {
		if err := sendControlRequest(ctx, exited, controlRequest{Action: action}); err == nil || !strings.Contains(err.Error(), "exited before acknowledging") {
			t.Fatalf("natural exit reported success for %s: %v", action, err)
		}
	}
	if err := sendControl(ctx, exited, SignalKILL); err != nil {
		t.Fatalf("natural exit no longer tolerated for KILL: %v", err)
	}
}

func TestPauseExitDuringAcknowledgement(t *testing.T) {
	for _, action := range []controlAction{actionPause, actionUnpause} {
		t.Run(string(action), func(t *testing.T) {
			ctx, rec, _ := pauseTestRuntime(t)
			target, err := ctx.Store.CreateContainer("racing-exit", store.ContainerRecord{State: rec.State})
			if err != nil {
				t.Fatal(err)
			}
			lk, err := store.OpenLock(ctx.Store.ContainerRunLockPath(target.ID), true, false)
			if err != nil {
				t.Fatal(err)
			}
			defer lk.Release()
			event, err := win32.CreateEvent(controlEventName(target.ID, target.State.RunID), false, false)
			if err != nil {
				t.Fatal(err)
			}
			defer win32.CloseHandle(event)
			done := make(chan win32.DWORD, 1)
			go func() {
				result := win32.WaitForSingleObject(event, 5000)
				lk.Release() // Simulate the supervisor exiting after delivery, without an ack.
				done <- result
			}()
			if err := sendControlRequest(ctx, target, controlRequest{Action: action}); err == nil || !strings.Contains(err.Error(), "exited before acknowledging") {
				t.Fatalf("exit while waiting for acknowledgement: %v", err)
			}
			if result := <-done; result != win32.WAIT_OBJECT_0 {
				t.Fatalf("control request was not delivered: %d", result)
			}
		})
	}
}

func TestPauseRejectsMismatchedAcknowledgement(t *testing.T) {
	ctx, rec, _ := pauseTestRuntime(t)
	event, err := win32.CreateEvent(controlEventName(rec.ID, rec.State.RunID), false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(event)
	done := make(chan error, 1)
	go func() {
		if win32.WaitForSingleObject(event, 5000) != win32.WAIT_OBJECT_0 {
			done <- errors.New("control request was not delivered")
			return
		}
		data, err := os.ReadFile(controlPath(ctx.Store, rec.ID, rec.State.RunID))
		var request controlRequest
		if err == nil {
			err = json.Unmarshal(data, &request)
		}
		if err == nil {
			err = writeControlJSON(ctx.Store, controlResultPath(ctx.Store, rec.ID, request.RequestID), controlResult{RequestID: "stale"})
		}
		if err == nil {
			var ack win32.HANDLE
			ack, err = win32.OpenEvent(controlAckName(request.RequestID), win32.EVENT_MODIFY_STATE)
			if err == nil {
				win32.SetEvent(ack)
				win32.CloseHandle(ack)
			}
		}
		done <- err
	}()
	if err := sendControlRequest(ctx, rec, controlRequest{Action: actionPause}); err == nil || !strings.Contains(err.Error(), "did not match") {
		t.Fatalf("accepted a stale acknowledgement: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
