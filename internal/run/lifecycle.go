package run

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/store"
)

func newRunID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func snapshotInheritedEnv(host map[string]string, spec *store.ContainerRunSpec) {
	if spec.InheritedEnv == nil {
		spec.InheritedEnv = make(map[string]*string)
	}
	for _, name := range spec.Env {
		if strings.Contains(name, "=") {
			continue
		}
		key := strings.ToLower(name)
		if _, saved := spec.InheritedEnv[key]; saved {
			continue
		}
		var value *string
		for k, v := range host {
			if strings.EqualFold(k, name) {
				value = &v
				break
			}
		}
		spec.InheritedEnv[key] = value
	}
}

func containerHostEnv(host map[string]string, spec store.ContainerRunSpec) map[string]string {
	env := maps.Clone(host)
	if env == nil {
		env = make(map[string]string)
	}
	for name, value := range spec.InheritedEnv {
		for k := range env {
			if strings.EqualFold(k, name) {
				delete(env, k)
			}
		}
		if value != nil {
			env[name] = *value
		}
	}
	return env
}

func lockTarget(ctx *context.Context, target string) (*store.ContainerRecord, *store.ContainerOperation, error) {
	rec, err := ctx.Store.ResolveContainer(target)
	if err != nil {
		return nil, nil, err
	}
	op, err := ctx.Store.LockContainer(rec.ID)
	if err != nil {
		return nil, nil, err
	}
	rec, err = ctx.Store.ReadContainer(rec.ID)
	if err != nil {
		op.Release()
		return nil, nil, err
	}
	return rec, op, nil
}

// PauseContainer freezes the current run's job without releasing its resources.
func PauseContainer(ctx *context.Context, target string) (*store.ContainerRecord, error) {
	return pauseContainer(ctx, target, actionPause)
}

// UnpauseContainer resumes the same processes in a paused container.
func UnpauseContainer(ctx *context.Context, target string) (*store.ContainerRecord, error) {
	return pauseContainer(ctx, target, actionUnpause)
}

func pauseContainer(ctx *context.Context, target string, action controlAction) (*store.ContainerRecord, error) {
	rec, op, err := lockTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	if !rec.State.Running || !ctx.Store.IsContainerRunning(rec.ID) {
		return nil, fmt.Errorf("container %s is not running", rec.ShortID())
	}
	if err := sendControlRequest(ctx, rec, controlRequest{Action: action}); err != nil {
		return nil, err
	}
	return ctx.Store.ReadContainer(rec.ID)
}

// StartContainer starts the recorded image and command under a new supervisor.
func StartContainer(ctx *context.Context, target string) (*store.ContainerRecord, error) {
	rec, op, err := lockTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	if ctx.Store.IsContainerRunning(rec.ID) {
		return rec, nil
	}
	return startContainerLocked(ctx, rec)
}

func startContainerLocked(ctx *context.Context, rec *store.ContainerRecord) (*store.ContainerRecord, error) {
	rec, err := prepareStart(ctx, rec)
	if err != nil {
		return nil, err
	}
	if err := launchSupervisor(ctx, rec, rec.RunSpec.HostCwd); err != nil {
		if !ctx.Store.IsContainerRunning(rec.ID) {
			recordLaunchFailure(ctx.Store, rec.ID, rec.State.RunID, err)
		}
		return rec, err
	}
	return ctx.Store.ReadContainer(rec.ID)
}

