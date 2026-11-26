package cli

import (
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func killTestSupervisor(t *testing.T, payloadPID uint32) {
	t.Helper()
	snapshot, err := win32.CreateToolhelp32Snapshot(win32.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(snapshot)
	var entry win32.PROCESSENTRY32W
	for more := win32.Process32First(snapshot, &entry); more; more = win32.Process32Next(snapshot, &entry) {
		if entry.Th32ProcessID != payloadPID {
			continue
		}
		h, err := win32.OpenProcess(win32.PROCESS_TERMINATE|win32.SYNCHRONIZE, false, entry.Th32ParentProcessID)
		if err != nil {
			t.Fatal(err)
		}
		defer win32.CloseHandle(h)
		if !win32.TerminateProcess(h, 1) || win32.WaitForSingleObject(h, 5000) != win32.WAIT_OBJECT_0 {
			t.Fatal("supervisor did not terminate")
		}
		return
	}
	t.Fatal("supervisor not found")
}

func TestContainerRenameUpdateRestart(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	binary, payload, buildDir := buildContainerTestBinaries(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) { c.Env["RESTART_COUNTER"] = "${data}/attempts" })
	app := reg.Host() + "/app:1"
	call := func(args ...string) (string, error) {
		deadline, cancel := stdcontext.WithTimeout(stdcontext.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(deadline, binary, args...)
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		out, err := cmd.CombinedOutput()
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
	fails := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := call(args...)
		if err == nil {
			t.Fatalf("unexpected success %v: %s", args, out)
		}
		return out
	}
	record := func(t *testing.T, target string) *store.ContainerRecord {
		t.Helper()
		rec, err := ctx.Store.ResolveContainer(target)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	stopped := func(t *testing.T, id string) {
		t.Helper()
		awaitPauseCondition(t, "supervisor did not exit", func() bool { return !ctx.Store.IsContainerRunning(id) })
	}
	t.Cleanup(func() {
		all, _ := ctx.Store.ListContainers(true)
		for _, c := range all {
			if out, err := call("rm", "-f", c.ID); err != nil {
				t.Errorf("cleanup: %s %v", out, err)
			}
		}
	})
	must(t, "pull", app)

	t.Run("saved settings rename and partial failures", func(t *testing.T) {
		must(t, "create", "--name", "saved", "--cpus=.5", "-m256m", "--pids-limit=4", "--restart=on-failure:3", app)
		before := record(t, "saved")
		if out := must(t, "container", "rename", "saved", "/Renamed"); out != "" {
			t.Fatalf("rename stdout: %q", out)
		}
		after := record(t, "Renamed")
		if after.ID != before.ID || after.DataDir != before.DataDir || after.State.Status != "created" {
			t.Fatal("rename changed identity")
		}
		must(t, "create", "--name", "saved", app)
		fails(t, "rename", "Renamed", "SAVED")
		out := fails(t, "update", "-m512m", "missing", "Renamed")
		if !strings.Contains(out, "Renamed\n") {
			t.Fatalf("missing successful output: %q", out)
		}
		after = record(t, "Renamed")
		if after.HostConfig.Memory != 512<<20 || after.HostConfig.NanoCPUs != 500_000_000 || after.HostConfig.RestartPolicy.MaximumRetryCount != 3 {
			t.Fatalf("partial update: %+v", after.HostConfig)
		}
		must(t, "update", "--memory=0", "--cpus=0", "--pids-limit=-1", "--restart=no", "Renamed")
		if after := record(t, "Renamed"); after.HostConfig.ContainerResources != (store.ContainerResources{}) || ctx.Store.IsContainerRunning(after.ID) {
			t.Fatal("clearing settings started container or retained limits")
		}
	})

	t.Run("live job limits and paused rename", func(t *testing.T) {
		ready := filepath.Join(buildDir, "limited")
		must(t, "run", "-d", "--name", "limited", "--pids-limit=1", "--memory=256m", app, "fixture.exe", "hold", ready)
		before := record(t, "limited")
		fails(t, "exec", "limited", "fixture.exe", "log", "blocked")
		must(t, "update", "--pids-limit=2", "limited")
		must(t, "exec", "-d", "limited", "fixture.exe", "hold", ready+"-exec")
		fails(t, "update", "--pids-limit=1", "--memory=512m", "--cpus=.5", "limited")
		if got := record(t, "limited"); got.HostConfig.PidsLimit != 2 || got.HostConfig.Memory != 256<<20 || got.HostConfig.NanoCPUs != 0 {
			t.Fatal("failed update changed configuration")
		}
		must(t, "update", "--pids-limit=0", "limited")
		out, err := call("exec", "limited", "fixture.exe", "allocate", fmt.Sprint(512<<20))
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 23 {
			t.Fatalf("memory was not enforced: %s %v", out, err)
		}
		must(t, "pause", "limited")
		must(t, "rename", "limited", "live-renamed")
		must(t, "update", "--memory=1g", "--cpus=.5", "live-renamed")
		if got := record(t, "live-renamed"); !got.State.Paused || got.State.RunID != before.State.RunID || got.State.Pid != before.State.Pid {
			t.Fatal("live update restarted or unpaused payload")
		}
		must(t, "unpause", "live-renamed")
		must(t, "exec", "live-renamed", "fixture.exe", "allocate", fmt.Sprint(512<<20))
		must(t, "restart", "-t", "0", "live-renamed")
		if got := record(t, "live-renamed"); got.HostConfig.Memory != 1<<30 || got.State.RunID == before.State.RunID {
			t.Fatal("restart lost limits")
		}
		must(t, "rm", "-f", "live-renamed")
		journal, err := ctx.Store.ReadEventSnapshot(stdcontext.Background())
		if err != nil {
			t.Fatal(err)
		}
		renamed := false
		for _, entry := range journal.Events {
			e := entry.Event
			if e.Actor.ID != before.ID {
				continue
			}
			if e.Action == "rename" {
				renamed = true
			}
			if renamed && e.Actor.Attributes["name"] != "live-renamed" {
				t.Fatalf("stale runtime event name: %+v", e)
			}
		}
	})

	t.Run("bounded retries preserve data and return first exit", func(t *testing.T) {
		out, err := call("run", "--name", "retries", "--restart=on-failure:2", app, "fixture.exe", "restart-exit", "${RESTART_COUNTER}", "7")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 7 || !strings.Contains(out, "attempt 1") {
			t.Fatalf("first exit: %q %v", out, err)
		}
		rec := record(t, "retries")
		stopped(t, rec.ID)
		rec = record(t, "retries")
		data, err := os.ReadFile(filepath.Join(rec.DataDir, "attempts"))
		if err != nil || string(data) != "3" || rec.RestartCount != 2 || rec.State.ExitCode != 7 || rec.State.Restarting {
			t.Fatalf("retries: %q %+v %v", data, rec, err)
		}
		if got := strings.Count(must(t, "logs", "retries"), "attempt "); got != 3 {
			t.Fatalf("log attempts: %d", got)
		}
		journal, err := ctx.Store.ReadEventSnapshot(stdcontext.Background())
		if err != nil {
			t.Fatal(err)
		}
		starts, deaths, automatic := 0, 0, 0
		runs := make(map[string]bool)
		for _, entry := range journal.Events {
			e := entry.Event
			if e.Actor.ID != rec.ID {
				continue
			}
			switch e.Action {
			case "start":
				starts++
				runs[e.Actor.Attributes["runID"]] = true
			case "die":
				deaths++
			case "restart":
				if e.Actor.Attributes["automatic"] == "true" {
					automatic++
				}
			}
		}
		if starts != 3 || deaths != 3 || automatic != 2 || len(runs) != 3 {
			t.Fatalf("retry events: starts=%d deaths=%d automatic=%d runs=%d", starts, deaths, automatic, len(runs))
		}
	})

	t.Run("enable policy on a running container and reject failed starts", func(t *testing.T) {
		ready, trigger := filepath.Join(buildDir, "policy-ready"), filepath.Join(buildDir, "policy-exit")
		must(t, "run", "-d", "--name", "live-policy", app, "fixture.exe", "wait-path", ready, trigger, "9")
		must(t, "update", "--restart=on-failure:1", "live-policy")
		rec := record(t, "live-policy")
		if err := os.WriteFile(trigger, nil, 0600); err != nil {
			t.Fatal(err)
		}
		stopped(t, rec.ID)
		if got := record(t, "live-policy"); got.RestartCount != 1 || got.State.ExitCode != 9 {
			t.Fatalf("live policy ignored: %+v", got)
		}
		fails(t, "run", "--name", "failed-policy", "--restart=always", app, "does-not-exist.exe")
		failed := record(t, "failed-policy")
		stopped(t, failed.ID)
		if failed.RestartCount != 0 || failed.State.Error == "" {
			t.Fatalf("failed launch retried: %+v", failed)
		}
		driveRoot := filepath.VolumeName(buildDir) + string(os.PathSeparator)
		out := fails(t, "run", "--name", "refused-grant", "--sandbox=low", "--filesystem="+driveRoot, app)
		if !strings.Contains(out, "it's the root of a drive") {
			t.Fatalf("supervisor lost launch diagnostic: %s", out)
		}
	})

	t.Run("limits and retries in isolated sandboxes", func(t *testing.T) {
		for _, sandbox := range []string{"low", "appcontainer"} {
			name := "sandbox-" + sandbox
			must(t, "run", "-d", "--name", name, "--sandbox="+sandbox, "--memory=256m", "--pids-limit=1", app, "fixture.exe", "hold-path", "$HOME/limited-ready")
			fails(t, "exec", name, "fixture.exe", "lines", "blocked")
			must(t, "update", "--pids-limit=2", "--cpus=.5", name)
			out, err := call("exec", name, "fixture.exe", "allocate", fmt.Sprint(512<<20))
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("%s memory enforcement: %s %v", sandbox, out, err)
			}
			must(t, "rm", "-f", name)
			name = "retry-" + sandbox
			_, err = call("run", "--name", name, "--sandbox="+sandbox, "--restart=on-failure:1", app, "fixture.exe", "restart-exit", "$RESTART_COUNTER", "9")
			if !errors.As(err, &exit) || exit.ExitCode() != 9 {
				t.Fatalf("%s first exit: %v", sandbox, err)
			}
			rec := record(t, name)
			stopped(t, rec.ID)
			if got := record(t, name); got.RestartCount != 1 || got.State.ExitCode != 9 {
				t.Fatalf("%s restart: %+v", sandbox, got)
			}
		}
	})

	t.Run("policy change cancels backoff", func(t *testing.T) {
		must(t, "run", "--name", "loop", "--restart=always", app, "fixture.exe", "restart-exit", "${RESTART_COUNTER}", "0")
		rec := record(t, "loop")
		awaitPauseCondition(t, "automatic restarts did not reach backoff", func() bool { c := record(t, "loop"); return c.RestartCount >= 2 && c.State.Restarting })
		if out := must(t, "ps", "--filter=status=restarting"); !strings.Contains(out, "loop") {
			t.Fatalf("restarting ps: %s", out)
		}
		must(t, "container", "update", "--restart=no", "--memory=512m", "loop")
		stopped(t, rec.ID)
		rec = record(t, "loop")
		if rec.State.Restarting || rec.HostConfig.Memory != 512<<20 || rec.HostConfig.RestartPolicy.Name != "no" {
			t.Fatalf("pending update: %+v", rec)
		}
	})

	t.Run("manual stop and supervisor loss", func(t *testing.T) {
		for _, policy := range []string{"always", "unless-stopped"} {
			name := "manual-" + policy
			ready := filepath.Join(buildDir, name)
			must(t, "run", "-d", "--name", name, "--restart="+policy, app, "fixture.exe", "hold", ready)
			before := record(t, name)
			must(t, "stop", "-t", "0", name)
			if got := record(t, name); got.RestartCount != 0 || ctx.Store.IsContainerRunning(got.ID) {
				t.Fatal("manual stop restarted")
			}
			must(t, "update", "--restart=always", name)
			if ctx.Store.IsContainerRunning(before.ID) {
				t.Fatal("update started exited container")
			}
			must(t, "start", name)
			after := record(t, name)
			if after.State.RunID == before.State.RunID || after.State.RestartSuppressed {
				t.Fatal("start did not reset session")
			}
			killTestSupervisor(t, after.State.Pid)
			stopped(t, after.ID)
			if got := record(t, name); got.State.Status != "exited" || got.State.Restarting {
				t.Fatal("supervisor crash was not reconciled")
			}
		}
		fails(t, "run", "--rm", "--restart=always", app)
		must(t, "create", "--rm", "--name", "auto", app)
		fails(t, "update", "--restart=always", "auto")
	})
}
