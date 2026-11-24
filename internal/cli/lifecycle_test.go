package cli

import (
	"bytes"
	stdcontext "context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestParseLifecycleArgs(t *testing.T) {
	for _, command := range []string{"start", "stop", "restart", "kill", "pause", "unpause"} {
		if _, _, _, err := parseLifecycleArgs(command, nil); err == nil {
			t.Fatalf("%s accepted no targets", command)
		}
	}
	for _, args := range [][]string{{"-t", "0", "one", "two"}, {"one", "--time=2"}, {"-t=3", "one"}} {
		_, _, targets, err := parseLifecycleArgs("restart", args)
		if err != nil || len(targets) == 0 {
			t.Fatalf("restart %v: %v", args, err)
		}
	}
	for _, signal := range []string{"KILL", "sigkill", "9", "TERM", "sigterm", "15"} {
		_, got, _, err := parseLifecycleArgs("kill", []string{"--signal=" + signal, "one"})
		if err != nil || (got != run.SignalKILL && got != run.SignalTERM) {
			t.Fatalf("kill signal %s: %s %v", signal, got, err)
		}
	}
	for command, cases := range map[string][][]string{
		"pause":   {{"-t", "1", "one"}, {"--signal=TERM", "one"}, {"--force", "one"}},
		"unpause": {{"--time=1", "one"}, {"-s", "TERM", "one"}},
		"start":   {{"-a", "one"}, {"-i", "one"}, {"--time=1", "one"}},
		"restart": {{"--time", "-1", "one"}, {"--time"}, {"-t=999999999999", "one"}, {"--signal=TERM", "one"}},
		"kill":    {{"-s", "INT", "one"}, {"--signal"}, {"--time=0", "one"}},
	} {
		for _, args := range cases {
			if _, _, _, err := parseLifecycleArgs(command, args); err == nil {
				t.Fatalf("%s accepted %v", command, args)
			}
		}
	}
}

