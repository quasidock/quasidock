package run

import (
	stdcontext "context"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

type waitTestResult struct {
	code uint32
	err  error
}

func waitTestContainer(t *testing.T) (*context.Context, *store.ContainerRecord, *attachRuntime, *store.Lock) {
	t.Helper()
	ctx := context.New()
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx.Store = st
	runID, _ := newRunID()
	rec, err := st.CreateContainer("wait-worker", store.ContainerRecord{State: store.ContainerState{
		RunID: runID, Status: "running", Running: true, Launched: true, AttachProtocol: attachProtocol, WaitProtocol: waitProtocol,
	}})
	if err != nil {
		t.Fatal(err)
	}
	lk, err := store.OpenLock(st.ContainerRunLockPath(rec.ID), true, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lk.Release)
	r, err := newAttachRuntime(rec.ID, runID, true, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.finish(0, nil) })
	return ctx, rec, r, lk
}

func awaitWaitCondition(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("wait condition did not become true")
}

func startTestWait(t *testing.T, ctx *context.Context, rec *store.ContainerRecord, r *attachRuntime) (<-chan waitTestResult, stdcontext.CancelFunc) {
	t.Helper()
	cancel, stop := stdcontext.WithCancel(stdcontext.Background())
	t.Cleanup(stop)
	done := make(chan waitTestResult, 1)
	go func() { code, err := WaitContainer(cancel, ctx, rec.ID); done <- waitTestResult{code, err} }()
	awaitWaitCondition(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.sessions) == 1
	})
	return done, stop
}

func testWaitResult(t *testing.T, done <-chan waitTestResult, code uint32) {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil || result.code != code {
			t.Fatalf("wait result: %+v, want %d", result, code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not finish")
	}
}

func TestWaitContainerStatusOnlyAndReplacement(t *testing.T) {
	ctx, rec, r, _ := waitTestContainer(t)
	r.admit = func() error { return errors.New("container is paused; unpause it first") }
	if _, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error { c.State.Paused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	done, _ := startTestWait(t, ctx, rec, r)
	select {
	case <-r.gate:
		t.Fatal("wait opened the attached-start gate")
	default:
	}
	r.markStarted(func() error { t.Error("wait sent a signal"); return nil })
	for range 80 {
		_, _ = (attachWriter{r, attachStdout}).Write(make([]byte, 32*1024))
	}
	r.mu.Lock()
	for session := range r.sessions {
		if !session.waitOnly || session.stdin || session.bytes.Load() != 0 || r.writer != nil {
			t.Error("wait subscribed to streams or stdin")
		}
	}
	r.mu.Unlock()
	// Replacement state can already exist by the time the client reads its exit
	// frame. That frame must retain the first attempt's code, including errors.
	r.finish(37, errors.New("runtime cleanup failed"))
	if _, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
		c.State.RunID = strings.Repeat("b", 32)
		c.State.ExitCode = 99
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	testWaitResult(t, done, 37)
}

func TestWaitContainerRemovalAndEnrollmentExitRace(t *testing.T) {
	t.Run("removal after subscription", func(t *testing.T) {
		ctx, rec, r, lk := waitTestContainer(t)
		done, _ := startTestWait(t, ctx, rec, r)
		if _, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
			c.State.Running, c.State.Status, c.State.ExitCode = false, "exited", 19
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		r.finish(19, nil)
		lk.Release()
		if err := ctx.Store.RemoveContainer(rec.ID); err != nil {
			t.Fatal(err)
		}
		testWaitResult(t, done, 19)
	})
	t.Run("listener closes before terminal write", func(t *testing.T) {
		ctx, rec, r, _ := waitTestContainer(t)
		r.stopAccepting()
		done := make(chan waitTestResult, 1)
		cancel, stop := stdcontext.WithTimeout(stdcontext.Background(), 3*time.Second)
		defer stop()
		go func() { code, err := WaitContainer(cancel, ctx, rec.ID); done <- waitTestResult{code, err} }()
		awaitWaitCondition(t, func() bool { return store.IsLocked(ctx.Store.ContainerOperationLockPath(rec.ID)) })
		if _, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
			c.State.Running, c.State.Status, c.State.ExitCode = false, "exited", 23
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		testWaitResult(t, done, 23)
		if store.IsLocked(ctx.Store.ContainerOperationLockPath(rec.ID)) {
			t.Fatal("enrollment retained the operation lock")
		}
	})
}

func TestWaitContainerCancellation(t *testing.T) {
	for _, stage := range []string{"index", "operation", "stream"} {
		t.Run(stage, func(t *testing.T) {
			ctx, rec, r, _ := waitTestContainer(t)
			if stage == "stream" {
				done, stop := startTestWait(t, ctx, rec, r)
				stop()
				result := <-done
				if !errors.Is(result.err, stdcontext.Canceled) {
					t.Fatalf("stream cancellation: %v", result.err)
				}
				awaitWaitCondition(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.sessions) == 0 })
				return
			}
			path := ctx.Store.ContainerOperationLockPath(rec.ID)
			if stage == "index" {
				path = ctx.Store.Path("containers", "index.lock")
			}
			lk, err := store.OpenLock(path, true, false)
			if err != nil {
				t.Fatal(err)
			}
			defer lk.Release()
			cancel, stop := stdcontext.WithTimeout(stdcontext.Background(), 30*time.Millisecond)
			defer stop()
			if _, err := WaitContainer(cancel, ctx, rec.ID); !errors.Is(err, stdcontext.DeadlineExceeded) {
				t.Fatalf("%s cancellation: %v", stage, err)
			}
		})
	}
}

