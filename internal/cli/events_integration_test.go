package cli

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestContainerEvents(t *testing.T) {
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
	call := func(wantCode int, args ...string) string {
		t.Helper()
		cmd := command(args...)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("%v: %v", args, err)
			}
			code = exit.ExitCode()
		}
		if code != wantCode {
			t.Fatalf("%v: %d (want %d): %s %s", args, code, wantCode, out.String(), stderr.String())
		}
		return out.String()
	}
	t.Cleanup(func() {
		records, _ := ctx.Store.ListContainers(true)
		for _, rec := range records {
			cmd := command("rm", "-f", rec.ID)
			_ = cmd.Run()
		}
	})
	readEvents := func(name string) []store.Event {
		t.Helper()
		snapshot, err := ctx.Store.ReadEventSnapshot(stdcontext.Background())
		if err != nil {
			t.Fatal(err)
		}
		var events []store.Event
		for _, entry := range snapshot.Events {
			if entry.Event.Actor.Attributes["name"] == name {
				events = append(events, entry.Event)
			}
		}
		return events
	}
	actions := func(name string, execEvents bool) []string {
		var result []string
		for _, event := range readEvents(name) {
			if strings.HasPrefix(event.Action, "exec_") == execEvents {
				result = append(result, event.Action)
			}
		}
		return result
	}
	assertActions := func(name string, want []string, execEvents bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			got := actions(name, execEvents)
			if reflect.DeepEqual(got, want) {
				return
			}
			if time.Now().After(deadline) || len(got) > len(want) {
				t.Fatalf("%s events: %v, want %v", name, got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFile := func(path string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			if _, err := os.Stat(path); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("fixture did not create %s", path)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	call(0, "pull", app)
	call(7, "run", "--name", "events-fg", app, "fixture.exe", "exit", "7")
	assertActions("events-fg", []string{"create", "start", "die"}, false)
	if got := readEvents("events-fg")[2].Actor.Attributes["exitCode"]; got != "7" {
		t.Fatalf("foreground exit code: %s", got)
	}
	call(0, "run", "--rm", "--name", "events-rm", app, "fixture.exe", "exit", "0")
	assertActions("events-rm", []string{"create", "start", "die", "destroy"}, false)
	call(0, "run", "-d", "--rm", "--name", "events-bg-rm", app, "fixture.exe", "exit", "0")
	assertActions("events-bg-rm", []string{"create", "start", "die", "destroy"}, false)

	ready := filepath.Join(buildDir, "events-ready")
	call(0, "create", "--name", "events-cycle", app, "fixture.exe", "signal-hold", ready, "ignore")
	call(0, "start", "events-cycle")
	waitFile(ready)
	call(0, "start", "events-cycle") // Idempotent: no second start.
	call(0, "pause", "events-cycle")
	call(0, "unpause", "events-cycle")
	call(37, "exec", "events-cycle", "fixture.exe", "exit", "37")
	call(0, "exec", "-d", "events-cycle", "fixture.exe", "exit", "0")
	wantExec := []string{"exec_create", "exec_start", "exec_die", "exec_create", "exec_start", "exec_die"}
	assertActions("events-cycle", wantExec, true)
	call(1, "exec", "events-cycle", "does-not-exist.exe")
	assertActions("events-cycle", wantExec, true)

	// Killing an attached exec client must still produce completion in its
	// supervisor, while the primary container stays running.
	execReady := filepath.Join(buildDir, "events-exec-ready")
	client := command("exec", "events-cycle", "fixture.exe", "wait-path", execReady, execReady+".never", "0")
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Process.Kill() })
	waitFile(execReady)
	if err := client.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = client.Wait()
	wantExec = append(wantExec, "exec_create", "exec_start", "exec_die")
	assertActions("events-cycle", wantExec, true)

	// A detached exec is also joined and reported when the container stops.
	call(0, "exec", "-d", "events-cycle", "fixture.exe", "wait-path", execReady+".detached", execReady+".never", "0")
	waitFile(execReady + ".detached")
	call(0, "stop", "-t", "0", "events-cycle")
	wantExec = append(wantExec, "exec_create", "exec_start", "exec_die")
	assertActions("events-cycle", wantExec, true)
	call(0, "stop", "events-cycle") // Idempotent: no second stop.
	call(0, "restart", "-t", "0", "events-cycle")
	call(0, "restart", "-t", "0", "events-cycle")
	call(0, "kill", "--signal", "TERM", "events-cycle")
	call(0, "rm", "-f", "events-cycle")
	wantLifecycle := []string{"create", "start", "pause", "unpause", "kill", "die", "stop", "start", "restart", "kill", "die", "start", "restart", "kill", "kill", "die", "destroy"}
	assertActions("events-cycle", wantLifecycle, false)
	groups := make(map[string][]store.Event)
	seenTERM := false
	for _, event := range readEvents("events-cycle") {
		if strings.HasPrefix(event.Action, "exec_") {
			id := event.Actor.Attributes["execID"]
			if len(id) != 32 {
				t.Fatalf("invalid exec ID: %+v", event)
			}
			groups[id] = append(groups[id], event)
		}
		seenTERM = seenTERM || event.Action == "kill" && event.Actor.Attributes["signal"] == "15"
	}
	if len(groups) != 4 || !seenTERM {
		t.Fatalf("missing exec groups or TERM: %d %t", len(groups), seenTERM)
	}
	for _, events := range groups {
		if len(events) != 3 || events[0].Action != "exec_create" || events[1].Action != "exec_start" || events[2].Action != "exec_die" || events[2].Actor.Attributes["exitCode"] == "" {
			t.Fatalf("exec lifecycle: %+v", events)
		}
	}

	call(0, "create", "--rm", "--name", "events-stop-rm", app, "fixture.exe", "signal-hold", ready, "ignore")
	call(0, "start", "events-stop-rm")
	call(0, "stop", "-t", "0", "events-stop-rm")
	assertActions("events-stop-rm", []string{"create", "start", "kill", "die", "stop", "destroy"}, false)
	call(0, "create", "--name", "events-failed", app, "does-not-exist.exe")
	call(1, "start", "events-failed")
	assertActions("events-failed", []string{"create"}, false)
	call(0, "create", "--name", "events-pruned", app)
	call(0, "container", "prune")
	assertActions("events-pruned", []string{"create", "destroy"}, false)
	assertActions("events-failed", []string{"create", "destroy"}, false)

	// Historical JSON survives record deletion, and the system alias has the
	// same contract as the top-level command.
	for _, prefix := range [][]string{{"events"}, {"system", "events"}} {
		args := append(prefix, "--since=0", "--until", time.Now().Format(time.RFC3339Nano), "--filter", "container=events-cycle", "--format=json")
		text := call(0, args...)
		decoder := json.NewDecoder(strings.NewReader(text))
		var historical []store.Event
		for {
			var event store.Event
			if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("events JSON: %v: %s", err, text)
			}
			historical = append(historical, event)
		}
		if !reflect.DeepEqual(historical, readEvents("events-cycle")) {
			t.Fatalf("historical JSON differs from journal: %s", text)
		}
	}

	// Use the existing console fixture to verify a real live subscriber and its
	// Ctrl+C exit code without sending console signals to the test runner.
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	var consoleOutput pauseOutput
	console, input := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), []string{
		"console-run-check-mode", binary, "events", "--since=0", "--format={{.Action}}:{{index .Actor.Attributes \"name\"}}",
	}, &consoleOutput)
	contains := func(text string) bool {
		consoleOutput.mu.Lock()
		defer consoleOutput.mu.Unlock()
		return strings.Contains(consoleOutput.b.String(), text)
	}
	awaitPauseCondition(t, "events subscriber did not replay history", func() bool { return contains("create:events-cycle") })
	call(0, "create", "--name", "events-live", app)
	call(0, "rm", "events-live")
	awaitPauseCondition(t, "events subscriber did not follow live removal", func() bool { return contains("destroy:events-live") })
	if _, err := win32.WriteFile(input, []byte{3}); err != nil {
		t.Fatal(err)
	}
	if win32.WaitForSingleObject(console, 5000) != win32.WAIT_OBJECT_0 {
		t.Fatal("events did not stop on Ctrl+C")
	}
	if code, err := win32.GetExitCodeProcess(console); err != nil || code != 130 {
		t.Fatalf("events Ctrl+C exit code: %d %v", code, err)
	}

	// Deliberately delay start-event publication after the application has
	// launched. Ctrl+C during this window must wait for the start acknowledgement
	// instead of sending a TERM request to an attachment that is not ready yet.
	earlyReady := filepath.Join(buildDir, "events-early-interrupt")
	call(0, "create", "-it", "--name", "events-early-interrupt", app, "fixture.exe", "signal-hold", earlyReady, "exit")
	journalLock, err := store.OpenLock(ctx.Store.Path("events.lock"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer journalLock.Release()
	earlyConsole, earlyInput := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), []string{
		"console-run-check-mode", binary, "start", "-ai", "events-early-interrupt",
	})
	waitFile(earlyReady)
	if _, err := win32.WriteFile(earlyInput, []byte{3}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	journalLock.Release()
	if win32.WaitForSingleObject(earlyConsole, 5000) != win32.WAIT_OBJECT_0 {
		t.Fatal("Ctrl+C during start-event publication was lost")
	}
	if code, err := win32.GetExitCodeProcess(earlyConsole); err != nil || code != 0 {
		t.Fatalf("early Ctrl+C exit code: %d %v", code, err)
	}
	assertActions("events-early-interrupt", []string{"create", "start", "kill", "die"}, false)
}
