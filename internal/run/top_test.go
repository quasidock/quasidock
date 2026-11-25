package run

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func topTestRuntime(t *testing.T) (*context.Context, *store.ContainerRecord, *execRuntime) {
	t.Helper()
	ctx, rec, live := pauseTestRuntime(t)
	var err error
	rec, err = ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error { c.State.TopProtocol = topProtocol; return nil })
	if err != nil {
		t.Fatal(err)
	}
	return ctx, rec, live
}

func TestContainerTopConcurrentAndCapability(t *testing.T) {
	ctx, rec, live := topTestRuntime(t)
	older := *rec
	older.State.TopProtocol = 0
	if _, err := ContainerTop(stdcontext.Background(), ctx, &older); err == nil || !strings.Contains(err.Error(), "stop and start") {
		t.Fatalf("old supervisor: %v", err)
	}
	stopped := *rec
	stopped.State.Running = false
	if _, err := ContainerTop(stdcontext.Background(), ctx, &stopped); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("stopped: %v", err)
	}
	if err := live.start(rec.ID, rec.State.RunID); err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	live.mu.Lock()
	live.paused, live.capturing = true, true
	live.mu.Unlock()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			snapshot, err := ContainerTop(stdcontext.Background(), ctx, rec)
			if err != nil || snapshot.RunID != rec.State.RunID || len(snapshot.Processes) != 0 {
				t.Errorf("snapshot: %+v %v", snapshot, err)
			}
		})
	}
	wg.Wait()
	for _, request := range []topRequest{{topProtocol, "wrong"}, {99, rec.State.RunID}} {
		file, err := win32.DialPipe(execPipeName(rec.ID, rec.State.RunID), time.Now().Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		c := &execStream{File: file}
		_ = c.SetDeadline(time.Now().Add(time.Second))
		data, _ := json.Marshal(request)
		if err := c.send(topQuery, data); err != nil {
			t.Fatal(err)
		}
		kind, _, err := c.receive()
		_ = c.Close()
		if err != nil || kind != execError {
			t.Fatalf("invalid request: %d %v", kind, err)
		}
	}
}

// topTestServer supplies controlled responses to the real client and joins its
// worker on cleanup so incomplete/cancelled requests cannot leak pipe handles.
func topTestServer(t *testing.T, rec *store.ContainerRecord, serve func(*execStream)) <-chan struct{} {
	t.Helper()
	listener, err := win32.ListenPipe(execPipeName(rec.ID, rec.State.RunID))
	if err != nil {
		t.Fatal(err)
	}
	accepted, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		file, err := listener.Accept()
		if err != nil {
			return
		}
		c := &execStream{File: file}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		kind, data, err := c.receive()
		var request topRequest
		if err != nil || kind != topQuery || json.Unmarshal(data, &request) != nil || request.Protocol != topProtocol || request.RunID != rec.State.RunID {
			t.Errorf("request: %d %s %v", kind, data, err)
			return
		}
		close(accepted)
		serve(c)
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	return accepted
}

func sendTopTestMessage(t *testing.T, c *execStream, kind byte, message topMessage) {
	t.Helper()
	data, err := json.Marshal(message)
	if err == nil {
		err = c.send(kind, data)
	}
	if err != nil {
		t.Errorf("send response: %v", err)
	}
}

func TestContainerTopLargeSnapshot(t *testing.T) {
	ctx, rec, _ := topTestRuntime(t)
	command := strings.Repeat("雪", 16000)
	topTestServer(t, rec, func(c *execStream) {
		for i := uint32(1); i <= 32; i++ {
			sendTopTestMessage(t, c, topProcess, topMessage{rec.State.RunID, &win32.ProcessInfo{PID: i, Command: command}})
		}
		sendTopTestMessage(t, c, topComplete, topMessage{RunID: rec.State.RunID})
	})
	snapshot, err := ContainerTop(stdcontext.Background(), ctx, rec)
	if err != nil || len(snapshot.Processes) != 32 || snapshot.Processes[31].Command != command || len(command)*len(snapshot.Processes) <= maxStreamFrame {
		t.Fatalf("large snapshot: %d rows %v", len(snapshot.Processes), err)
	}
}

