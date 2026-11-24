package cli

import (
	"bytes"
	stdcontext "context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestParseAttachAndStartOptions(t *testing.T) {
	for _, flags := range [][]string{{"-ai"}, {"--attach", "--interactive"}, {"-ia", "--detach-keys=", "--sig-proxy=false"}} {
		opts, err := parseStreamOptions(context.New(), append(flags, "target"), true)
		if err != nil || !opts.attach || !opts.interactive || opts.stream.NoStdin || opts.stream.Container != "target" {
			t.Fatalf("%v: %+v %v", flags, opts, err)
		}
	}
	for _, start := range []bool{false, true} {
		for _, args := range [][]string{nil, {""}, {"-", "target"}, {"--unknown=x", "target"}, {"--detach-keys", "invalid", "target"}, {"--sig-proxy=x", "target"}, {"--verbose=true", "target"}} {
			if _, err := parseStreamOptions(context.New(), args, start); err == nil {
				t.Fatalf("accepted %t %v", start, args)
			}
		}
	}
	for _, args := range [][]string{{"a", "b"}, {"--interactive", "a"}, {"-i", "a"}, {"--no-stdin=true", "a"}} {
		if _, err := parseStreamOptions(context.New(), args, false); err == nil {
			t.Fatalf("attach accepted %v", args)
		}
	}
	for _, args := range [][]string{{"-a", "a", "b"}, {"-i", "a", "b"}, {"--detach-keys=", "a"}, {"--sig-proxy=false", "a"}, {"--no-stdin", "a"}} {
		if _, err := parseStreamOptions(context.New(), args, true); err == nil {
			t.Fatalf("start accepted %v", args)
		}
	}
	opts, err := parseStreamOptions(context.New(), []string{"--no-stdin", "--sig-proxy=false", "--detach-keys", "ctrl-x,x", "--", "target"}, false)
	if err != nil || !opts.stream.NoStdin || opts.stream.SigProxy || *opts.stream.DetachKeys != "ctrl-x,x" {
		t.Fatalf("attach flags: %+v %v", opts, err)
	}
	if opts, err := parseStreamOptions(context.New(), []string{"one", "two"}, true); err != nil || len(opts.targets) != 2 {
		t.Fatalf("plain start: %+v %v", opts, err)
	}
}