func TestContainerStartRestartKill(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	binary, payload, buildDir := buildContainerTestBinaries(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) { c.Env["IMAGE_VERSION"] = "one" })
	app := reg.Host() + "/app:1"
	call := func(args ...string) (string, int) {
		deadline, cancel := stdcontext.WithTimeout(stdcontext.Background(), 40*time.Second)
		defer cancel()
		cmd := exec.CommandContext(deadline, binary, args...)
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode()
		}
		return string(out) + err.Error(), -1
	}
	must := func(t *testing.T, args ...string) string {
		t.Helper()
		out, code := call(args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return out
	}
	record := func(t *testing.T, name string) *store.ContainerRecord {
		t.Helper()
		rec, err := ctx.Store.ResolveContainer(name)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	stopped := func(t *testing.T, id string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for ctx.Store.IsContainerRunning(id) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if ctx.Store.IsContainerRunning(id) {
			t.Fatal("container did not exit")
		}
	}
	waitPID := func(t *testing.T, path string, want uint32) uint32 {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(path)
			pid, parseErr := strconv.ParseUint(string(data), 10, 32)
			if err == nil && parseErr == nil && (want == 0 || uint32(pid) == want) {
				return uint32(pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("fixture not ready: %s (PID %d)", path, want)
		return 0
	}
	t.Cleanup(func() {
		containers, _ := ctx.Store.ListContainers(true)
		for _, rec := range containers {
			if out, code := call("rm", "-f", rec.ID); code != 0 {
				t.Errorf("cleanup %s: %s (%d)", rec.ID, out, code)
			}
		}
	})
	must(t, "pull", app)

	t.Run("saved configuration and short-lived starts", func(t *testing.T) {
		t.Setenv("LIFECYCLE_VALUE", "original")
		t.Setenv("LIFECYCLE_ABSENT", "")
		if err := os.Unsetenv("LIFECYCLE_ABSENT"); err != nil {
			t.Fatal(err)
		}
		latest := reg.Host() + "/app:latest"
		must(t, "pull", latest)
		must(t, "run", "--name", "replay", "-e", "LIFECYCLE_VALUE", "-e", "LIFECYCLE_ABSENT", "--entrypoint=fixture.exe", latest, "env", "LIFECYCLE_VALUE", "LIFECYCLE_ABSENT", "IMAGE_VERSION")
		before := record(t, "replay")
		sentinel := filepath.Join(before.DataDir, "retained.txt")
		if err := os.WriteFile(sentinel, []byte("preserved"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("LIFECYCLE_VALUE", "changed")
		t.Setenv("LIFECYCLE_ABSENT", "now-present")
		fixtureImage(t, reg, "2", payload, func(c *oci.AppConfig) { c.Env["IMAGE_VERSION"] = "two" })
		must(t, "pull", latest)
		oldReady, err := win32.CreateEvent(run.ReadyEventName(before.ID, before.State.RunID), true, true)
		if err != nil {
			t.Fatal(err)
		}
		defer win32.CloseHandle(oldReady)
		must(t, "container", "start", before.ID[:12])
		stopped(t, before.ID)
		must(t, "restart", "replay")
		stopped(t, before.ID)
		after := record(t, before.ID)
		if after.ID != before.ID || after.Created != before.Created || after.Image != before.Image || after.Name != before.Name || after.State.RunID == before.State.RunID || after.State.Error != "" {
			t.Fatalf("start changed identity/configuration: before=%+v after=%+v", before, after)
		}
		logs := must(t, "logs", "replay")
		for _, line := range []string{"LIFECYCLE_VALUE=original:true", "LIFECYCLE_ABSENT=:false", "IMAGE_VERSION=one:true"} {
			if strings.Count(logs, line) != 3 {
				t.Fatalf("launch settings/logs changed: %q", logs)
			}
		}
		if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserved" {
			t.Fatalf("lost private data: %s %v", data, err)
		}
		if out, code := call("kill", "replay"); code == 0 || !strings.Contains(out, "not running") {
			t.Fatalf("kill accepted stopped container: %s %d", out, code)
		}
		if out, code := call("start", "missing", "replay"); code == 0 || !strings.Contains(out, "replay") {
			t.Fatalf("batch did not continue after failure: %s %d", out, code)
		}
		stopped(t, before.ID)
	})

	t.Run("TERM does not escalate and KILL ends descendants", func(t *testing.T) {
		ready := filepath.Join(buildDir, "tree-ready")
		must(t, "run", "-d", "--name", "tree", app, "fixture.exe", "tree-hold", ready, "ignore")
		rec := record(t, "tree")
		waitPID(t, ready, rec.State.Pid)
		childPID := waitPID(t, ready+".child", 0)
		parent, err := win32.OpenProcess(win32.SYNCHRONIZE, false, win32.DWORD(rec.State.Pid))
		if err != nil {
			t.Fatal(err)
		}
		defer win32.CloseHandle(parent)
		child, err := win32.OpenProcess(win32.SYNCHRONIZE, false, win32.DWORD(childPID))
		if err != nil {
			t.Fatal(err)
		}
		defer win32.CloseHandle(child)
		must(t, "container", "kill", "--signal=TERM", "tree")
		if win32.WaitForSingleObject(parent, 150) != win32.WAIT_TIMEOUT {
			t.Fatal("TERM escalated to termination")
		}
		must(t, "start", "tree")
		if got := record(t, "tree").State.Pid; got != rec.State.Pid {
			t.Fatal("start launched a duplicate running container")
		}
		began := time.Now()
		must(t, "restart", "-t", "1", "tree")
		if time.Since(began) < 900*time.Millisecond {
			t.Fatal("restart did not honor the grace period")
		}
		if win32.WaitForSingleObject(parent, 0) != win32.WAIT_OBJECT_0 || win32.WaitForSingleObject(child, 0) != win32.WAIT_OBJECT_0 {
			t.Fatal("restart left descendants of the old run alive")
		}
		after := record(t, "tree")
		waitPID(t, ready, after.State.Pid)
		newChildPID := waitPID(t, ready+".child", 0)
		deadline := time.Now().Add(5 * time.Second)
		for newChildPID == childPID && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			newChildPID = waitPID(t, ready+".child", 0)
		}
		if newChildPID == childPID {
			t.Fatal("replacement descendant did not start")
		}
		newChild, err := win32.OpenProcess(win32.SYNCHRONIZE, false, win32.DWORD(newChildPID))
		if err != nil {
			t.Fatal(err)
		}
		defer win32.CloseHandle(newChild)
		must(t, "kill", "tree")
		if win32.WaitForSingleObject(newChild, 0) != win32.WAIT_OBJECT_0 {
			t.Fatal("KILL left a descendant alive")
		}
		must(t, "container", "restart", "--time=0", "tree")
		must(t, "stop", "-t", "0", "tree")
	})

	t.Run("TERM cooperative exit", func(t *testing.T) {
		ready := filepath.Join(buildDir, "term-ready")
		must(t, "run", "-d", "--name", "cooperative", app, "fixture.exe", "signal-hold", ready, "exit")
		rec := record(t, "cooperative")
		waitPID(t, ready, rec.State.Pid)
		must(t, "kill", "-s", "15", "cooperative")
		stopped(t, rec.ID)
		if rec := record(t, "cooperative"); rec.State.ExitCode != 0 || rec.State.Status != "exited" {
			t.Fatalf("TERM lost application exit status: %+v", rec.State)
		}
		if logs := must(t, "logs", "cooperative"); !strings.Contains(logs, "signal received") {
			t.Fatalf("TERM lost final output: %s", logs)
		}
	})

	t.Run("stop forces exit when console signaling is unavailable", func(t *testing.T) {
		ready := filepath.Join(buildDir, "no-console-ready")
		must(t, "run", "-d", "--name", "no-console", app, "fixture.exe", "signal-hold", ready, "no-console")
		rec := record(t, "no-console")
		waitPID(t, ready, rec.State.Pid)
		must(t, "stop", "-t", "1", "no-console")
		if ctx.Store.IsContainerRunning(rec.ID) {
			t.Fatal("stop left the application running after TERM was unavailable")
		}
	})

	t.Run("auto removal resumes after restart", func(t *testing.T) {
		ready := filepath.Join(buildDir, "auto-ready")
		must(t, "run", "-d", "--rm", "--name", "automatic", app, "fixture.exe", "signal-hold", ready, "ignore")
		before := record(t, "automatic")
		waitPID(t, ready, before.State.Pid)
		sentinel := filepath.Join(before.DataDir, "retained.txt")
		if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		must(t, "restart", "-t", "0", "automatic")
		after := record(t, "automatic")
		if after.ID != before.ID || !after.HostConfig.AutoRemove || after.State.RunID == before.State.RunID {
			t.Fatal("restart did not preserve the --rm container")
		}
		if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
			t.Fatalf("restart lost --rm data: %s %v", data, err)
		}
		// Inject an unavailable original working directory for the next launch.
		cwd := after.RunSpec.HostCwd
		_, err := ctx.Store.UpdateContainer(after.ID, func(c *store.ContainerRecord) error {
			c.RunSpec.HostCwd = filepath.Join(buildDir, "missing-directory")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, code := call("restart", "-t", "0", "automatic"); code == 0 {
			t.Fatal("restart accepted an unavailable working directory")
		}
		failed := record(t, "automatic")
		if failed.State.Running || failed.State.Error == "" || !failed.HostConfig.AutoRemove {
			t.Fatalf("failed restart was not retained: %+v", failed)
		}
		_, err = ctx.Store.UpdateContainer(after.ID, func(c *store.ContainerRecord) error { c.RunSpec.HostCwd = cwd; return nil })
		if err != nil {
			t.Fatal(err)
		}
		must(t, "start", "automatic")
		must(t, "kill", "automatic")
		if _, err := ctx.Store.ResolveContainer("automatic"); !errors.Is(err, store.ErrContainerNotFound) {
			t.Fatalf("KILL did not auto-remove: %v", err)
		}
	})

	t.Run("retry failed launch and concurrent starts", func(t *testing.T) {
		exe := filepath.Join(buildDir, "later.exe")
		ready := filepath.Join(buildDir, "later-ready")
		if _, code := call("run", "--name", "retry", "--entrypoint="+exe, app, "signal-hold", ready, "ignore"); code == 0 {
			t.Fatal("missing executable ran")
		}
		before := record(t, "retry")
		if _, code := call("start", "retry"); code == 0 {
			t.Fatal("start accepted missing executable")
		}
		if err := os.WriteFile(exe, payload, 0o755); err != nil {
			t.Fatal(err)
		}
		type result struct {
			out  string
			code int
		}
		done := make(chan result, 2)
		for range 2 {
			go func() { out, code := call("start", "retry"); done <- result{out, code} }()
		}
		for range 2 {
			got := <-done
			if got.code != 0 || strings.TrimSpace(got.out) != "retry" {
				t.Fatalf("concurrent start failed: %s %d", got.out, got.code)
			}
		}
		after := record(t, "retry")
		waitPID(t, ready, after.State.Pid)
		if after.ID != before.ID || after.State.Error != "" {
			t.Fatal("retry did not reset launch failure")
		}
		must(t, "kill", "retry")
	})

	t.Run("foreground TERM preserves native console behavior", func(t *testing.T) {
		ready := filepath.Join(buildDir, "foreground-ready")
		cmd := exec.Command(binary, "run", "--name", "foreground", app, "fixture.exe", "signal-hold", ready, "ignore")
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		waitPID(t, ready, 0)
		if out, code := call("kill", "-s", "TERM", "foreground"); code == 0 || !strings.Contains(out, "foreground") {
			t.Fatalf("foreground TERM was not rejected: %s %d", out, code)
		}
		must(t, "kill", "foreground")
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("foreground supervisor did not finish")
		}
	})

	t.Run("start after supervisor crash", func(t *testing.T) {
		ready := filepath.Join(buildDir, "crash-ready")
		must(t, "run", "-d", "--name", "crashed", app, "fixture.exe", "signal-hold", ready, "ignore")
		before := record(t, "crashed")
		waitPID(t, ready, before.State.Pid)
		snapshot, err := win32.CreateToolhelp32Snapshot(win32.TH32CS_SNAPPROCESS, 0)
		if err != nil {
			t.Fatal(err)
		}
		var entry win32.PROCESSENTRY32W
		var parentID uint32
		for more := win32.Process32First(snapshot, &entry); more; more = win32.Process32Next(snapshot, &entry) {
			if entry.Th32ProcessID == before.State.Pid {
				parentID = entry.Th32ParentProcessID
				break
			}
		}
		win32.CloseHandle(snapshot)
		if parentID == 0 {
			t.Fatal("supervisor not found")
		}
		parent, err := win32.OpenProcess(win32.PROCESS_TERMINATE|win32.SYNCHRONIZE, false, win32.DWORD(parentID))
		if err != nil {
			t.Fatal(err)
		}
		defer win32.CloseHandle(parent)
		if !win32.TerminateProcess(parent, 1) || win32.WaitForSingleObject(parent, 5000) != win32.WAIT_OBJECT_0 {
			t.Fatal("supervisor did not terminate")
		}
		stopped(t, before.ID)
		if got := record(t, "crashed"); got.State.Running || got.State.Status != "exited" {
			t.Fatal("crashed supervisor was not reconciled")
		}
		must(t, "start", "crashed")
		after := record(t, "crashed")
		waitPID(t, ready, after.State.Pid)
		if after.ID != before.ID || after.State.RunID == before.State.RunID {
			t.Fatal("recovery did not reuse container identity with a new attempt")
		}
		must(t, "kill", "crashed")
	})

	t.Run("native foreground Ctrl+C", func(t *testing.T) {
		ready := filepath.Join(buildDir, "console-ready")
		// Use a real pseudoconsole so this checks the event received by the
		// application, rather than inferring it from process creation flags.
		t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
		consoleProcess, consoleInput := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), []string{"console-run", binary, "run", "--name", "console", app, "fixture.exe", "signal-hold", ready, "exit"})
		waitPID(t, ready, 0)
		if _, err := win32.WriteFile(consoleInput, []byte{3}); err != nil {
			t.Fatal(err)
		}
		if win32.WaitForSingleObject(consoleProcess, 5000) != win32.WAIT_OBJECT_0 {
			t.Fatal("foreground Ctrl+C did not reach application")
		}
		if data, err := os.ReadFile(ready + ".signal"); err != nil || string(data) != "0" {
			t.Fatalf("application did not receive native CTRL_C_EVENT: %q %v", data, err)
		}
	})
}

func startLifecycleConsole(t *testing.T, exe string, args []string, capture ...io.Writer) (win32.HANDLE, win32.HANDLE) {
	t.Helper()
	inRead, inWrite, err := win32.CreatePipe(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { win32.CloseHandle(inWrite) })
	defer win32.CloseHandle(inRead)
	outRead, outWrite, err := win32.CreatePipe(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(outWrite)
	output := os.NewFile(uintptr(outRead), "console-output")
	t.Cleanup(func() { _ = output.Close() })
	dst := io.Discard
	if len(capture) != 0 {
		dst = capture[0]
	}
	go func() { _, _ = io.Copy(dst, output) }()
	var pc uintptr
	if hr := win32.CreatePseudoConsole(win32.COORD{X: 100, Y: 30}, inRead, outWrite, 0, &pc); hr < 0 {
		t.Fatalf("CreatePseudoConsole: 0x%x", hr)
	}
	t.Cleanup(func() { win32.ClosePseudoConsole(pc) })
	attrs, err := win32.NewProcThreadAttributeList(1)
	if err != nil {
		t.Fatal(err)
	}
	defer attrs.Delete()
	if !attrs.SetPseudoConsole(pc) {
		t.Fatal("UpdateProcThreadAttribute(PSEUDOCONSOLE) failed")
	}
	var startup win32.STARTUPINFOEXW
	var info win32.PROCESS_INFORMATION
	if err := win32.CreateProcessEx(exe, process.BuildCommandLine(exe, args), nil, nil, false, win32.EXTENDED_STARTUPINFO_PRESENT, nil, "", &startup, attrs, &info); err != nil {
		t.Fatal(err)
	}
	win32.CloseHandle(info.HThread)
	t.Cleanup(func() {
		win32.TerminateProcess(info.HProcess, 1)
		win32.WaitForSingleObject(info.HProcess, 5000)
		win32.CloseHandle(info.HProcess)
	})
	return info.HProcess, inWrite
}
