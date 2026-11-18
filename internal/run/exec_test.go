package run

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestExecEnvironmentAndCommand(t *testing.T) {
	changes, err := execEnvironment(map[string]string{"Mixed": "host"}, []string{"mixed", "EMPTY=", "GONE", "VALUE=first", "value=last"})
	if err != nil || *changes["mixed"] != "host" || *changes["empty"] != "" || changes["gone"] != nil || *changes["value"] != "last" {
		t.Fatalf("%v %v", changes, err)
	}
	for _, entries := range [][]string{{""}, {"=x"}, {"A=\x00"}} {
		if _, err := execEnvironment(nil, entries); err == nil {
			t.Fatalf("accepted %q", entries)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "custom")
	os.Mkdir(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "custom.exe"), []byte("fixture"), 0o644)
	r := &execRuntime{runID: "attempt", cwd: dir, placeholders: oci.Placeholders{App: dir, Data: dir}, env: []environment.Var{{Name: "BASE", Value: "original"}}, systemRoot: os.Getenv("SystemRoot")}
	req := execRequest{Protocol: execProtocol, RunID: r.runID, Args: []string{"custom", "literal ${data}"}, Workdir: "${data}"}
	value := bin
	req.Env = map[string]*string{"PATH": &value}
	spec, err := r.command(req)
	if err != nil || spec.Exe != filepath.Join(bin, "custom.exe") || spec.Cwd != dir || !strings.Contains(spec.CommandLine, "literal ${data}") {
		t.Fatalf("%+v %v", spec, err)
	}
	if len(r.env) != 1 || r.env[0].Value != "original" {
		t.Fatal("mutated owner's environment")
	}
	req.RunID = "stale"
	if _, err := r.command(req); err == nil {
		t.Fatal("accepted stale run")
	}
	req.RunID = r.runID
	for _, cwd := range []string{"missing", "C:relative", `\ambiguous`} {
		req.Workdir = cwd
		if _, err := r.command(req); err == nil {
			t.Fatalf("accepted workdir %s", cwd)
		}
	}
}

func TestExecFrameAcknowledgement(t *testing.T) {
	runID, err := newRunID()
	if err != nil {
		t.Fatal(err)
	}
	name := execPipeName(strings.Repeat("b", 64), runID)
	l, err := win32.ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// A detached client disconnects as soon as it has the complete frame.
	// Repeat to exercise disconnects racing the sender's write completion.
	const attempts = 4096
	sent := make(chan error, attempts)
	go func() {
		for range attempts {
			f, err := l.Accept()
			if err != nil {
				sent <- err
				return
			}
			c := &execStream{File: f}
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			err = c.send(execStarted, nil)
			c.Close()
			sent <- err
		}
	}()
	for i := range attempts {
		f, err := win32.DialPipe(name, time.Now().Add(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		c := &execStream{File: f}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		kind, data, err := c.receive()
		c.Close()
		if err != nil || kind != execStarted || len(data) != 0 {
			t.Fatalf("receive %d: %d %q %v", i, kind, data, err)
		}
		if err := <-sent; err != nil {
			t.Fatalf("acknowledgement %d failed after delivery: %v", i, err)
		}
	}
}

func TestExecChildHelper(t *testing.T) {
	mode := os.Getenv("QUASIDOCK_EXEC_TEST_CHILD")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	if path := os.Getenv("QUASIDOCK_EXEC_TEST_READY"); path != "" {
		if err := os.WriteFile(path, []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
	}
	switch mode {
	case "spam":
		data := bytes.Repeat([]byte("x"), 64*1024)
		for {
			if _, err := os.Stdout.Write(data); err != nil {
				os.Exit(3)
			}
		}
	case "hold":
		for {
			time.Sleep(time.Second)
		}
	case "echo":
		_, _ = io.Copy(io.MultiWriter(os.Stdout, os.Stderr), os.Stdin)
	}
	os.Exit(0)
}

func TestExecProtocolAndShutdown(t *testing.T) {
	job, err := process.NewJob()
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(job)
	runID, err := newRunID()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	dir := t.TempDir()
	r := &execRuntime{primary: win32.GetCurrentProcess(), job: job, cwd: dir, systemRoot: os.Getenv("SystemRoot"), placeholders: oci.Placeholders{App: dir, Data: dir}}
	if err := r.start(id, runID); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dial := func() *execStream {
		t.Helper()
		f, err := win32.DialPipe(execPipeName(id, runID), time.Now().Add(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		f.SetDeadline(time.Now().Add(5 * time.Second))
		c := &execStream{File: f}
		t.Cleanup(func() { c.Close() })
		return c
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launch := func(c *execStream, mode string) {
		t.Helper()
		ready := filepath.Join(dir, mode+"-ready")
		req := execRequest{Protocol: execProtocol, RunID: runID, Args: []string{exe, "-test.run=^TestExecChildHelper$"}, Env: map[string]*string{"QUASIDOCK_EXEC_TEST_CHILD": &mode, "QUASIDOCK_EXEC_TEST_READY": &ready}, Interactive: true}
		data, _ := json.Marshal(req)
		if err := c.send(execLaunch, data); err != nil {
			t.Fatal(err)
		}
		kind, data, err := c.receive()
		if err != nil || kind != execStarted {
			t.Fatalf("launch: %d %s %v", kind, data, err)
		}
	}
	t.Run("malformed and oversized launch", func(t *testing.T) {
		c := dial()
		c.send(execLaunch, []byte("invalid json"))
		if kind, _, err := c.receive(); err != nil || kind != execError {
			t.Fatalf("malformed: %d %v", kind, err)
		}
		c = dial()
		var header [5]byte
		header[0] = execLaunch
		binary.LittleEndian.PutUint32(header[1:], maxExecFrame+1)
		c.Write(header[:])
		if _, _, err := c.receive(); err == nil {
			t.Fatal("accepted oversized frame")
		}
	})
	t.Run("explicit cancel", func(t *testing.T) {
		c := dial()
		launch(c, "hold")
		if err := c.send(execCancel, nil); err != nil {
			t.Fatal(err)
		}
		if kind, _, err := c.receive(); err != nil || kind != execExit {
			t.Fatalf("cancel: %d %v", kind, err)
		}
	})
	t.Run("shutdown cancels blocked input output and handshake", func(t *testing.T) {
		output, input := dial(), dial()
		launch(output, "spam")
		if kind, _, err := output.receive(); err != nil || kind != execStdout {
			t.Fatalf("output probe: %d %v", kind, err)
		}
		launch(input, "hold")
		if err := input.send(execStdin, bytes.Repeat([]byte("x"), 64*1024)); err != nil {
			t.Fatal(err)
		}
		input.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		if _, _, err := input.receive(); !os.IsTimeout(err) {
			t.Fatalf("stdin did not block as expected: %v", err)
		}
		_ = dial() // Connected client that never sends a launch request.
		done := make(chan struct{})
		go func() { r.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("shutdown blocked on exec I/O")
		}
	})
}