func TestContainerAttachAndAttachedStart(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	binary, payload, buildDir := buildContainerTestBinaries(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	fixtureImage(t, reg, "1", payload)
	app := reg.Host() + "/app:1"
	command := func(args ...string) *exec.Cmd {
		deadline, cancel := stdcontext.WithTimeout(stdcontext.Background(), 35*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(deadline, binary, args...)
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		return cmd
	}
	call := func(input string, args ...string) (string, string, int) {
		t.Helper()
		cmd := command(args...)
		cmd.Stdin = strings.NewReader(input)
		var out, errout bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errout
		err := cmd.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		return out.String(), errout.String(), code
	}
	must := func(args ...string) string {
		t.Helper()
		out, stderr, code := call("", args...)
		if code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out, stderr)
		}
		return out
	}
	record := func(name string) *store.ContainerRecord {
		t.Helper()
		rec, err := ctx.Store.ResolveContainer(name)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	waitLog := func(t *testing.T, path, value string) {
		t.Helper()
		deadline := time.Now().Add(7 * time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), value) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("missing %q in %s", value, path)
	}
	t.Cleanup(func() {
		records, _ := ctx.Store.ListContainers(true)
		for _, rec := range records {
			call("", "rm", "-f", rec.ID)
		}
	})
	must("pull", app)

	t.Run("immediate output status and auto-removal", func(t *testing.T) {
		for _, name := range []string{"short-one", "short-two", "short-three"} {
			must("create", "--rm", "--name", name, app, "fixture.exe", "log-exit", "stdout", "stderr", "23")
			out, stderr, code := call("", "container", "start", "-a", name)
			if code != 23 || out != "stdout\n" || stderr != "stderr\n" {
				t.Fatalf("short-lived start: %d %q %q", code, out, stderr)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, err := ctx.Store.ResolveContainer(name)
				if errors.Is(err, store.ErrContainerNotFound) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("--rm record retained: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
	t.Run("saved stdin and byte-preserving redirected TTY", func(t *testing.T) {
		must("create", "-it", "--name", "duplex-start", app, "fixture.exe", "duplex")
		input := "first\x00\x10\x11last"
		out, stderr, code := call(input, "start", "-i", "duplex-start")
		if code != 0 || out != input || stderr != input {
			t.Fatalf("duplex: %d %q %q", code, out, stderr)
		}
		must("create", "--name", "saved-no-stdin", app, "fixture.exe", "stdin")
		_, stderr, code = call("ignored", "start", "-ai", "saved-no-stdin")
		if code != 4 || stderr != "" || record("saved-no-stdin").Config.OpenStdin {
			t.Fatalf("changed saved stdin: %d %q", code, stderr)
		}
	})
	t.Run("detached interactive runs and already-running start", func(t *testing.T) {
		must("run", "-dit", "--name", "detach-input", app, "fixture.exe", "duplex")
		before := record("detach-input")
		if !ctx.Store.IsContainerRunning(before.ID) {
			t.Fatal("detached stdin got premature EOF")
		}
		out, stderr, code := call("new-input", "container", "attach", before.ID[:12])
		if code != 0 || out != "new-input" || stderr != "new-input" {
			t.Fatalf("attach: %d %q %q", code, out, stderr)
		}
		must("start", "detach-input")
		before = record("detach-input")
		out, stderr, code = call("existing-run", "start", "-ai", "detach-input")
		if code != 0 || out != "existing-run" || stderr != "existing-run" || before.State.RunID != record("detach-input").State.RunID {
			t.Fatalf("existing run: %d %q %q", code, out, stderr)
		}
	})
	t.Run("child closes stdin before its nonzero exit", func(t *testing.T) {
		must("create", "-i", "--name", "closed-input", app, "fixture.exe", "close-stdin")
		out, stderr, code := call(strings.Repeat("x", 1024*1024), "start", "-ai", "closed-input")
		if code != 27 || out != "closed-input-output\n" || stderr != "closed-input-error\n" {
			t.Fatalf("closed input lost status or output: %d %q %q", code, out, stderr)
		}
	})
	t.Run("client termination releases stdin without EOF or log replay", func(t *testing.T) {
		must("run", "-di", "--name", "disconnect", app, "fixture.exe", "duplex")
		rec := record("disconnect")
		cmd := command("attach", "disconnect")
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(input, "before-detach"); err != nil {
			t.Fatal(err)
		}
		waitLog(t, rec.LogPath, "before-detach")
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = input.Close()
		if !ctx.Store.IsContainerRunning(rec.ID) {
			t.Fatal("disconnect killed main")
		}
		var out, stderr string
		var code int
		deadline := time.Now().Add(5 * time.Second)
		for {
			out, stderr, code = call("after-detach", "attach", "disconnect")
			if !strings.Contains(stderr, "already has a writer") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(stderr)
			}
		}
		if code != 0 || out != "after-detach" || stderr != "after-detach" {
			t.Fatalf("reattach: %d %q %q", code, out, stderr)
		}
	})
	t.Run("output-only start releases lifecycle lock", func(t *testing.T) {
		ready := filepath.Join(buildDir, "attach-start-hold")
		must("create", "-i", "--name", "output-only", app, "fixture.exe", "signal-hold", ready, "ignore")
		cmd := command("start", "-a", "output-only")
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitLog(t, ready, "")
		must("kill", "output-only")
		if err := cmd.Wait(); err == nil {
			t.Fatal("kill returned zero")
		}
		if !strings.Contains(out.String(), "started") || stderr.Len() != 0 {
			t.Fatalf("output-only: %q %q", out.String(), stderr.String())
		}
	})
	t.Run("restart ends the old attachment", func(t *testing.T) {
		must("run", "-di", "--name", "attached-restart", app, "fixture.exe", "duplex")
		before := record("attached-restart")
		cmd := command("attach", "attached-restart")
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = input.Close() }()
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(input, "old-run"); err != nil {
			t.Fatal(err)
		}
		waitLog(t, before.LogPath, "old-run")
		must("restart", "-t", "0", "attached-restart")
		if err := cmd.Wait(); err == nil {
			t.Fatal("old attachment did not return killed status")
		}
		if out.String() != "old-run" || stderr.String() != "old-run" || before.State.RunID == record("attached-restart").State.RunID {
			t.Fatalf("restart boundary: %q %q", out.String(), stderr.String())
		}
		newOut, newErr, code := call("new-run", "attach", "attached-restart")
		if code != 0 || newOut != "new-run" || newErr != "new-run" {
			t.Fatalf("replacement: %d %q %q", code, newOut, newErr)
		}
	})
	t.Run("failed starts and incompatible owners", func(t *testing.T) {
		must("create", "--rm", "--name", "bad-attached", app, "missing.exe")
		_, stderr, code := call("", "start", "-a", "bad-attached")
		if code != 1 || stderr == "" || record("bad-attached").State.Launched {
			t.Fatalf("failed start: %d %s", code, stderr)
		}
		ready := filepath.Join(buildDir, "old-attach-owner")
		must("run", "-d", "--name", "old-owner", app, "fixture.exe", "signal-hold", ready, "ignore")
		rec := record("old-owner")
		_, err := ctx.Store.UpdateContainer(rec.ID, func(rec *store.ContainerRecord) error { rec.State.AttachProtocol = 0; return nil })
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"attach", "old-owner"}, {"start", "-a", "old-owner"}} {
			_, stderr, code := call("", args...)
			if code != 1 || !strings.Contains(stderr, "stop and start") {
				t.Fatalf("old owner: %d %s", code, stderr)
			}
		}
		must("kill", "old-owner")
		if _, _, code := call("", "attach", "old-owner"); code != 1 {
			t.Fatal("attached to stopped container")
		}
	})
	t.Run("console detach restores mode and leaves main alive", func(t *testing.T) {
		t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
		must("run", "-dit", "--name", "keys", app, "fixture.exe", "duplex")
		rec := record("keys")
		for i, flags := range [][]string{nil, {"--detach-keys=ctrl-x,x"}} {
			args := append([]string{"console-run-check-mode", binary, "attach"}, flags...)
			args = append(args, "keys")
			capture, err := os.CreateTemp(buildDir, "console-attach-output-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = capture.Close() })
			proc, input := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), args, capture)
			t.Cleanup(func() {
				if t.Failed() {
					data, _ := os.ReadFile(capture.Name())
					t.Logf("console: %s", data)
				}
			})
			probe := strings.Repeat("ready", i+1)
			if _, err := win32.WriteFile(input, []byte(probe+"\r")); err != nil {
				t.Fatal(err)
			}
			waitLog(t, rec.LogPath, probe)
			keys := []byte{16, 17}
			if i == 1 {
				keys = []byte{24, 'x'}
			}
			if _, err := win32.WriteFile(input, keys); err != nil {
				t.Fatal(err)
			}
			if win32.WaitForSingleObject(proc, 5000) != win32.WAIT_OBJECT_0 {
				t.Fatal("detach keys did not disconnect")
			}
			if code, _ := win32.GetExitCodeProcess(proc); code != 0 {
				t.Fatalf("detach or console restoration failed: %d", code)
			}
			if !ctx.Store.IsContainerRunning(rec.ID) {
				t.Fatal("detach stopped main")
			}
		}
		must("kill", "keys")
	})
	t.Run("Ctrl+C forwards TERM or detaches", func(t *testing.T) {
		t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
		for _, tc := range []struct{ proxy, paused bool }{{true, false}, {false, false}, {true, true}, {false, true}} {
			proxy := tc.proxy
			name := "ctrl-proxy"
			if !proxy {
				name = "ctrl-detach"
			}
			if tc.paused {
				name += "-paused"
			}
			ready := filepath.Join(buildDir, name)
			must("create", "-it", "--name", name, app, "fixture.exe", "signal-hold", ready, "exit")
			args := []string{"console-run-check-mode", binary, "start", "-ai"}
			if !proxy {
				args = append(args, "--sig-proxy=false")
			}
			args = append(args, name)
			proc, input := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), args)
			waitLog(t, ready, "")
			if tc.paused {
				must("pause", name)
			}
			if _, err := win32.WriteFile(input, []byte{3}); err != nil {
				t.Fatal(err)
			}
			if win32.WaitForSingleObject(proc, 5000) != win32.WAIT_OBJECT_0 {
				t.Fatal("Ctrl+C did not complete")
			}
			want := uint32(0)
			if !proxy {
				want = 130
			}
			if code, _ := win32.GetExitCodeProcess(proc); code != win32.DWORD(want) {
				t.Fatalf("Ctrl+C exit: %d want %d", code, want)
			}
			if proxy {
				waitLog(t, ready+".signal", "1")
				if record(name).State.Paused {
					t.Fatal("forwarded TERM retained pause state")
				}
			} else {
				if rec := record(name); !ctx.Store.IsContainerRunning(rec.ID) || rec.State.Paused != tc.paused {
					t.Fatal("Ctrl+C without proxy changed main or pause state")
				}
				must("kill", name)
			}
		}
		ready := filepath.Join(buildDir, "ignored-attach-TERM")
		must("create", "-it", "--name", "ignored-term", app, "fixture.exe", "signal-hold", ready, "ignore")
		proc, input := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), []string{"console-run-check-mode", binary, "start", "-ai", "ignored-term"})
		waitLog(t, ready, "")
		if _, err := win32.WriteFile(input, []byte{3}); err != nil {
			t.Fatal(err)
		}
		waitLog(t, ready+".signal", "1")
		if win32.WaitForSingleObject(proc, 0) != win32.WAIT_TIMEOUT {
			t.Fatal("ignored TERM ended attachment")
		}
		if _, err := win32.WriteFile(input, []byte{16, 17}); err != nil {
			t.Fatal(err)
		}
		if win32.WaitForSingleObject(proc, 5000) != win32.WAIT_OBJECT_0 {
			t.Fatal("could not detach after ignored TERM")
		}
		if code, _ := win32.GetExitCodeProcess(proc); code != 0 || !ctx.Store.IsContainerRunning(record("ignored-term").ID) {
			t.Fatalf("ignored TERM detach: %d", code)
		}
		must("kill", "ignored-term")
	})
}