func prepareStart(ctx *context.Context, rec *store.ContainerRecord) (*store.ContainerRecord, error) {
	runID, err := newRunID()
	if err != nil {
		return nil, err
	}
	cwd := rec.RunSpec.HostCwd
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	rec, err = ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
		snapshotInheritedEnv(ctx.Env, &c.RunSpec)
		c.RunSpec.HostCwd = cwd
		c.Config.AttachStdin = false
		c.RestartCount = 0
		c.State = store.ContainerState{Status: "created", RunID: runID, SupervisorID: runID, ControlProtocol: controlProtocol}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// RestartContainer preserves the record through both halves of the operation,
// including containers whose normal exit policy is automatic removal.
func RestartContainer(ctx *context.Context, target string, timeoutSec int) (*store.ContainerRecord, error) {
	if err := validateStopTimeout(timeoutSec); err != nil {
		return nil, err
	}
	rec, op, err := lockTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	if _, err := stopContainerLocked(ctx, rec, op, timeoutSec, true); err != nil {
		return nil, err
	}
	started, err := startContainerLocked(ctx, rec)
	if err == nil {
		ctx.Store.RecordContainerEvent(started, "restart", nil)
	}
	return started, err
}

func validateStopTimeout(seconds int) error {
	if seconds < 0 || uint64(seconds) > uint64((1<<63-1)/int64(time.Second)) {
		return errors.New("stop time must be a non-negative number of seconds within the supported duration")
	}
	return nil
}

// StopContainer requests graceful shutdown, then kills the job at the deadline.
func StopContainer(ctx *context.Context, target string, timeoutSec int) (*store.ContainerRecord, error) {
	if err := validateStopTimeout(timeoutSec); err != nil {
		return nil, err
	}
	rec, op, err := lockTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	wasRunning := ctx.Store.IsContainerRunning(rec.ID)
	updated, err := stopContainerLocked(ctx, rec, op, timeoutSec, true)
	if err != nil || !wasRunning {
		return updated, err
	}
	ctx.Store.RecordContainerEvent(updated, "stop", nil)
	return finishStopped(ctx, updated, op, false)
}

func stopContainerLocked(ctx *context.Context, rec *store.ContainerRecord, op *store.ContainerOperation, seconds int, preserve bool) (*store.ContainerRecord, error) {
	if !ctx.Store.IsContainerRunning(rec.ID) {
		return rec, nil
	}
	if seconds > 0 {
		if rec.State.Paused {
			if err := sendControlRequest(ctx, rec, controlRequest{Action: actionResume}); err != nil {
				if ctx.Store.IsContainerRunning(rec.ID) {
					return nil, err
				}
				return finishStopped(ctx, rec, op, preserve)
			}
		}
		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		// Foreground runs deliberately retain their caller's process group.
		// They cannot receive a targeted console signal; still honor the grace
		// period before ending their job.
		err := sendControl(ctx, rec, SignalTERM)
		if err != nil && !errors.Is(err, errForegroundTERM) && !errors.Is(err, errSignalDelivery) && ctx.Store.IsContainerRunning(rec.ID) {
			return nil, err
		}
		waitForContainer(ctx.Store, rec.ID, deadline)
	}
	if ctx.Store.IsContainerRunning(rec.ID) {
		deadline := time.Now().Add(5 * time.Second)
		if err := sendControl(ctx, rec, SignalKILL); err != nil {
			return nil, err
		}
		if !waitForContainer(ctx.Store, rec.ID, deadline) {
			return nil, fmt.Errorf("container %s did not finish cleanup after KILL", rec.ShortID())
		}
	}
	return finishStopped(ctx, rec, op, preserve)
}

func waitForContainer(st *store.Store, id string, deadline time.Time) bool {
	for st.IsContainerRunning(id) {
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

func finishStopped(ctx *context.Context, rec *store.ContainerRecord, op *store.ContainerOperation, preserve bool) (*store.ContainerRecord, error) {
	updated, err := ctx.Store.ReadContainer(rec.ID)
	if err != nil {
		return nil, err
	}
	if updated.HostConfig.AutoRemove && !preserve {
		_ = hkcu.SettleLeftovers(ctx.Store, rec.AppID)
		if err := op.Remove(); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// KillContainer sends TERM without escalation, or immediately terminates the job.
func KillContainer(ctx *context.Context, target string, signal KillSignal) (*store.ContainerRecord, error) {
	if signal != SignalKILL && signal != SignalTERM {
		return nil, fmt.Errorf("unsupported signal %q", signal)
	}
	rec, op, err := lockTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	if !ctx.Store.IsContainerRunning(rec.ID) {
		return nil, fmt.Errorf("container %s is not running", rec.ShortID())
	}
	deadline := time.Now().Add(5 * time.Second)
	if err := sendControl(ctx, rec, signal); err != nil {
		return nil, err
	}
	if signal == SignalTERM {
		return ctx.Store.ReadContainer(rec.ID)
	}
	if !waitForContainer(ctx.Store, rec.ID, deadline) {
		return nil, fmt.Errorf("container %s did not finish cleanup after KILL", rec.ShortID())
	}
	return finishStopped(ctx, rec, op, false)
}

// RemoveContainer holds one operation lock across forced shutdown and deletion.
func RemoveContainer(ctx *context.Context, target string, force bool) (*store.ContainerRecord, error) {
	rec, op, err := lockTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	if ctx.Store.IsContainerRunning(rec.ID) {
		if !force {
			return nil, fmt.Errorf("cannot remove running container %s: stop it first or use -f", rec.ShortID())
		}
		if _, err := stopContainerLocked(ctx, rec, op, 0, true); err != nil {
			return nil, err
		}
	}
	_ = hkcu.SettleLeftovers(ctx.Store, rec.AppID)
	if err := op.Remove(); err != nil {
		return nil, err
	}
	return rec, grants.RepairMounts(ctx.Store)
}

func autoRemoveContainer(ctx *context.Context, rec *store.ContainerRecord) {
	op, err := ctx.Store.LockContainer(rec.ID)
	if err != nil {
		return
	}
	defer op.Release()
	current, err := ctx.Store.ReadContainer(rec.ID)
	if err != nil || current.State.RunID != rec.State.RunID || ctx.Store.IsContainerRunning(rec.ID) {
		return
	}
	_ = hkcu.SettleLeftovers(ctx.Store, rec.AppID)
	if err := op.Remove(); err != nil {
		ctx.Note("warning: removing container %s: %v", rec.ShortID(), err)
	}
}
