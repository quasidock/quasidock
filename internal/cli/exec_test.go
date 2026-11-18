package cli

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestParseExecOptions(t *testing.T) {
	for _, flags := range [][]string{{"-dit", "-e", "A=one", "-eB", "-w", "relative"}, {"--detach", "--interactive", "--tty", "--env=A=one", "--env", "B", "--workdir=relative"}} {
		opts, err := parseExecOptions(context.New(), append(flags, "target", "app", "--tty", "literal"))
		if err != nil || !opts.Detach || !opts.Interactive || !opts.TTY || opts.Workdir != "relative" || !reflect.DeepEqual(opts.Env, []string{"A=one", "B"}) || !reflect.DeepEqual(opts.Args, []string{"app", "--tty", "literal"}) {
			t.Fatalf("%v: %+v %v", flags, opts, err)
		}
	}
	for _, args := range [][]string{nil, {"target"}, {"target", ""}, {"-x", "target", "app"}, {"--mount=x", "target", "app"}, {"--detach=true", "target", "app"}, {"--workdir=", "target", "app"}, {"-e"}, {"-w"}} {
		if _, err := parseExecOptions(context.New(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	opts, err := parseExecOptions(context.New(), []string{"--", "target", "--command"})
	if err != nil || opts.Args[0] != "--command" {
		t.Fatalf("separator: %+v %v", opts, err)
	}
}

func TestContainerExec(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	binary, payload, buildDir := buildContainerTestBinaries(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) { c.Env["EXEC_BASE"] = "image" })
	app := reg.Host() + "/app:1"
	call := func(input string, args ...string) (string, string, int) {
		deadline, cancel := stdcontext.WithTimeout(stdcontext.Background(), 35*time.Second)
		defer cancel()
		cmd := exec.CommandContext(deadline, binary, args...)
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		cmd.Stdin = strings.NewReader(input)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		if err == nil {
			return out.String(), stderr.String(), 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out.String(), stderr.String(), exit.ExitCode()
		}
		return out.String(), stderr.String() + err.Error(), -1
	}
	must := func(args ...string) string {
		t.Helper()
		out, stderr, code := call("", args...)
		if code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out, stderr)
		}
		return out
	}
	waitFile := func(path string) string {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
				return string(data)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("fixture not ready: %s", path)
		return ""
	}
	t.Cleanup(func() {
		records, _ := ctx.Store.ListContainers(true)
		for _, rec := range records {
			_, _, _ = call("", "rm", "-f", rec.ID)
		}
	})
	must("pull", app)
	ready := filepath.Join(buildDir, "main-ready")
	must("run", "-d", "--name", "exec-main", "-e", "EXEC_BASE=saved", app, "fixture.exe", "signal-hold", ready, "ignore")
	waitFile(ready)
	before, err := ctx.Store.ResolveContainer("exec-main")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("streams exit status and literal arguments", func(t *testing.T) {
		out, stderr, code := call("", "exec", "exec-main", "fixture.exe", "log", "exec-out", "exec-err")
		if code != 0 || strings.TrimSpace(out) != "exec-out" || strings.TrimSpace(stderr) != "exec-err" {
			t.Fatalf("%d %q %q", code, out, stderr)
		}
		if _, _, code := call("", "container", "exec", before.ID[:12], "fixture.exe", "exit", "37"); code != 37 {
			t.Fatalf("exit code %d", code)
		}
		out = must("exec", "exec-main", "args.cmd", "spaces here", "", "%PATH%", "&|<>^()", `say "hi"`, "--tty")
		var got []string
		want := []string{"spaces here", "", "%PATH%", "&|<>^()", `say "hi"`, "--tty"}
		if json.Unmarshal([]byte(out), &got) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("batch arguments %q", out)
		}
		if logs := must("logs", "exec-main"); strings.Contains(logs, "exec-out") || strings.Contains(logs, "exec-err") {
			t.Fatalf("exec entered logs: %s", logs)
		}
	})
	t.Run("stdin and EOF", func(t *testing.T) {
		input := strings.Repeat("bidirectional payload\n", 100000)
		out, stderr, code := call(input, "exec", "-it", "exec-main", "fixture.exe", "stdin")
		if code != 0 || out != input {
			t.Fatalf("stdin: code=%d bytes=%d/%d stderr=%s", code, len(out), len(input), stderr)
		}
		out, stderr, code = call(input, "exec", "-i", "exec-main", "fixture.exe", "duplex")
		if code != 0 || out != input || stderr != input {
			t.Fatalf("duplex: code=%d stdout=%d stderr=%d", code, len(out), len(stderr))
		}
		if _, _, code := call("ignored", "exec", "exec-main", "fixture.exe", "stdin"); code != 4 {
			t.Fatalf("default stdin wasn't EOF: %d", code)
		}
	})
	t.Run("environment and workdir", func(t *testing.T) {
		t.Setenv("EXEC_INHERIT", "caller")
		t.Setenv("REMOVE_ME", "")
		os.Unsetenv("REMOVE_ME")
		out := must("exec", "-e", "EXEC_BASE=override", "-e", "exec_base=last", "-e", "EXEC_INHERIT", "-e", "REMOVE_ME", "exec-main", "fixture.exe", "env", "EXEC_BASE", "EXEC_INHERIT", "REMOVE_ME")
		for _, want := range []string{"EXEC_BASE=last:true", "EXEC_INHERIT=caller:true", "REMOVE_ME=:false"} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %s in %q", want, out)
			}
		}
		if out := must("exec", "exec-main", "fixture.exe", "env", "EXEC_BASE"); !strings.Contains(out, "EXEC_BASE=saved:true") {
			t.Fatalf("overrode saved environment: %s", out)
		}
		must("exec", "-w", "${data}", "exec-main", "fixture.exe", "write-path", "relative.txt", "shared")
		if data, err := os.ReadFile(filepath.Join(before.DataDir, "relative.txt")); err != nil || string(data) != "shared" {
			t.Fatalf("workdir: %q %v", data, err)
		}
		must("exec", "-w", "..", "exec-main", "fixture.exe", "read-path", "relative.txt")
		if _, _, code := call("", "exec", "-w", "${data}/missing", "exec-main", "fixture.exe"); code != 1 {
			t.Fatalf("missing workdir code %d", code)
		}
	})
	t.Run("detached and concurrent commands", func(t *testing.T) {
		if out := must("exec", "-d", "exec-main", "fixture.exe", "log", "detached-output", "detached-error"); out != "" {
			t.Fatalf("detached output leaked: %q", out)
		}
		if logs := must("logs", "exec-main"); strings.Contains(logs, "detached-output") || strings.Contains(logs, "detached-error") {
			t.Fatalf("detached exec entered logs: %s", logs)
		}
		must("exec", "-d", "exec-main", "fixture.exe", "write", "detached")
		if got := waitFile(filepath.Join(before.DataDir, "home", "saved.txt")); got != "detached" {
			t.Fatal(got)
		}
		results := make(chan string, 6)
		for i := range 6 {
			go func() {
				value := fmt.Sprint(i)
				out, stderr, code := call("", "exec", "exec-main", "fixture.exe", "lines", value)
				if code != 0 || strings.TrimSpace(out) != value {
					results <- fmt.Sprintf("%d %s %s", code, out, stderr)
				} else {
					results <- ""
				}
			}()
		}
		for range 6 {
			if err := <-results; err != "" {
				t.Error(err)
			}
		}
	})
	t.Run("launch failure and old supervisor", func(t *testing.T) {
		for _, args := range [][]string{{"exec", "exec-main", "missing.exe"}, {"exec", "-d", "exec-main", "missing.exe"}, {"exec", "absent", "fixture.exe"}, {"exec", "exec-main", "test.cli.fixture"}} {
			if _, _, code := call("", args...); code != 1 {
				t.Fatalf("accepted %v: %d", args, code)
			}
		}
		_, err := ctx.Store.UpdateContainer(before.ID, func(rec *store.ContainerRecord) error { rec.State.ExecProtocol = 0; return nil })
		if err != nil {
			t.Fatal(err)
		}
		_, stderr, code := call("", "exec", "exec-main", "fixture.exe")
		if code != 1 || !strings.Contains(stderr, "stop and start") {
			t.Fatalf("older owner: %d %s", code, stderr)
		}
		_, err = ctx.Store.UpdateContainer(before.ID, func(rec *store.ContainerRecord) error { rec.State.ExecProtocol = 1; return nil })
		if err != nil {
			t.Fatal(err)
		}
	})
	after, err := ctx.Store.ResolveContainer(before.ID)
	if err != nil || !reflect.DeepEqual(before.State, after.State) || !reflect.DeepEqual(before.RunSpec, after.RunSpec) || !reflect.DeepEqual(before.Config, after.Config) {
		t.Fatalf("exec changed main configuration: %v", err)
	}
	t.Run("native Ctrl+C cancels only exec", func(t *testing.T) {
		t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
		ready := filepath.Join(buildDir, "exec-ctrlc")
		consoleProcess, consoleInput := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), []string{"console-run", binary, "exec", "-it", "exec-main", "fixture.exe", "tree-hold", ready, "ignore"})
		waitFile(ready + ".child")
		if _, err := win32.WriteFile(consoleInput, []byte{3}); err != nil {
			t.Fatal(err)
		}
		if win32.WaitForSingleObject(consoleProcess, 5000) != win32.WAIT_OBJECT_0 {
			t.Fatal("Ctrl+C did not cancel exec")
		}
		if code, err := win32.GetExitCodeProcess(consoleProcess); err != nil || code != 130 {
			t.Fatalf("Ctrl+C exit: %d %v", code, err)
		}
		if !ctx.Store.IsContainerRunning(before.ID) {
			t.Fatal("exec Ctrl+C killed main container")
		}
	})
	t.Run("foreground owner and natural primary exit", func(t *testing.T) {
		ready, stop := filepath.Join(buildDir, "foreground-exec-ready"), filepath.Join(buildDir, "foreground-exec-stop")
		cmd := exec.Command(binary, "run", "--name", "exec-foreground", app, "fixture.exe", "wait-path", ready, stop, "23")
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill() })
		waitFile(ready)
		if out := must("exec", "exec-foreground", "fixture.exe", "lines", "foreground-exec"); strings.TrimSpace(out) != "foreground-exec" {
			t.Fatal(out)
		}
		execReady := ready + "-child"
		must("exec", "-d", "exec-foreground", "fixture.exe", "tree-hold", execReady, "ignore")
		var pid uint32
		fmt.Sscan(waitFile(execReady+".child"), &pid)
		child, err := win32.OpenProcess(win32.SYNCHRONIZE, false, win32.DWORD(pid))
		if err != nil {
			t.Fatal(err)
		}
		defer win32.CloseHandle(child)
		if err := os.WriteFile(stop, []byte("done"), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("primary exit changed: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("natural exit blocked on detached exec")
		}
		if win32.WaitForSingleObject(child, 5000) != win32.WAIT_OBJECT_0 {
			t.Fatal("natural exit left exec descendant")
		}
	})

	t.Run("caller disconnect and container shutdown", func(t *testing.T) {
		execReady := filepath.Join(buildDir, "exec-ready")
		cmd := exec.Command(binary, "exec", "-i", "exec-main", "fixture.exe", "tree-hold", execReady, "ignore")
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		cmd.Stdin = strings.NewReader(strings.Repeat("unread stdin", 100000))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill() })
		pid := waitFile(execReady)
		waitFile(execReady + ".child")
		var childPID uint32
		fmt.Sscan(pid, &childPID)
		child, err := win32.OpenProcess(win32.SYNCHRONIZE, false, win32.DWORD(childPID))
		if err != nil {
			t.Fatal(err)
		}
		defer win32.CloseHandle(child)
		cmd.Process.Kill()
		_ = cmd.Wait()
		if win32.WaitForSingleObject(child, 5000) != win32.WAIT_OBJECT_0 {
			t.Fatal("disconnect left exec running")
		}
		if !ctx.Store.IsContainerRunning(before.ID) {
			t.Fatal("disconnect killed main container")
		}
		must("exec", "-d", "exec-main", "fixture.exe", "tree-hold", execReady+"-detached", "ignore")
		pid = waitFile(execReady + "-detached")
		fmt.Sscan(pid, &childPID)
		detached, err := win32.OpenProcess(win32.SYNCHRONIZE, false, win32.DWORD(childPID))
		if err != nil {
			t.Fatal(err)
		}
		defer win32.CloseHandle(detached)
		must("kill", "exec-main")
		if win32.WaitForSingleObject(detached, 5000) != win32.WAIT_OBJECT_0 {
			t.Fatal("KILL left detached exec running")
		}
		if _, _, code := call("", "exec", "exec-main", "fixture.exe"); code != 1 {
			t.Fatal("exec accepted exited container")
		}
	})
}
