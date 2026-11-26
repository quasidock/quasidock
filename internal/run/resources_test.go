package run

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestResourceParsing(t *testing.T) {
	for _, value := range []string{"NaN", "Inf", "-1", "1/2", "0x1", "1e1", "0.000000001", "999999999999999999"} {
		if _, err := ParseCPUs(value); err == nil {
			t.Errorf("accepted CPU %q", value)
		}
	}
	if n, err := ParseCPUs(".5"); err != nil || n != 500_000_000 {
		t.Fatalf("CPU: %d %v", n, err)
	}
	for value, want := range map[string]int64{"0": 0, "0m": 0, "512m": 512 << 20, "1.5GiB": 1536 << 20, "1024": 1024, "9223372036854775807": 1<<63 - 1} {
		if got, err := ParseMemory(value); err != nil || got != want {
			t.Errorf("memory %q: %d %v, want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"NaN", "-1", "Infinity", "1e500", "9223372036854775808", "99999999999999999999999g", "0.1b", "12watts"} {
		if _, err := ParseMemory(value); err == nil {
			t.Errorf("accepted memory %q", value)
		}
	}
	for _, value := range []string{"-2", "4294967296", "1.5", ""} {
		if _, err := ParsePidsLimit(value); err == nil {
			t.Errorf("accepted PIDs %q", value)
		}
	}
	for _, value := range []string{"no", "on-failure", "on-failure:0", "on-failure:3", "always", "unless-stopped"} {
		if _, err := ParseRestartPolicy(value); err != nil {
			t.Errorf("policy %q: %v", value, err)
		}
	}
	for _, value := range []string{"", "yes", "no:1", "always:2", "on-failure:", "on-failure:-1"} {
		if _, err := ParseRestartPolicy(value); err == nil {
			t.Errorf("accepted policy %q", value)
		}
	}
}

func TestUpdateResourcesAndPersistenceRollback(t *testing.T) {
	ctx, rec, live := pauseTestRuntime(t)
	memory, pids, cpus := int64(256<<20), int64(8), int64(500_000_000)
	opts := UpdateOptions{Memory: &memory, PidsLimit: &pids, NanoCPUs: &cpus}
	if err := live.control(ctx, rec, controlRequest{Action: actionUpdate, Update: &opts}, true); err != nil {
		t.Fatal(err)
	}
	old, err := win32.QueryJobLimits(live.job)
	if err != nil {
		t.Fatal(err)
	}
	if old.Extended.JobMemoryLimit != uintptr(memory) || old.Extended.BasicLimitInformation.ActiveProcessLimit != 8 || old.CPU.Flags != 5 {
		t.Fatalf("kernel limits: %+v", old)
	}
	// The saved state must agree with the kernel after a successful request.
	got, err := ctx.Store.ReadContainer(rec.ID)
	if err != nil || got.HostConfig.Memory != memory || got.HostConfig.NanoCPUs != cpus {
		t.Fatalf("saved limits: %+v %v", got, err)
	}
	if err := live.control(ctx, rec, controlRequest{Action: actionPause}, true); err != nil {
		t.Fatal(err)
	}
	// Prevent atomic writes while preserving readable state and working locks.
	tmp := ctx.Store.Path("tmp")
	if err := os.Rename(tmp, tmp+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(tmp); _ = os.Rename(tmp+"-saved", tmp) })
	memory = 512 << 20
	if err := live.control(ctx, rec, controlRequest{Action: actionUpdate, Update: &opts}, true); err == nil {
		t.Fatal("persistence failure was ignored")
	}
	after, err := win32.QueryJobLimits(live.job)
	if err != nil || after != old {
		t.Fatalf("kernel rollback: %+v %v, wanted %+v", after, err, old)
	}
	if frozen, err := win32.IsJobFrozen(live.job); err != nil || !frozen {
		t.Fatalf("update changed pause state: %t %v", frozen, err)
	}
}

func TestRestartDecisionsAndBackoff(t *testing.T) {
	for _, tc := range []struct {
		policy        string
		exit, count   int
		stopped, want bool
	}{
		{"no", 1, 0, false, false}, {"on-failure:2", 1, 0, false, true}, {"on-failure:2", 1, 2, false, false},
		{"on-failure", 0, 0, false, false}, {"on-failure", 1, 1000, false, true}, {"always", 0, 0, false, true},
		{"unless-stopped", 0, 0, false, true}, {"always", 1, 0, true, false}, {"unless-stopped", 1, 0, true, false},
	} {
		p, _ := ParseRestartPolicy(tc.policy)
		rec := &store.ContainerRecord{RestartCount: tc.count, HostConfig: store.ContainerHostConfig{RestartPolicy: p}, State: store.ContainerState{Launched: true, ExitCode: tc.exit, RestartSuppressed: tc.stopped}}
		if got := shouldRestart(rec); got != tc.want {
			t.Errorf("%+v: %t", tc, got)
		}
		rec.State.Launched = false
		if shouldRestart(rec) {
			t.Fatal("retrying a process-creation failure")
		}
	}
	delay := time.Duration(0)
	for _, want := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond} {
		delay = restartDelay(delay, 0)
		if delay != want {
			t.Fatalf("delay: %s", delay)
		}
	}
	if restartDelay(time.Minute, 0) != time.Minute || restartDelay(time.Minute, 10*time.Second) != 100*time.Millisecond {
		t.Fatal("backoff cap/reset")
	}
}

