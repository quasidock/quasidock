package cli

import (
	"bytes"
	stdcontext "context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestContainerWaitLifecycle(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	binary, payload, buildDir := buildContainerTestBinaries(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	fixtureImage(t, reg, "1", payload)
	app := reg.Host() + "/app:1"
	command := func(t *testing.T, args ...string) *exec.Cmd {
		t.Helper()
		cancel, stop := stdcontext.WithTimeout(stdcontext.Background(), 30*time.Second)
		t.Cleanup(stop)
		cmd := exec.CommandContext(cancel, binary, args...)
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		return cmd
	}
	must := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := command(t, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s %v", args, out, err)
		}
		return string(out)
	}
	record := func(t *testing.T, target string) *store.ContainerRecord {
		t.Helper()
		rec, err := ctx.Store.ResolveContainer(target)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	t.Cleanup(func() {
		all, _ := ctx.Store.ListContainers(true)
		for _, c := range all {
			if out, err := command(t, "rm", "-f", c.ID).CombinedOutput(); err != nil {
				t.Errorf("cleanup: %s %v", out, err)
			}
		}
	})
	outputText := func(out *pauseOutput) string {
		out.mu.Lock()
		defer out.mu.Unlock()
		return out.b.String()
	}
	startWait := func(t *testing.T, targets ...string) (func(string), *pauseOutput) {
		t.Helper()
		args := append([]string{"--verbose", "container", "wait"}, targets...)
		cmd := command(t, args...)
		var out, diagnostics pauseOutput
		cmd.Stdout, cmd.Stderr = &out, &diagnostics
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		// Verbose subscription diagnostics are emitted after enrollment releases
		// its operation lock, so lifecycle controls can now race with completion.
		awaitPauseCondition(t, "wait did not subscribe: "+strings.Join(targets, ","), func() bool {
			return strings.Count(outputText(&diagnostics), "waiting for container ") == len(targets)
		})
		return func(want string) {
			t.Helper()
			select {
			case err := <-done:
				if err != nil || outputText(&out) != want {
					t.Fatalf("wait output %q, want %q: %v (%s)", outputText(&out), want, err, outputText(&diagnostics))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("wait did not finish")
			}
		}, &out
	}
	trigger := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	startPayload := func(t *testing.T, name, code string, options ...string) (*store.ContainerRecord, string) {
		t.Helper()
		ready, exit := filepath.Join(buildDir, name+".ready"), filepath.Join(buildDir, name+".exit")
		args := append([]string{"run", "-d", "--name", name}, options...)
		args = append(args, app, "fixture.exe", "wait-path", ready, exit, code)
		must(t, args...)
		awaitPauseCondition(t, "payload did not initialize", func() bool { return store.Exists(ready) })
		return record(t, name), exit
	}
	must(t, "pull", app)

	t.Run("ordered results retain auto-removed later target", func(t *testing.T) {
		_, slowExit := startPayload(t, "wait-slow", "17")
		fast, fastExit := startPayload(t, "wait-fast", "29", "--rm")
		finish, out := startWait(t, "wait-slow", fast.ID[:12])
		trigger(t, fastExit)
		awaitPauseCondition(t, "later target was not auto-removed", func() bool {
			_, err := ctx.Store.ResolveContainer(fast.ID)
			return errors.Is(err, store.ErrContainerNotFound)
		})
		if out.Len() != 0 {
			t.Fatal("later target printed before the first target")
		}
		trigger(t, slowExit)
		finish("17\n29\n")
		if got := must(t, "wait", "wait-slow"); got != "17\n" {
			t.Fatalf("stopped container: %q", got)
		}
	})

	t.Run("paused subscription follows rename", func(t *testing.T) {
		rec, exit := startPayload(t, "wait-paused", "23")
		must(t, "pause", rec.ID)
		finish, _ := startWait(t, rec.ID)
		if !record(t, rec.ID).State.Paused {
			t.Fatal("wait resumed a paused container")
		}
		must(t, "rename", rec.ID, "wait-renamed")
		must(t, "unpause", "wait-renamed")
		trigger(t, exit)
		finish("23\n")
	})

	t.Run("manual restart returns previous attempt", func(t *testing.T) {
		before, _ := startPayload(t, "wait-manual", "31")
		finish, _ := startWait(t, "wait-manual")
		must(t, "restart", "-t", "0", "wait-manual")
		finish("1\n")
		after := record(t, "wait-manual")
		if !after.State.Running || after.State.RunID == before.State.RunID {
			t.Fatalf("replacement was not left running: %+v", after.State)
		}
		must(t, "rm", "-f", "wait-manual")
	})

	t.Run("automatic restart returns first exit", func(t *testing.T) {
		rec, exit := startPayload(t, "wait-auto", "7", "--restart=always")
		finish, _ := startWait(t, "wait-auto")
		trigger(t, exit)
		finish("7\n")
		awaitPauseCondition(t, "automatic restart did not continue", func() bool { return record(t, rec.ID).RestartCount >= 1 })
		if !ctx.Store.IsContainerRunning(rec.ID) {
			t.Fatal("wait stopped the restart supervisor")
		}
		must(t, "rm", "-f", rec.ID)
	})

	t.Run("foreground container", func(t *testing.T) {
		ready, exit := filepath.Join(buildDir, "wait-foreground.ready"), filepath.Join(buildDir, "wait-foreground.exit")
		cmd := command(t, "run", "--name", "wait-foreground", app, "fixture.exe", "wait-path", ready, exit, "41")
		var foregroundOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &foregroundOut, &foregroundOut
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		awaitPauseCondition(t, "foreground fixture not ready", func() bool { return store.Exists(ready) })
		finish, _ := startWait(t, "wait-foreground")
		trigger(t, exit)
		finish("41\n")
		var status *exec.ExitError
		if err := <-done; !errors.As(err, &status) || status.ExitCode() != 41 {
			t.Fatalf("foreground exit: %v (%s)", err, &foregroundOut)
		}
	})

	t.Run("graceful stop and kill", func(t *testing.T) {
		for _, action := range []string{"stop", "kill"} {
			name := "wait-" + action
			ready := filepath.Join(buildDir, name+".ready")
			must(t, "run", "-d", "--name", name, app, "fixture.exe", "signal-hold", ready, "exit")
			awaitPauseCondition(t, "signal fixture not ready", func() bool { return store.Exists(ready) })
			finish, _ := startWait(t, name)
			if action == "stop" {
				must(t, "stop", "-t", "1", name)
				finish("0\n")
			} else {
				must(t, "kill", name)
				finish("1\n")
			}
		}
	})

	t.Run("Ctrl+C disconnects without signals", func(t *testing.T) {
		rec, _ := startPayload(t, "wait-interrupt", "43")
		t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
		var consoleOut pauseOutput
		console, input := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), []string{"console-run-check-mode", binary, "--verbose", "wait", rec.ID}, &consoleOut)
		awaitPauseCondition(t, "console wait did not subscribe", func() bool { return strings.Contains(outputText(&consoleOut), "waiting for container ") })
		if _, err := win32.WriteFile(input, []byte{3}); err != nil {
			t.Fatal(err)
		}
		if win32.WaitForSingleObject(console, 5000) != win32.WAIT_OBJECT_0 {
			t.Fatal("wait did not disconnect on Ctrl+C")
		}
		if code, err := win32.GetExitCodeProcess(console); err != nil || code != 130 {
			t.Fatalf("Ctrl+C: %d %v (%s)", code, err, outputText(&consoleOut))
		}
		if !record(t, rec.ID).State.Running {
			t.Fatal("Ctrl+C stopped the observed container")
		}
		must(t, "rm", "-f", rec.ID)
	})
}
