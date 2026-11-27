package run

import (
	stdcontext "context"
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

func lockTargetContext(cancel stdcontext.Context, ctx *context.Context, target string) (*store.ContainerRecord, *store.ContainerOperation, error) {
	rec, err := ctx.Store.ResolveContainerContext(cancel, target)
	if err != nil {
		return nil, nil, err
	}
	for cancel.Err() == nil {
		op, err := ctx.Store.TryLockContainer(rec.ID)
		if err != nil {
			return nil, nil, err
		}
		if op != nil {
			current, err := ctx.Store.ReadContainerContext(cancel, rec.ID)
			if err != nil {
				op.Release()
				return nil, nil, err
			}
			return current, op, nil
		}
		select {
		case <-cancel.Done():
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil, nil, cancel.Err()
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
func RestartContainer(cancel stdcontext.Context, ctx *context.Context, target string, timeoutOverride *int) (*store.ContainerRecord, error) {
	rec, op, err := lockTargetContext(cancel, ctx, target)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	stopped, err := stopContainerLocked(cancel, ctx, rec, timeoutOverride)
	if err != nil {
		return nil, err
	}
	if err := cancel.Err(); err != nil {
		return nil, err
	}
	started, err := startContainerLocked(ctx, stopped)
	if err == nil {
		ctx.Store.RecordContainerEvent(started, "restart", nil)
	}
	return started, err
}

const defaultStopTimeout = 10

func ValidateStopTimeout(seconds int) error {
	if seconds != -1 && (seconds < 0 || uint64(seconds) > uint64((1<<63-1)/int64(time.Second))) {
		return errors.New("stop time must be -1 or non-negative seconds within the supported duration")
	}
	return nil
}

func stopSettings(rec *store.ContainerRecord, timeoutOverride *int) (KillSignal, int, error) {
	signal := KillSignal(rec.Config.StopSignal)
	if signal == "" {
		signal = SignalTERM
	}
	if !signal.valid() {
		return "", 0, fmt.Errorf("unsupported stop signal %q", signal)
	}
	timeout := defaultStopTimeout
	if timeoutOverride != nil {
		timeout = *timeoutOverride
	} else if rec.Config.StopTimeout != nil {
		timeout = *rec.Config.StopTimeout
	}
	return signal, timeout, ValidateStopTimeout(timeout)
}

// StopContainer shuts down the running job using the container's saved settings.
func StopContainer(cancel stdcontext.Context, ctx *context.Context, target string, timeoutOverride *int) (*store.ContainerRecord, error) {
	rec, op, err := lockTargetContext(cancel, ctx, target)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	if !ctx.Store.IsContainerRunning(rec.ID) {
		return rec, nil
	}
	updated, err := stopContainerLocked(cancel, ctx, rec, timeoutOverride)
	if err != nil {
		return nil, err
	}
	ctx.Store.RecordContainerEvent(updated, "stop", nil)
	return finishStopped(ctx, updated, op)
}

func stopContainerLocked(cancel stdcontext.Context, ctx *context.Context, rec *store.ContainerRecord, timeoutOverride *int) (*store.ContainerRecord, error) {
	if err := cancel.Err(); err != nil {
		return nil, err
	}
	if !ctx.Store.IsContainerRunning(rec.ID) {
		return rec, nil
	}
	signal, seconds, err := stopSettings(rec, timeoutOverride)
	if err != nil {
		return nil, err
	}
	if signal == SignalTERM && seconds != 0 {
		if rec.State.Paused {
			if err := sendControlRequest(ctx, rec, controlRequest{Action: actionResume}); err != nil {
				if ctx.Store.IsContainerRunning(rec.ID) {
					return nil, err
				}
				return ctx.Store.ReadContainer(rec.ID)
			}
		}
		if err := cancel.Err(); err != nil {
			return nil, err
		}
		// A finite stop can still escalate if the target has no signalable console.
		err := sendControl(ctx, rec, SignalTERM)
		if err != nil && ctx.Store.IsContainerRunning(rec.ID) &&
			(seconds == -1 || (!errors.Is(err, errForegroundTERM) && !errors.Is(err, errSignalDelivery))) {
			return nil, err
		}
		var deadline time.Time
		if seconds > 0 {
			deadline = time.Now().Add(time.Duration(seconds) * time.Second)
		}
		if waitForContainerContext(cancel, ctx.Store, rec.ID, deadline) {
			return ctx.Store.ReadContainer(rec.ID)
		}
	}
	if err := cancel.Err(); err != nil {
		return nil, err
	}
	if err := forceStopContainer(ctx, rec); err != nil {
		return nil, err
	}
	return ctx.Store.ReadContainer(rec.ID)
}

func forceStopContainer(ctx *context.Context, rec *store.ContainerRecord) error {
	if !ctx.Store.IsContainerRunning(rec.ID) {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	if err := sendControl(ctx, rec, SignalKILL); err != nil {
		return err
	}
	if !waitForContainer(ctx.Store, rec.ID, deadline) {
		return fmt.Errorf("container %s did not finish cleanup after KILL", rec.ShortID())
	}
	return nil
}

func waitForContainer(st *store.Store, id string, deadline time.Time) bool {
	return waitForContainerContext(stdcontext.Background(), st, id, deadline)
}

func waitForContainerContext(cancel stdcontext.Context, st *store.Store, id string, deadline time.Time) bool {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for st.IsContainerRunning(id) {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-cancel.Done():
			return false
		case <-ticker.C:
		}
	}
	return true
}

func finishStopped(ctx *context.Context, rec *store.ContainerRecord, op *store.ContainerOperation) (*store.ContainerRecord, error) {
	updated, err := ctx.Store.ReadContainer(rec.ID)
	if err != nil {
		return nil, err
	}
	if updated.HostConfig.AutoRemove {
		_ = hkcu.SettleLeftovers(ctx.Store, rec.AppID)
		if err := removeStoppedContainer(ctx, updated, op, true); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// KillContainer sends TERM without escalation, or immediately terminates the job.
func KillContainer(ctx *context.Context, target string, signal KillSignal) (*store.ContainerRecord, error) {
	if !signal.valid() {
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
	if signal == SignalTERM {
		if err := sendControl(ctx, rec, signal); err != nil {
			return nil, err
		}
		return ctx.Store.ReadContainer(rec.ID)
	}
	if err := forceStopContainer(ctx, rec); err != nil {
		return nil, err
	}
	return finishStopped(ctx, rec, op)
}

// RemoveContainer holds one operation lock across forced shutdown and deletion.
func RemoveContainer(ctx *context.Context, target string, force, volumes bool) (*store.ContainerRecord, error) {
	rec, op, err := lockTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	if ctx.Store.IsContainerRunning(rec.ID) {
		if !force {
			return nil, fmt.Errorf("cannot remove running container %s: stop it first or use -f", rec.ShortID())
		}
		if err := forceStopContainer(ctx, rec); err != nil {
			return nil, err
		}
	}
	_ = hkcu.SettleLeftovers(ctx.Store, rec.AppID)
	if err := removeStoppedContainer(ctx, rec, op, volumes); err != nil {
		return nil, err
	}
	return rec, grants.RepairMounts(ctx.Store)
}

func removeStoppedContainer(ctx *context.Context, rec *store.ContainerRecord, op *store.ContainerOperation, volumes bool) error {
	if volumes {
		for _, mount := range rec.HostConfig.Mounts {
			if !mount.Anonymous {
				continue
			}
			if err := grants.RepairMounts(ctx.Store); err != nil {
				return removeWithoutVolumeCleanup(rec, op, err)
			}
			guard, err := grants.LockMountSources(ctx.Store)
			if err != nil {
				return removeWithoutVolumeCleanup(rec, op, err)
			}
			defer guard.Close()
			return op.RemoveWithVolumes(guard.Protected)
		}
	}
	return op.Remove()
}

func removeWithoutVolumeCleanup(rec *store.ContainerRecord, op *store.ContainerOperation, cleanupErr error) error {
	if err := op.Remove(); err != nil {
		return errors.Join(err, cleanupErr)
	}
	return fmt.Errorf("container %s was removed, but its anonymous volumes were retained: %w", rec.BareName(), cleanupErr)
}

func autoRemoveContainer(ctx *context.Context, rec *store.ContainerRecord) {
	op, err := ctx.Store.LockContainer(rec.ID)
	if err != nil {
		return
	}
	defer op.Release()
	autoRemoveContainerLocked(ctx, rec, op)
}

func autoRemoveContainerLocked(ctx *context.Context, rec *store.ContainerRecord, op *store.ContainerOperation) {
	current, err := ctx.Store.ReadContainer(rec.ID)
	if err != nil || current.State.RunID != rec.State.RunID || ctx.Store.IsContainerRunning(rec.ID) {
		return
	}
	_ = hkcu.SettleLeftovers(ctx.Store, rec.AppID)
	if err := removeStoppedContainer(ctx, current, op, true); err != nil {
		ctx.Note("warning: removing container %s: %v", rec.ShortID(), err)
	}
}