func TestRestartBackoffControlWhileOperationLocked(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "update", true: "stop"}[cancel], func(t *testing.T) {
			ctx, rec, _ := pauseTestRuntime(t)
			var err error
			rec, err = ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
				c.State.Running = false
				c.State.Status, c.State.Restarting = "restarting", true
				c.HostConfig.RestartPolicy = store.RestartPolicy{Name: "always"}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			op, err := ctx.Store.LockContainer(rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer op.Release()
			event, err := win32.CreateEvent(controlEventName(rec.ID, controlKey(rec)), false, false)
			if err != nil {
				t.Fatal(err)
			}
			defer win32.CloseHandle(event)
			done := make(chan error, 1)
			go func() {
				op, _, err := awaitRestart(ctx, rec, event, 0)
				if op != nil {
					op.Release()
					err = errors.New("automatic launch bypassed operation lock")
				}
				done <- err
			}()
			request := controlRequest{Action: actionCancelRestart}
			memory := int64(256 << 20)
			policy := store.RestartPolicy{Name: "no"}
			if !cancel {
				request = controlRequest{Action: actionUpdate, Update: &UpdateOptions{Memory: &memory, RestartPolicy: &policy}}
			}
			if err := sendControlRequest(ctx, rec, request); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("control deadlocked with operation lock")
			}
			got, err := ctx.Store.ReadContainer(rec.ID)
			if err != nil || got.State.Restarting || (cancel && !got.State.RestartSuppressed) || (!cancel && got.HostConfig.Memory != memory) {
				t.Fatalf("pending control: %+v %v", got, err)
			}
		})
	}
}

func TestUpdateRejectsOldSupervisor(t *testing.T) {
	ctx, rec, _ := pauseTestRuntime(t)
	_, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error { c.State.ControlProtocol = 1; return nil })
	if err != nil {
		t.Fatal(err)
	}
	memory := int64(256 << 20)
	if _, err := UpdateContainer(ctx, rec.ID, UpdateOptions{Memory: &memory}); err == nil {
		t.Fatal("old live supervisor accepted update")
	}
	got, err := ctx.Store.ReadContainer(rec.ID)
	if err != nil || got.HostConfig.Memory != 0 {
		t.Fatalf("old supervisor config changed: %+v %v", got, err)
	}
	if _, err := RenameContainer(ctx, rec.ID, "legacy-renamed"); err != nil {
		t.Fatal(err)
	}
}