func TestWaitContainerUnsupportedSupervisorAndBrokenStream(t *testing.T) {
	t.Run("unsupported supervisor", func(t *testing.T) {
		ctx, rec, _, _ := waitTestContainer(t)
		_, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error { c.State.WaitProtocol = 0; return nil })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := WaitContainer(stdcontext.Background(), ctx, rec.ID); err == nil || !strings.Contains(err.Error(), "stop and start") {
			t.Fatalf("old supervisor: %v", err)
		}
	})
	t.Run("lost live stream", func(t *testing.T) {
		ctx, rec, r, _ := waitTestContainer(t)
		done, _ := startTestWait(t, ctx, rec, r)
		r.mu.Lock()
		var session *attachSession
		for s := range r.sessions {
			session = s
		}
		r.mu.Unlock()
		session.close()
		select {
		case result := <-done:
			if result.err == nil {
				t.Fatal("lost stream fabricated an exit code")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("lost stream hung")
		}
	})
	for _, replacement := range []bool{false, true} {
		name := "recover completed status"
		if replacement {
			name = "reject replacement status"
		}
		t.Run(name, func(t *testing.T) {
			ctx, rec, r, _ := waitTestContainer(t)
			done, _ := startTestWait(t, ctx, rec, r)
			if _, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
				c.State.Running, c.State.Status, c.State.ExitCode = false, "exited", 42
				if replacement {
					c.State.RunID = strings.Repeat("b", 32)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r.mu.Lock()
			var session *attachSession
			for s := range r.sessions {
				session = s
			}
			r.mu.Unlock()
			session.close()
			select {
			case result := <-done:
				if replacement && result.err == nil || !replacement && (result.err != nil || result.code != 42) {
					t.Fatalf("replacement %t: %+v", replacement, result)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("closed stream did not finish")
			}
		})
	}
}

func TestWaitContainerHandshakeCancellation(t *testing.T) {
	ctx, rec, r, _ := waitTestContainer(t)
	r.stopAccepting()
	listener, err := win32.ListenPipe(attachPipeName(rec.ID, rec.State.RunID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted, closed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(closed)
		file, err := listener.Accept()
		if err != nil {
			return
		}
		c := &framedStream{File: file}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, _, err := c.receive(); err != nil {
			return
		}
		close(accepted)
		_, _, _ = c.receive() // Cancellation must close the unacknowledged session.
	}()
	cancel, stop := stdcontext.WithCancel(stdcontext.Background())
	defer stop()
	done := make(chan waitTestResult, 1)
	go func() { code, err := WaitContainer(cancel, ctx, rec.ID); done <- waitTestResult{code, err} }()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not send its handshake")
	}
	stop()
	select {
	case result := <-done:
		if !errors.Is(result.err, stdcontext.Canceled) {
			t.Fatalf("handshake cancellation: %v", result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not interrupt the handshake")
	}
	<-closed
	if store.IsLocked(ctx.Store.ContainerOperationLockPath(rec.ID)) {
		t.Fatal("canceled handshake retained its operation lock")
	}
}

func TestReceiveWaitValidatesStatus(t *testing.T) {
	var maximum [4]byte
	binary.LittleEndian.PutUint32(maximum[:], ^uint32(0))
	for _, tc := range []struct {
		kind  byte
		data  []byte
		valid bool
	}{
		{attachExit, maximum[:], true},
		{attachExit, []byte{1}, false},
		{attachStdout, []byte("unexpected output"), false},
		{attachError, []byte("supervisor failed"), false},
	} {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		peer := &framedStream{File: writer}
		go func() { _ = peer.send(tc.kind, tc.data); _ = peer.Close() }()
		code, err := receiveWait(&framedStream{File: reader})
		_ = reader.Close()
		if tc.valid && (err != nil || code != ^uint32(0)) || !tc.valid && err == nil {
			t.Fatalf("frame %d/%d: %d %v", tc.kind, len(tc.data), code, err)
		}
	}
}
