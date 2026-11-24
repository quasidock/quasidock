package run

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestCaptureLeaseRestoresState(t *testing.T) {
	for _, paused := range []bool{false, true} {
		for _, disconnect := range []bool{false, true} {
			t.Run(map[bool]string{false: "running", true: "paused"}[paused]+map[bool]string{false: "/release", true: "/disconnect"}[disconnect], func(t *testing.T) {
				ctx, rec, r := pauseTestRuntime(t)
				rec.State.CommitProtocol = commitProtocol
				r.commitContext, r.commitRecord = ctx, rec
				if paused {
					if err := r.control(ctx, rec, controlRequest{Action: actionPause}, true); err != nil {
						t.Fatal(err)
					}
				}
				if err := r.start(rec.ID, rec.State.RunID); err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				lease, err := BeginCapture(ctx, rec)
				if err != nil {
					t.Fatal(err)
				}
				if frozen, err := win32.IsJobFrozen(r.job); err != nil || !frozen {
					t.Fatalf("not frozen: %t %v", frozen, err)
				}
				if err := r.control(ctx, rec, controlRequest{Action: actionResume}, true); err == nil {
					t.Fatal("control changed capture pause")
				}
				if _, err := BeginCapture(ctx, rec); err == nil {
					t.Fatal("accepted concurrent capture")
				}
				if disconnect {
					lease.stream.Close()
				} else if err := lease.Close(); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(3 * time.Second)
				for {
					r.mu.Lock()
					capturing := r.capturing
					r.mu.Unlock()
					if !capturing {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("capture was not released on disconnect")
					}
					time.Sleep(10 * time.Millisecond)
				}
				current, err := ctx.Store.ReadContainer(rec.ID)
				if err != nil || current.State.Paused != paused || !current.State.Running || current.State.RunID != rec.State.RunID || current.State.StartedAt != rec.State.StartedAt {
					t.Fatalf("state: %+v %v", current, err)
				}
				if frozen, err := win32.IsJobFrozen(r.job); err != nil || frozen != paused {
					t.Fatalf("freeze was not restored: %t %v", frozen, err)
				}
			})
		}
	}
}

func TestCaptureLeaseClient(t *testing.T) {
	if os.Getenv("QUASIDOCK_CAPTURE_TEST_CHILD") == "" {
		t.Skip("subprocess helper")
	}
	ctx := context.New()
	st, err := store.Open(os.Getenv("QUASIDOCK_CAPTURE_TEST_STORE"), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx.Store = st
	rec, err := st.ReadContainer(os.Getenv("QUASIDOCK_CAPTURE_TEST_ID"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := BeginCapture(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := os.WriteFile(os.Getenv("QUASIDOCK_CAPTURE_TEST_READY"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestCaptureClientTermination(t *testing.T) {
	ctx, rec, r := pauseTestRuntime(t)
	rec, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error { c.State.CommitProtocol = commitProtocol; return nil })
	if err != nil {
		t.Fatal(err)
	}
	r.commitContext, r.commitRecord = ctx, rec
	if err := r.start(rec.ID, rec.State.RunID); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(exe, "-test.run=^TestCaptureLeaseClient$")
	cmd.Env = append(os.Environ(), "QUASIDOCK_CAPTURE_TEST_CHILD=1", "QUASIDOCK_CAPTURE_TEST_STORE="+ctx.Store.Root, "QUASIDOCK_CAPTURE_TEST_ID="+rec.ID, "QUASIDOCK_CAPTURE_TEST_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("capture client did not acquire freeze")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if frozen, err := win32.IsJobFrozen(r.job); err != nil || !frozen {
		t.Fatalf("client did not freeze job: %t %v", frozen, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	for {
		current, err := ctx.Store.ReadContainer(rec.ID)
		if err == nil && !current.State.Paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminated client left source paused: %+v %v", current, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCaptureCapabilityAndRunID(t *testing.T) {
	ctx, rec, r := pauseTestRuntime(t)
	if _, err := BeginCapture(ctx, rec); err == nil {
		t.Fatal("accepted old owner")
	}
	rec.State.CommitProtocol = commitProtocol
	r.commitContext, r.commitRecord = ctx, rec
	if err := r.start(rec.ID, rec.State.RunID); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Dial the actual owner and send a stale run identifier.
	f, err := win32.DialPipe(execPipeName(rec.ID, rec.State.RunID), time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	c := &execStream{File: f}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	c.send(commitAcquire, []byte(`{"Protocol":1,"RunID":"wrong"}`))
	if kind, _, err := c.receive(); err != nil || kind != execError {
		t.Fatalf("stale request: %d %v", kind, err)
	}
	current, _ := ctx.Store.ReadContainer(rec.ID)
	if current.State.Paused {
		t.Fatal("stale capture paused the container")
	}
}
