package run

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestContainerStatsProtocolAndConcurrentClients(t *testing.T) {
	ctx, rec, live := pauseTestRuntime(t)
	if _, err := ContainerStats(stdcontext.Background(), ctx, rec); err == nil || !strings.Contains(err.Error(), "stop and start") {
		t.Fatalf("old supervisor: %v", err)
	}
	var err error
	rec, err = ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error { c.State.StatsProtocol = statsProtocol; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := live.start(rec.ID, rec.State.RunID); err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	// A paused/capturing runtime must still accept read-only samples.
	live.mu.Lock()
	live.paused, live.capturing = true, true
	live.mu.Unlock()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			sample, err := ContainerStats(stdcontext.Background(), ctx, rec)
			if err != nil || sample.RunID != rec.State.RunID || sample.SampledAt.IsZero() || sample.PIDs != 0 {
				t.Errorf("sample: %+v %v", sample, err)
			}
		})
	}
	wg.Wait()
	for _, request := range []statsRequest{{statsProtocol, "wrong"}, {99, rec.State.RunID}} {
		file, err := win32.DialPipe(execPipeName(rec.ID, rec.State.RunID), time.Now().Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		c := &execStream{File: file}
		_ = c.SetDeadline(time.Now().Add(time.Second))
		data, _ := json.Marshal(request)
		if err := c.send(statsQuery, data); err != nil {
			t.Fatal(err)
		}
		kind, _, err := c.receive()
		_ = c.Close()
		if err != nil || kind != execError {
			t.Fatalf("invalid request: %d %v", kind, err)
		}
	}
	cancel, stop := stdcontext.WithCancel(stdcontext.Background())
	stop()
	if _, err := ContainerStats(cancel, ctx, rec); !errors.Is(err, stdcontext.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	stopped := *rec
	stopped.State.Running = false
	if _, err := ContainerStats(stdcontext.Background(), ctx, &stopped); !errors.Is(err, ErrStatsRunChanged) {
		t.Fatalf("stopped: %v", err)
	}
}

func TestContainerStatsCancellationDuringResponse(t *testing.T) {
	ctx, rec, _ := pauseTestRuntime(t)
	rec.State.StatsProtocol = statsProtocol
	listener, err := win32.ListenPipe(execPipeName(rec.ID, rec.State.RunID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		file, err := listener.Accept()
		if err != nil {
			return
		}
		c := &execStream{File: file}
		defer func() { _ = c.Close() }()
		_, _, _ = c.receive()
		close(accepted)
		_, _, _ = c.receive() // Wait for cancellation to close the client.
	}()
	cancel, stop := stdcontext.WithCancel(stdcontext.Background())
	defer stop()
	result := make(chan error, 1)
	go func() { _, err := ContainerStats(cancel, ctx, rec); result <- err }()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("stats did not connect")
	}
	stop()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt response read")
	}
	<-done
}
