package run

import (
	stdcontext "context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestStopSettings(t *testing.T) {
	rec := &store.ContainerRecord{}
	signal, timeout, err := stopSettings(rec, nil)
	if err != nil || signal != SignalTERM || timeout != 10 {
		t.Fatalf("legacy defaults: %q %d %v", signal, timeout, err)
	}
	saved, override := -1, 0
	rec.Config.StopSignal, rec.Config.StopTimeout = string(SignalKILL), &saved
	signal, timeout, err = stopSettings(rec, &override)
	if err != nil || signal != SignalKILL || timeout != 0 || *rec.Config.StopTimeout != -1 {
		t.Fatalf("explicit override: %q %d %v, saved %d", signal, timeout, err, *rec.Config.StopTimeout)
	}
}

func TestStopContainerCancelsWhileOperationLocked(t *testing.T) {
	ctx := context.New()
	var err error
	ctx.Store, err = store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := ctx.Store.CreateContainer("locked", store.ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	op, err := ctx.Store.LockContainer(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Release()
	cancel, stop := stdcontext.WithTimeout(stdcontext.Background(), 100*time.Millisecond)
	defer stop()
	result := make(chan error, 1)
	go func() {
		_, err := StopContainer(cancel, ctx, rec.ID, nil)
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, stdcontext.DeadlineExceeded) {
			t.Fatalf("stop while holding the operation lock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not cancel while waiting for the operation lock")
	}
}

func TestInheritedEnvironmentSnapshot(t *testing.T) {
	host := map[string]string{"Mixed_Case": "original", "OTHER": "unchanged"}
	spec := store.ContainerRunSpec{Env: []string{"MIXED_CASE", "missing", "FIXED=literal", "mixed_case=last"}}
	snapshotInheritedEnv(host, &spec)
	host["Mixed_Case"] = "changed"
	host["missing"] = "new"
	snapshotInheritedEnv(host, &spec) // Starting a legacy record snapshots only once.
	saved := containerHostEnv(host, spec)
	if _, present := saved["missing"]; present || saved["mixed_case"] != "original" || saved["OTHER"] != "unchanged" {
		t.Fatalf("snapshot did not preserve value/absence: %#v", saved)
	}
	if host["Mixed_Case"] != "changed" || host["missing"] != "new" {
		t.Fatal("snapshot altered the caller's environment")
	}
	vars, err := environment.Build(saved, environment.Spec{Extra: spec.Env})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vars {
		if v.Name == "MIXED_CASE" && v.Value != "last" {
			t.Fatal("snapshot changed last-setting-wins semantics")
		}
	}
	if !reflect.DeepEqual(spec.Env, []string{"MIXED_CASE", "missing", "FIXED=literal", "mixed_case=last"}) {
		t.Fatal("snapshot changed original launch arguments")
	}
}

func TestSupervisorRetainsFailedAutoRemoveStart(t *testing.T) {
	for _, attached := range []bool{false, true} {
		t.Run(strconv.FormatBool(attached), func(t *testing.T) {
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
			// The absent image fails startup before a payload can be launched.
			rec, err := ctx.Store.CreateContainer("failed-start", store.ContainerRecord{
				Image:      "sha256:" + strings.Repeat("a", 64),
				HostConfig: store.ContainerHostConfig{AutoRemove: true},
				State:      store.ContainerState{Status: "created", RunID: runID},
			})
			if err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(rec.DataDir, "retained.txt")
			if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Calling synchronously includes the supervisor's deferred cleanup.
			if code, err := Supervise(ctx, rec.ID, runID, attached); code != 1 || err == nil {
				t.Fatalf("failed startup: %d %v", code, err)
			}
			failed, err := ctx.Store.ResolveContainer(rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if failed.State.Launched || failed.State.Running || failed.State.Status != "created" || failed.State.Error == "" {
				t.Fatalf("failed start was not retained for retry: %+v", failed.State)
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
				t.Fatalf("failed start lost private data: %q %v", data, err)
			}
		})
	}
}

// This helper is launched as a separate supervisor-like process. Its child job
// must be ended by Windows when startup cancellation closes the owner's handles.
func TestSupervisorStartupChild(t *testing.T) {
	probe := os.Getenv("QUASIDOCK_STARTUP_PROBE")
	if probe == "" {
		t.Skip("subprocess helper")
	}
	exe := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	child, err := process.Start(process.SpawnSpec{
		Exe: exe, CommandLine: process.BuildCommandLine(exe, []string{"/d", "/c", "ping -n 60 127.0.0.1 >nul"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if err := os.WriteFile(probe, []byte(strconv.FormatUint(uint64(win32.GetProcessId(child.Process)), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = child.Wait()
}

func TestSupervisorStartupTimeoutCancelsProcessTree(t *testing.T) {
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
	rec, err := ctx.Store.CreateContainer("timeout", store.ContainerRecord{State: store.ContainerState{RunID: runID}})
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(t.TempDir(), "child-pid")
	t.Setenv("QUASIDOCK_STARTUP_PROBE", probe)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	det, err := process.StartDetached(exe, process.BuildCommandLine(exe, []string{"-test.run=^TestSupervisorStartupChild$"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(det.Process)
	defer win32.TerminateProcess(det.Process, 1)
	var child win32.HANDLE
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(probe)
		pid, parseErr := strconv.ParseUint(string(data), 10, 32)
		if parseErr == nil {
			child, err = win32.OpenProcess(win32.SYNCHRONIZE, false, win32.DWORD(pid))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("startup helper did not spawn child")
	}
	defer win32.CloseHandle(child)
	ready, err := win32.CreateEvent("", true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(ready)
	if err := awaitSupervisor(ctx, rec, det.Process, ready, 30); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("startup wait: %v", err)
	}
	if win32.WaitForSingleObject(det.Process, 0) != win32.WAIT_OBJECT_0 || win32.WaitForSingleObject(child, 5000) != win32.WAIT_OBJECT_0 {
		t.Fatal("startup cancellation left a process running")
	}
}
