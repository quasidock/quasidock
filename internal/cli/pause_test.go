package cli

import (
	"bytes"
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

type pauseOutput struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *pauseOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *pauseOutput) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Len()
}

func awaitPauseCondition(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(description)
}

func TestContainerPauseUnpause(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	binary, payload, buildDir := buildContainerTestBinaries(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	fixtureImage(t, reg, "1", payload)
	app := reg.Host() + "/app:1"
	command := func(args ...string) *exec.Cmd {
		deadline, cancel := stdcontext.WithTimeout(stdcontext.Background(), 40*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(deadline, binary, args...)
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		return cmd
	}
	call := func(args ...string) (string, error) {
		out, err := command(args...).CombinedOutput()
		return string(out), err
	}
	must := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := call(args...)
		if err != nil {
			t.Fatalf("%v: %s %v", args, out, err)
		}
		return out
	}
	fails := func(t *testing.T, message string, args ...string) {
		t.Helper()
		out, err := call(args...)
		if err == nil || !strings.Contains(out, message) {
			t.Fatalf("%v: expected %q error, got %q %v", args, message, out, err)
		}
	}
	record := func(t *testing.T, target string) *store.ContainerRecord {
		t.Helper()
		rec, err := ctx.Store.ResolveContainer(target)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	ticks := func(path string) int64 {
		info, err := os.Stat(path + ".ticks")
		if err != nil {
			return 0
		}
		return info.Size()
	}
	ready := func(t *testing.T, path string) {
		t.Helper()
		awaitPauseCondition(t, "heartbeat did not start: "+path, func() bool { return ticks(path) > 0 })
	}
	startSession := func(t *testing.T, args ...string) (*exec.Cmd, <-chan error, *pauseOutput) {
		t.Helper()
		cmd := command(args...)
		out := &pauseOutput{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		return cmd, done, out
	}
	waitSession := func(t *testing.T, done <-chan error) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(7 * time.Second):
			t.Fatal("live session did not finish")
		}
	}
	t.Cleanup(func() {
		containers, _ := ctx.Store.ListContainers(true)
		for _, rec := range containers {
			if out, err := call("rm", "-f", rec.ID); err != nil {
				t.Errorf("cleanup: %s %v", out, err)
			}
		}
	})
	must(t, "pull", app)

	t.Run("created containers rejected", func(t *testing.T) {
		must(t, "create", "--name", "unstarted", app)
		for _, action := range []string{"pause", "unpause"} {
			fails(t, "not running", action, "unstarted")
		}
		if rec := record(t, "unstarted"); rec.State.Status != "created" || rec.State.Paused {
			t.Fatalf("pause changed an unstarted container: %+v", rec.State)
		}
		must(t, "rm", "unstarted")
	})

	t.Run("whole job and live sessions", func(t *testing.T) {
		mainPath := filepath.Join(buildDir, "main")
		execPath := filepath.Join(buildDir, "exec")
		detachedPath := filepath.Join(buildDir, "exec-detached")
		must(t, "run", "-d", "--name", "tree", app, "fixture.exe", "heartbeat-tree", mainPath, "ignore")
		before := record(t, "tree")
		_, attachDone, attached := startSession(t, "attach", "--no-stdin", "tree")
		_, execDone, executed := startSession(t, "exec", "tree", "fixture.exe", "heartbeat", execPath, "ignore")
		must(t, "exec", "-d", "tree", "fixture.exe", "heartbeat", detachedPath, "ignore")
		paths := []string{mainPath, mainPath + ".child", execPath, detachedPath}
		var handles []win32.HANDLE
		for _, path := range paths {
			ready(t, path)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.ParseUint(string(data), 10, 32)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := win32.OpenProcess(win32.SYNCHRONIZE, false, win32.DWORD(pid))
			if err != nil {
				t.Fatal(err)
			}
			defer win32.CloseHandle(handle)
			handles = append(handles, handle)
		}
		awaitPauseCondition(t, "sessions did not receive output", func() bool { return attached.Len() > 0 && executed.Len() > 0 })
		short := before.ShortID()
		if got := strings.TrimSpace(must(t, "pause", short)); got != short {
			t.Fatalf("pause output: %q", got)
		}
		time.Sleep(50 * time.Millisecond) // Allow already-issued I/O to finish.
		counts := make([]int64, len(paths))
		for i, path := range paths {
			counts[i] = ticks(path)
		}
		time.Sleep(200 * time.Millisecond)
		for i, path := range paths {
			if got := ticks(path); got != counts[i] {
				t.Fatalf("paused process advanced: %s %d -> %d", path, counts[i], got)
			}
		}
		if got := strings.TrimSpace(must(t, "inspect", "-f", "{{.State.Status}} {{.State.Running}} {{.State.Paused}}", "tree")); got != "paused true true" {
			t.Fatalf("inspect: %s", got)
		}
		if out := must(t, "ps"); !strings.Contains(out, "tree") || !strings.Contains(out, "(Paused)") {
			t.Fatalf("paused container missing from ps: %s", out)
		}
		if out := must(t, "logs", "--tail=3", "tree"); !strings.Contains(out, "tick") {
			t.Fatalf("logs unavailable while paused: %s", out)
		}
		fails(t, "already paused", "container", "pause", "tree")
		for _, args := range [][]string{
			{"exec", "tree", "fixture.exe", "log", "unexpected", ""},
			{"exec", "-d", "tree", "fixture.exe", "log", "unexpected", ""},
			{"attach", "tree"}, {"start", "-a", "tree"}, {"start", "-i", "tree"},
		} {
			fails(t, "paused", args...)
		}
		must(t, "start", "tree")
		must(t, "container", "prune")
		fails(t, "running container", "rm", "tree")
		fails(t, "in use by container", "rmi", app)
		paused := record(t, "tree")
		if !paused.State.Paused || paused.State.RunID != before.State.RunID || paused.State.Pid != before.State.Pid || paused.State.StartedAt != before.State.StartedAt {
			t.Fatalf("pause/start changed run identity: %+v", paused.State)
		}
		attachBytes, execBytes := attached.Len(), executed.Len()
		must(t, "container", "unpause", "tree")
		for i, path := range paths {
			awaitPauseCondition(t, "process did not resume: "+path, func() bool { return ticks(path) > counts[i] })
		}
		awaitPauseCondition(t, "existing streams did not resume", func() bool { return attached.Len() > attachBytes && executed.Len() > execBytes })
		fails(t, "not paused", "unpause", "tree")
		for _, action := range []string{"pause", "unpause"} {
			out, err := call(action, "missing", "tree")
			if err == nil || !strings.Contains(out, "tree\n") || record(t, "tree").State.Paused != (action == "pause") {
				t.Fatalf("batch did not continue: %s %v", out, err)
			}
		}
		must(t, "pause", "tree")
		must(t, "kill", "tree")
		waitSession(t, attachDone)
		waitSession(t, execDone)
		for _, handle := range handles {
			if win32.WaitForSingleObject(handle, 1000) != win32.WAIT_OBJECT_0 {
				t.Fatal("KILL left a frozen job process alive")
			}
		}
		if current := record(t, "tree"); current.State.Paused || current.State.Running || current.State.Status != "exited" {
			t.Fatalf("stale pause state after KILL: %+v", current.State)
		}
		for _, action := range []string{"pause", "unpause"} {
			fails(t, "not running", action, "tree")
			fails(t, "container not found", action, "missing")
		}
	})

	t.Run("shutdown and restart", func(t *testing.T) {
		for _, action := range []string{"stop", "TERM", "restart", "stop-zero", "remove", "auto-remove"} {
			t.Run(action, func(t *testing.T) {
				name := "shutdown-" + action
				path := filepath.Join(buildDir, name)
				args := []string{"run", "-d", "--name", name}
				if action == "auto-remove" {
					args = append(args, "--rm")
				}
				mode := "ignore"
				if action == "stop" || action == "TERM" {
					mode = "exit"
				}
				must(t, append(args, app, "fixture.exe", "heartbeat", path, mode)...)
				ready(t, path)
				before := record(t, name)
				must(t, "pause", name)
				switch action {
				case "stop":
					must(t, "stop", "-t", "5", name)
				case "TERM":
					must(t, "kill", "--signal=TERM", name)
				case "restart":
					began := time.Now()
					must(t, "restart", "-t", "1", name)
					if time.Since(began) < 900*time.Millisecond {
						t.Fatal("restart skipped the grace period")
					}
					after := record(t, name)
					if !after.State.Running || after.State.Paused || after.State.RunID == before.State.RunID || after.State.Pid == before.State.Pid {
						t.Fatalf("restart retained paused run: %+v", after.State)
					}
					must(t, "rm", "-f", name)
					return
				case "stop-zero":
					must(t, "stop", "-t", "0", name)
				case "remove":
					must(t, "rm", "-f", name)
				case "auto-remove":
					must(t, "kill", name)
				}
				awaitPauseCondition(t, "shutdown did not finish", func() bool { return !ctx.Store.IsContainerRunning(before.ID) })
				after, err := ctx.Store.ResolveContainer(name)
				if action == "remove" || action == "auto-remove" {
					if !errors.Is(err, store.ErrContainerNotFound) {
						t.Fatalf("removed record retained: %v", err)
					}
					return
				}
				if err != nil || after.State.Paused || after.State.Running {
					t.Fatalf("shutdown retained pause state: %+v %v", after, err)
				}
				if action == "stop" || action == "TERM" {
					if after.State.ExitCode != 0 {
						t.Fatalf("graceful exit escalated to KILL: %+v", after.State)
					}
					if _, err := os.Stat(path + ".signal"); err != nil {
						t.Fatalf("graceful shutdown did not deliver TERM: %v", err)
					}
				}
			})
		}
	})

	t.Run("foreground shutdown and supervisor death", func(t *testing.T) {
		for _, mode := range []string{"stop", "crash", "crash-auto-remove"} {
			name := "foreground-" + mode
			path := filepath.Join(buildDir, name)
			args := []string{"run", "--name", name}
			if mode == "crash-auto-remove" {
				args = append(args, "--rm")
			}
			owner, done, _ := startSession(t, append(args, app, "fixture.exe", "heartbeat", path, "ignore")...)
			ready(t, path)
			before := record(t, name)
			must(t, "pause", name)
			if mode == "stop" {
				fails(t, "foreground", "kill", "--signal=TERM", name)
				if !record(t, name).State.Paused {
					t.Fatal("rejected foreground TERM unpaused the container")
				}
				must(t, "stop", "-t", "1", name)
			} else if err := owner.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			waitSession(t, done)
			awaitPauseCondition(t, "dead owner retained run lock", func() bool { return !ctx.Store.IsContainerRunning(before.ID) })
			current, err := ctx.Store.ResolveContainer(name)
			if mode == "crash-auto-remove" {
				if !errors.Is(err, store.ErrContainerNotFound) {
					t.Fatalf("crashed --rm container retained: %v", err)
				}
			} else if err != nil || current.State.Paused || current.State.Status != "exited" {
				t.Fatalf("dead owner retained paused state: %+v %v", current, err)
			}
		}
	})

	t.Run("concurrent exec and restart", func(t *testing.T) {
		path := filepath.Join(buildDir, "concurrent")
		must(t, "run", "-d", "--name", "concurrent", app, "fixture.exe", "heartbeat", path, "ignore")
		ready(t, path)
		before := record(t, "concurrent")
		type result struct {
			out string
			err error
		}
		results := make(chan result, 2)
		for _, args := range [][]string{{"pause", "concurrent"}, {"exec", "concurrent", "fixture.exe", "log", "race", ""}} {
			go func() { out, err := call(args...); results <- result{out, err} }()
		}
		for range 2 {
			r := <-results
			if r.err != nil && !strings.Contains(r.out, "paused") {
				t.Fatalf("pause/exec race: %s %v", r.out, r.err)
			}
		}
		must(t, "unpause", "concurrent")
		for _, args := range [][]string{{"pause", "concurrent"}, {"restart", "-t=0", "concurrent"}} {
			go func() { out, err := call(args...); results <- result{out, err} }()
		}
		for range 2 {
			if r := <-results; r.err != nil {
				t.Fatalf("pause/restart race: %s %v", r.out, r.err)
			}
		}
		after := record(t, "concurrent")
		if after.State.RunID == before.State.RunID || !after.State.Running {
			t.Fatalf("restart race lost replacement: %+v", after.State)
		}
		if after.State.Paused {
			must(t, "unpause", "concurrent")
		}
		count := ticks(path)
		awaitPauseCondition(t, fmt.Sprintf("replacement did not run after concurrent controls: %+v", after.State), func() bool { return ticks(path) > count })
		must(t, "rm", "-f", "concurrent")
	})
}