func TestContainerTopRejectsIncompleteAndInvalidResponses(t *testing.T) {
	for _, mode := range []string{"incomplete", "wrong-kind", "bad-json", "wrong-run", "missing-process", "zero-pid", "duplicate-pid", "bad-completion", "server-error", "run-changed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, rec, _ := topTestRuntime(t)
			topTestServer(t, rec, func(c *execStream) {
				message := topMessage{rec.State.RunID, &win32.ProcessInfo{PID: 1, Command: "fixture.exe"}}
				sendTopTestMessage(t, c, topProcess, message)
				switch mode {
				case "incomplete":
					return
				case "wrong-kind":
					_ = c.send(statsResult, []byte(`{}`))
				case "bad-json":
					_ = c.send(topComplete, []byte(`{`))
				case "wrong-run":
					sendTopTestMessage(t, c, topComplete, topMessage{RunID: "replacement"})
				case "missing-process":
					sendTopTestMessage(t, c, topProcess, topMessage{RunID: rec.State.RunID})
				case "zero-pid":
					sendTopTestMessage(t, c, topProcess, topMessage{rec.State.RunID, &win32.ProcessInfo{}})
				case "duplicate-pid":
					sendTopTestMessage(t, c, topProcess, message)
				case "bad-completion":
					sendTopTestMessage(t, c, topComplete, message)
				case "server-error":
					_ = c.send(execError, []byte("enumeration failed"))
				case "run-changed":
					_ = c.send(execError, []byte(ErrTopRunChanged.Error()))
				}
			})
			snapshot, err := ContainerTop(stdcontext.Background(), ctx, rec)
			if err == nil || snapshot.RunID != "" || len(snapshot.Processes) != 0 {
				t.Fatalf("partial response escaped: %+v %v", snapshot, err)
			}
			if (mode == "wrong-run" || mode == "run-changed") && !errors.Is(err, ErrTopRunChanged) {
				t.Fatalf("run change: %v", err)
			}
		})
	}
}

func TestContainerTopRevalidatesRun(t *testing.T) {
	for _, mode := range []string{"exited", "restarted", "removed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, rec, _ := topTestRuntime(t)
			topTestServer(t, rec, func(c *execStream) {
				_, err := ctx.Store.UpdateContainer(rec.ID, func(current *store.ContainerRecord) error {
					if mode == "restarted" {
						current.State.RunID = strings.Repeat("a", 32)
					} else {
						current.State.Running = false
						current.State.Status = "exited"
					}
					return nil
				})
				if err != nil {
					t.Error(err)
				}
				if mode == "removed" {
					// Removal is represented by a missing state file while its
					// old supervisor connection remains open.
					if err := os.Remove(ctx.Store.ContainerStatePath(rec.ID)); err != nil {
						t.Error(err)
					}
				}
				sendTopTestMessage(t, c, topComplete, topMessage{RunID: rec.State.RunID})
			})
			if snapshot, err := ContainerTop(stdcontext.Background(), ctx, rec); !errors.Is(err, ErrTopRunChanged) || len(snapshot.Processes) != 0 {
				t.Fatalf("stale snapshot: %+v %v", snapshot, err)
			}
		})
	}
}

func TestContainerTopDetectsReplacementBeforeConnect(t *testing.T) {
	ctx, rec, _ := topTestRuntime(t)
	if _, err := ctx.Store.UpdateContainer(rec.ID, func(current *store.ContainerRecord) error {
		current.State.RunID = strings.Repeat("b", 32)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cancel, stop := stdcontext.WithTimeout(stdcontext.Background(), time.Second)
	defer stop()
	if _, err := ContainerTop(cancel, ctx, rec); !errors.Is(err, ErrTopRunChanged) {
		t.Fatalf("replacement before connection: %v", err)
	}
}

func TestContainerTopCancellation(t *testing.T) {
	for _, mode := range []string{"cancelled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, rec, _ := topTestRuntime(t)
			accepted := topTestServer(t, rec, func(c *execStream) { _, _, _ = c.receive() })
			cancel, stop := stdcontext.WithTimeout(stdcontext.Background(), 150*time.Millisecond)
			defer stop()
			result := make(chan error, 1)
			go func() { _, err := ContainerTop(cancel, ctx, rec); result <- err }()
			select {
			case <-accepted:
			case <-time.After(3 * time.Second):
				t.Fatal("top did not connect")
			}
			want := stdcontext.DeadlineExceeded
			if mode == "cancelled" {
				stop()
				want = stdcontext.Canceled
			}
			select {
			case err := <-result:
				if !errors.Is(err, want) {
					t.Fatalf("cancellation: %v; want %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not close response read")
			}
		})
	}
}
