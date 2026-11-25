package cli

import (
	"bytes"
	stdcontext "context"
	encodingbinary "encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestContainerTopLifecycle(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	binary, payload, buildDir := buildContainerTestBinaries(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	fixtureImage(t, reg, "1", payload)
	app := reg.Host() + "/app:1"
	command := func(args ...string) *exec.Cmd {
		cancel, stop := stdcontext.WithTimeout(stdcontext.Background(), 30*time.Second)
		t.Cleanup(stop)
		cmd := exec.CommandContext(cancel, binary, args...)
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		return cmd
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := command(args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s %v", args, out, err)
		}
		return string(out)
	}
	fail := func(contains string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := command(args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), contains) {
			t.Fatalf("%v: stdout %q stderr %q error %v", args, &stdout, &stderr, err)
		}
	}
	t.Cleanup(func() {
		all, _ := ctx.Store.ListContainers(true)
		for _, rec := range all {
			out, err := command("rm", "-f", rec.ID).CombinedOutput()
			if err != nil {
				t.Errorf("cleanup %s: %s %v", rec.ID, out, err)
			}
		}
	})
	rows := func(args ...string) []topRow {
		t.Helper()
		text := must(args...)
		decoder := json.NewDecoder(strings.NewReader(text))
		var result []topRow
		previous := 0
		for {
			var row topRow
			if err := decoder.Decode(&row); err == io.EOF {
				return result
			} else if err != nil {
				t.Fatalf("decode top %s: %v", text, err)
			}
			pid, err := strconv.Atoi(row.PID)
			if err != nil || pid <= previous || row.PPID == "--" || row.Time == "--" || row.Command == "--" || row.Command == "" {
				t.Fatalf("invalid process row: %+v %v", row, err)
			}
			previous = pid
			result = append(result, row)
		}
	}
	awaitFile := func(path string) string {
		t.Helper()
		awaitPauseCondition(t, "fixture not ready: "+path, func() bool { data, err := os.ReadFile(path); return err == nil && len(data) != 0 })
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	must("pull", app)
	ready := filepath.Join(buildDir, "top-main")
	marker := "quoted marker 雪 " + strings.Repeat("long argument ", 200)
	must("run", "-d", "--name", "top-main", app, "fixture.exe", "tree-hold", ready, "ignore", marker)
	mainPID, childPID := awaitFile(ready), awaitFile(ready+".child")
	rec, err := ctx.Store.ResolveContainer("top-main")
	if err != nil || rec.State.TopProtocol != 1 {
		t.Fatalf("top capability: %+v %v", rec, err)
	}
	initial := rows("top", "top-main", "--format=json")
	if len(initial) != 2 {
		t.Fatalf("main and child: %+v", initial)
	}
	for _, row := range initial {
		if row.PID == mainPID && !strings.Contains(row.Command, marker) {
			t.Fatalf("truncated command: %s", row.Command)
		}
		if row.PID == childPID && row.PPID != mainPID {
			t.Fatalf("parent: %+v, want %s", row, mainPID)
		}
	}
	if text := must("top", "top-main"); !strings.Contains(text, "PPID") || !strings.Contains(text, marker) || strings.Contains(text, "\x1b") {
		t.Fatalf("table: %s", text)
	}
	if text := must("top", "--format", "{{.PID}}:{{.PPID}}", rec.ID); !strings.Contains(text, childPID+":"+mainPID) || strings.Contains(text, "COMMAND") {
		t.Fatalf("template: %s", text)
	}
	execReady := filepath.Join(buildDir, "top-exec")
	must("exec", "-d", "top-main", "fixture.exe", "tree-hold", execReady, "ignore")
	execPID, execChildPID := awaitFile(execReady), awaitFile(execReady+".child")
	withExec := rows("container", "top", rec.ID[:12], "--format", "json")
	if len(withExec) != 4 {
		t.Fatalf("nested exec processes: %+v", withExec)
	}
	found := make(map[string]bool)
	for _, row := range withExec {
		found[row.PID] = true
		if row.PID == execChildPID && row.PPID != execPID {
			t.Fatalf("exec child parent: %+v", row)
		}
	}
	for _, pid := range []string{mainPID, childPID, execPID, execChildPID} {
		if !found[pid] {
			t.Fatalf("missing PID %s: %+v", pid, withExec)
		}
	}
	must("pause", "top-main")
	paused := rows("top", "--format=json", "top-main")
	if len(paused) != 4 {
		t.Fatalf("paused processes: %+v", paused)
	}
	stillPaused, err := ctx.Store.ResolveContainer("top-main")
	if err != nil || !stillPaused.State.Paused {
		t.Fatalf("top changed pause state: %+v %v", stillPaused, err)
	}
	must("unpause", "top-main")
	if _, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error { c.State.TopProtocol = 0; return nil }); err != nil {
		t.Fatal(err)
	}
	fail("stop and start", "top", "top-main")
	for _, path := range []string{ready, ready + ".child"} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	must("restart", "-t", "0", "top-main")
	awaitFile(ready)
	awaitFile(ready + ".child")
	if restarted := rows("top", "top-main", "--format=json"); len(restarted) != 2 {
		t.Fatalf("restarted processes: %+v", restarted)
	}
	if _, err := run.ContainerTop(stdcontext.Background(), ctx, rec); !errors.Is(err, run.ErrTopRunChanged) {
		t.Fatalf("old run returned a snapshot: %v", err)
	}
	must("stop", "-t", "0", "top-main")
	fail("not running", "top", "top-main")
	must("create", "--name", "top-created", app)
	fail("not running", "container", "top", "top-created")
	fail("container not found", "top", "missing")
	fail("Linux ps arguments", "top", "top-main", "-ef")

	foregroundReady := filepath.Join(buildDir, "top-foreground")
	foreground := command("run", "--name", "top-foreground", app, "fixture.exe", "hold", foregroundReady)
	var output bytes.Buffer
	foreground.Stdout, foreground.Stderr = &output, &output
	if err := foreground.Start(); err != nil {
		t.Fatal(err)
	}
	foregroundDone := make(chan error, 1)
	go func() { foregroundDone <- foreground.Wait() }()
	awaitFile(foregroundReady)
	if snapshot := rows("top", "top-foreground", "--format=json"); len(snapshot) != 1 {
		t.Fatalf("foreground: %+v", snapshot)
	}
	must("rm", "-f", "top-foreground")
	select {
	case <-foregroundDone:
	case <-time.After(5 * time.Second):
		t.Fatal("foreground did not finish")
	}
	for _, sandbox := range []string{"low", "appcontainer"} {
		t.Run(sandbox, func(t *testing.T) {
			name := "top-" + sandbox
			must("run", "-d", "--name", name, "--sandbox="+sandbox, app, "fixture.exe", "tree-hold", "$HOME/top-ready", "ignore")
			awaitPauseCondition(t, "sandbox child not started", func() bool { return len(rows("top", name, "--format=json")) == 2 })
			must("pause", name)
			if snapshot := rows("top", name, "--format=json"); len(snapshot) != 2 {
				t.Fatalf("paused sandbox: %+v", snapshot)
			}
			must("rm", "-f", name)
		})
	}

	t.Run("interrupt", func(t *testing.T) {
		// A stalled supervisor makes cancellation deterministic: top must leave
		// without output or a signal to the container it is observing.
		rec, err := ctx.Store.CreateContainer("top-stalled", store.ContainerRecord{State: store.ContainerState{
			Status: "running", Running: true, TopProtocol: 1, RunID: strings.Repeat("b", 32),
		}})
		if err != nil {
			t.Fatal(err)
		}
		lock, err := store.OpenLock(ctx.Store.ContainerRunLockPath(rec.ID), true, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			lock.Release()
			_, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
				c.State.Running, c.State.Status = false, "exited"
				return nil
			})
			if err != nil {
				t.Error(err)
			}
			if err := ctx.Store.RemoveContainer(rec.ID); err != nil {
				t.Error(err)
			}
		})
		listener, err := win32.ListenPipe(`\\.\pipe\quasidock-exec-` + rec.ID + "-" + rec.State.RunID)
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
			defer func() { _ = file.Close() }()
			_ = file.SetDeadline(time.Now().Add(5 * time.Second))
			var header [5]byte
			if _, err := io.ReadFull(file, header[:]); err != nil {
				return
			}
			if _, err := io.CopyN(io.Discard, file, int64(encodingbinary.LittleEndian.Uint32(header[1:]))); err != nil {
				return
			}
			close(accepted)
			_, _ = file.Read(header[:]) // Cancellation must close the pipe.
		}()
		t.Cleanup(func() { _ = listener.Close(); <-done })
		t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
		console, input := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), []string{"console-run-check-mode", binary, "top", rec.ID})
		select {
		case <-accepted:
		case <-time.After(4 * time.Second):
			t.Fatal("top did not connect")
		}
		if _, err := win32.WriteFile(input, []byte{3}); err != nil {
			t.Fatal(err)
		}
		if win32.WaitForSingleObject(console, 5000) != win32.WAIT_OBJECT_0 {
			t.Fatal("top did not exit on Ctrl+C")
		}
		if code, err := win32.GetExitCodeProcess(console); err != nil || code != 130 {
			t.Fatalf("Ctrl+C: %d %v", code, err)
		}
		if !ctx.Store.IsContainerRunning(rec.ID) {
			t.Fatal("interrupt changed workload")
		}
	})
}
