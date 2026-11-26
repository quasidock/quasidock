package cli

import (
	"bytes"
	stdcontext "context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
)

func TestWaitOptionsAndDispatch(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"--"}, {"--timeout=1", "worker"}, {"worker", "-x"}} {
		if _, err := parseWaitOptions(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if targets, err := parseWaitOptions([]string{"one", "--", "-two"}); err != nil || strings.Join(targets, ",") != "one,-two" {
		t.Fatalf("end of options: %v %v", targets, err)
	}
	ctx, _ := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	var out bytes.Buffer
	ctx.Stdout = &out
	created, err := ctx.Store.CreateContainer("wait-created", store.ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	backoff, err := ctx.Store.CreateContainer("wait-backoff", store.ContainerRecord{State: store.ContainerState{Status: "restarting", Restarting: true, ExitCode: 11}})
	if err != nil {
		t.Fatal(err)
	}
	lk, err := store.OpenLock(ctx.Store.ContainerRunLockPath(backoff.ID), true, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lk.Release)
	exited, err := ctx.Store.CreateContainer("wait-exited", store.ContainerRecord{State: store.ContainerState{Status: "exited", ExitCode: 37}})
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range [][]string{{"wait"}, {"container", "wait"}} {
		for _, flag := range []string{"-h", "--help"} {
			out.Reset()
			if code, err := Dispatch(ctx, append(append([]string{}, prefix...), flag)); code != 0 || err != nil || !strings.Contains(out.String(), waitUsage) {
				t.Fatalf("help: %d %v %s", code, err, &out)
			}
		}
		out.Reset()
		args := append(append([]string{}, prefix...), "/WAIT-EXITED", created.ID, exited.ID[:12], "wait-exited")
		if code, err := Dispatch(ctx, args); code != 0 || err != nil || out.String() != "37\n0\n37\n37\n" {
			t.Fatalf("wait: %d %v %q", code, err, out.String())
		}
		out.Reset()
		if code, err := Dispatch(ctx, append(append([]string{}, prefix...), "wait-backoff")); code != 0 || err != nil || out.String() != "11\n" {
			t.Fatalf("restart backoff: %d %v %q", code, err, out.String())
		}
		out.Reset()
		args = append(append([]string{}, prefix...), "missing", "wait-exited")
		if code, err := Dispatch(ctx, args); code != 1 || !errors.Is(err, store.ErrContainerNotFound) || !strings.Contains(err.Error(), "wait missing") || out.String() != "37\n" {
			t.Fatalf("partial failure: %d %v %q", code, err, out.String())
		}
	}
}

func TestWaitConcurrentObservationAndOrderedOutput(t *testing.T) {
	ctx := context.New()
	var out bytes.Buffer
	ctx.Stdout = &out
	first, second := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	wait := func(cancel stdcontext.Context, _ *context.Context, target string) (uint32, error) {
		if target == "second" {
			close(second)
			return 29, nil
		}
		select {
		case <-first:
			return 17, nil
		case <-cancel.Done():
			return 0, cancel.Err()
		}
	}
	go func() { done <- waitContainers(stdcontext.Background(), ctx, []string{"first", "second"}, wait) }()
	select {
	case <-second:
	case <-time.After(3 * time.Second):
		t.Fatal("second target was not observed while first was running")
	}
	close(first)
	if err := <-done; err != nil || out.String() != "17\n29\n" {
		t.Fatalf("ordered results: %q %v", out.String(), err)
	}
}

func TestWaitCancellationAndOutputFailure(t *testing.T) {
	for _, outputFailure := range []bool{false, true} {
		ctx := context.New()
		ctx.Stdout = io.Discard
		if outputFailure {
			ctx.Stdout = statsFailWriter{}
		}
		cancel, stop := stdcontext.WithCancel(stdcontext.Background())
		started, finished := make(chan struct{}), make(chan struct{})
		wait := func(cancel stdcontext.Context, _ *context.Context, target string) (uint32, error) {
			if target == "done" {
				<-started
				return 0, nil
			}
			close(started)
			<-cancel.Done()
			close(finished)
			return 0, cancel.Err()
		}
		done := make(chan error, 1)
		go func() { done <- waitContainers(cancel, ctx, []string{"done", "pending"}, wait) }()
		<-started
		if !outputFailure {
			stop()
		}
		select {
		case err := <-done:
			want := error(stdcontext.Canceled)
			if outputFailure {
				want = io.ErrClosedPipe
			}
			if !errors.Is(err, want) {
				t.Fatalf("output failure %t: %v", outputFailure, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("wait workers were not canceled")
		}
		stop()
		<-finished // waitContainers joins every worker before returning.
	}
}
