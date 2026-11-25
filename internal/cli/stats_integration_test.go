package cli

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestContainerStatsLifecycle(t *testing.T) {
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
	t.Cleanup(func() {
		all, _ := ctx.Store.ListContainers(true)
		for _, rec := range all {
			out, err := command("rm", "-f", rec.ID).CombinedOutput()
			if err != nil {
				t.Errorf("cleanup %s: %s %v", rec.ID, out, err)
			}
		}
	})
	must("pull", app)
	ready := filepath.Join(buildDir, "stats-main")
	must("run", "-d", "--name", "stats-main", app, "fixture.exe", "stats-hold", ready)
	awaitPauseCondition(t, "stats fixture not ready", func() bool { _, err := os.Stat(ready); return err == nil })
	rec, err := ctx.Store.ResolveContainer("stats-main")
	if err != nil || rec.State.StatsProtocol != 1 {
		t.Fatalf("stats capability: %+v %v", rec, err)
	}
	initial, err := run.ContainerStats(stdcontext.Background(), ctx, rec)
	if err != nil || initial.PrivateMemory < 32<<20 || initial.ReadBytes < 1<<20 || initial.WriteBytes < 1<<20 || initial.PIDs < 1 {
		t.Fatalf("raw stats: %+v %v", initial, err)
	}
	var snapshot statsRow
	text := must("stats", "--no-stream", "--no-trunc", "--format=json", "stats-main", rec.ID[:12])
	if err := json.Unmarshal([]byte(text), &snapshot); err != nil || snapshot.ID != rec.ID || snapshot.Name != "stats-main" || snapshot.CPUPerc == "--" || snapshot.PIDs != "1" {
		t.Fatalf("snapshot: %s %v", text, err)
	}
	childReady := filepath.Join(buildDir, "stats-exec")
	must("exec", "-d", "stats-main", "fixture.exe", "hold", childReady)
	awaitPauseCondition(t, "exec fixture not ready", func() bool { _, err := os.Stat(childReady); return err == nil })
	withExec, err := run.ContainerStats(stdcontext.Background(), ctx, rec)
	if err != nil || withExec.PIDs < initial.PIDs+1 {
		t.Fatalf("exec accounting: %+v %v", withExec, err)
	}
	must("pause", "stats-main")
	paused, err := run.ContainerStats(stdcontext.Background(), ctx, rec)
	if err != nil || paused.PIDs != withExec.PIDs || paused.PrivateMemory == 0 {
		t.Fatalf("paused accounting: %+v %v", paused, err)
	}
	if text := must("container", "stats", "stats-main", "--no-stream", "--format={{.CPUPerc}}"); strings.TrimSpace(text) != "0.00%" {
		t.Fatalf("paused CPU: %q", text)
	}
	must("unpause", "stats-main")
	must("create", "--name", "stats-created", app)
	text = must("stats", "--all", "--no-stream", "--format={{.Name}}:{{.PIDs}}")
	if !strings.Contains(text, "stats-created:0") || !strings.Contains(text, "stats-main:2") {
		t.Fatalf("all states: %s", text)
	}

	// A redirected live stream keeps sampling across stop/restart and completes
	// when the explicitly selected container is removed.
	stream := command("stats", "stats-main", "--format=json")
	var output pauseOutput
	var stderr bytes.Buffer
	stream.Stdout, stream.Stderr = &output, &stderr
	if err := stream.Start(); err != nil {
		t.Fatal(err)
	}
	streamDone := make(chan error, 1)
	go func() { streamDone <- stream.Wait() }()
	contains := func(text string) bool {
		output.mu.Lock()
		defer output.mu.Unlock()
		return strings.Contains(output.b.String(), text)
	}
	awaitPauseCondition(t, "live stats did not start", func() bool { return contains(`"PIDs":"2"`) })
	must("stop", "-t", "0", "stats-main")
	awaitPauseCondition(t, "live stats missed stop", func() bool { return contains(`"PIDs":"0"`) })
	must("start", "stats-main")
	awaitPauseCondition(t, "live stats missed restart", func() bool { return contains(`"PIDs":"1"`) })
	must("rm", "-f", "stats-main")
	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatalf("live stats: %s %v", stderr.String(), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("live stats did not finish after removal")
	}
	output.mu.Lock()
	if strings.Contains(output.b.String(), "\x1b") {
		t.Error("redirected stats emitted terminal escapes")
	}
	output.mu.Unlock()

	// Foreground runs expose the same stats pipe and must remain running when
	// the observer is interrupted.
	foregroundReady := filepath.Join(buildDir, "stats-foreground")
	foreground := command("run", "--name", "stats-foreground", app, "fixture.exe", "hold", foregroundReady)
	var foregroundOutput bytes.Buffer
	foreground.Stdout, foreground.Stderr = &foregroundOutput, &foregroundOutput
	if err := foreground.Start(); err != nil {
		t.Fatal(err)
	}
	foregroundDone := make(chan error, 1)
	go func() { foregroundDone <- foreground.Wait() }()
	awaitPauseCondition(t, "foreground fixture not ready", func() bool { _, err := os.Stat(foregroundReady); return err == nil })
	if text := must("stats", "--no-stream", "--format={{.PIDs}}", "stats-foreground"); strings.TrimSpace(text) != "1" {
		t.Fatalf("foreground stats: %s", text)
	}
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	var consoleOutput pauseOutput
	console, input := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), []string{"console-run-check-mode", binary, "stats", "stats-foreground"}, &consoleOutput)
	awaitPauseCondition(t, "console stats did not render", func() bool {
		consoleOutput.mu.Lock()
		defer consoleOutput.mu.Unlock()
		return strings.Contains(consoleOutput.b.String(), "PRIVATE MEMORY")
	})
	if _, err := win32.WriteFile(input, []byte{3}); err != nil {
		t.Fatal(err)
	}
	if win32.WaitForSingleObject(console, 5000) != win32.WAIT_OBJECT_0 {
		t.Fatal("stats did not exit on Ctrl+C")
	}
	if code, err := win32.GetExitCodeProcess(console); err != nil || code != 130 {
		t.Fatalf("stats Ctrl+C: %d %v", code, err)
	}
	foregroundRecord, err := ctx.Store.ResolveContainer("stats-foreground")
	if err != nil || !foregroundRecord.State.Running {
		t.Fatalf("stats interruption stopped workload: %+v %v", foregroundRecord, err)
	}
	must("rm", "-f", "stats-foreground")
	select {
	case <-foregroundDone:
	case <-time.After(5 * time.Second):
		t.Fatal("foreground run did not finish")
	}
	for _, sandbox := range []string{"low", "appcontainer"} {
		t.Run(sandbox, func(t *testing.T) {
			name := "stats-" + sandbox
			must("run", "-d", "--name", name, "--sandbox="+sandbox, app, "fixture.exe", "stats-hold", "$HOME/stats-ready")
			rec, err := ctx.Store.ResolveContainer(name)
			if err != nil {
				t.Fatal(err)
			}
			awaitPauseCondition(t, "sandbox resource fixture not ready", func() bool {
				sample, err := run.ContainerStats(stdcontext.Background(), ctx, rec)
				if err != nil {
					t.Fatalf("%s memory query: %v; logs: %s", sandbox, err, must("logs", name))
				}
				return sample.PrivateMemory >= 32<<20 && sample.WriteBytes >= 1<<20 && sample.ReadBytes >= 1<<20
			})
			must("rm", "-f", name)
		})
	}
}
