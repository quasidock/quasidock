package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
)

func buildContainerTestBinaries(t *testing.T) (quasidockExe string, fixturePayload []byte, buildDir string) {
	t.Helper()
	buildDir = t.TempDir()
	fixtureExe := filepath.Join(buildDir, "fixture.exe")
	cmd := exec.Command("go", "build", "-o", fixtureExe, "../../tests/cmd/clifixture")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build clifixture: %s %v", out, err)
	}
	payload, err := os.ReadFile(fixtureExe)
	if err != nil {
		t.Fatal(err)
	}
	quasidockExe = filepath.Join(buildDir, "quasidock.exe")
	cmd = exec.Command("go", "build", "-o", quasidockExe, "../../cmd/quasidock")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build quasidock: %s %v", out, err)
	}
	return quasidockExe, payload, buildDir
}

func TestContainerLifecycleForegroundAndDetached(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	binary, payload, buildDir := buildContainerTestBinaries(t)

	reg := testregistry.New()
	defer reg.Close()
	a, _ := fixtureImage(t, reg, "1", payload)
	v2, _ := fixtureImage(t, reg, "2", payload)
	_ = v2
	app1 := reg.Host() + "/app:1"

	call := func(stdin string, args ...string) (string, int) {
		t.Helper()
		c := exec.Command(binary, args...)
		c.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		if stdin != "" {
			c.Stdin = strings.NewReader(stdin)
		}
		var out, errout bytes.Buffer
		c.Stdout = &out
		c.Stderr = &errout
		err := c.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if code != 0 {
			return out.String() + errout.String(), code
		}
		return out.String(), code
	}

	// Pull app:1 first.
	if out, code := call("", "pull", app1); code != 0 {
		t.Fatalf("pull app:1: %s %d", out, code)
	}

	// Default stdin without -i reads NUL (EOF -> fixture stdin exits 4).
	if out, code := call("hello-stdin", "run", "--name", "no-stdin", app1, "fixture.exe", "stdin"); code != 4 {
		t.Fatalf("expected exit 4 without -i, got %d (%s)", code, out)
	}

	// With -i, stdin passes through.
	if out, code := call("hello-stdin", "run", "-it", "--name", "with-stdin", app1, "fixture.exe", "stdin"); code != 0 || out != "hello-stdin" {
		t.Fatalf("with -it stdin: %q (%d)", out, code)
	}

	// Duplicate name is refused, and invalid name is rejected.
	if out, code := call("", "run", "--name", "with-stdin", app1); code == 0 || !strings.Contains(out, "already in use") {
		t.Fatalf("expected duplicate name failure, got %q (%d)", out, code)
	}
	if out, code := call("", "run", "--name", "bad/name!", app1); code == 0 {
		t.Fatalf("expected invalid name failure, got %q (%d)", out, code)
	}

	// Foreground run persists container record, logs, and per-container DataDir.
	if out, code := call("", "run", "--name", "fg-log", app1, "fixture.exe", "log", "line-one", "line-two"); code != 0 {
		t.Fatalf("fg-log: %s %d", out, code)
	}
	if out, code := call("", "ps"); code != 0 || strings.Contains(out, "fg-log") {
		t.Fatalf("ps without -a should omit exited containers: %s %d", out, code)
	}
	if out, code := call("", "ps", "-a"); code != 0 || !strings.Contains(out, "fg-log") || !strings.Contains(out, "with-stdin") {
		t.Fatalf("ps -a missing persisted containers: %s %d", out, code)
	}
	if out, code := call("", "logs", "fg-log"); code != 0 || !strings.Contains(out, "line-one") || !strings.Contains(out, "line-two") {
		t.Fatalf("logs fg-log: %q %d", out, code)
	}
	if out, code := call("", "run", "--name", "fg-lines", app1, "fixture.exe", "lines", "first", "second", "third"); code != 0 {
		t.Fatalf("fg-lines: %s %d", out, code)
	}
	if out, code := call("", "logs", "--tail", "1", "fg-lines"); code != 0 || strings.TrimSpace(out) != "third" {
		t.Fatalf("logs --tail 1: %q %d", out, code)
	}

	// Unified inspect resolves containers first, falls back to images; typed inspect enforces scope.
	if out, code := call("", "inspect", "-f", "{{.Name}}", "fg-log"); code != 0 || strings.TrimSpace(out) != "/fg-log" {
		t.Fatalf("inspect container: %q %d", out, code)
	}
	if out, code := call("", "inspect", "-f", "{{.Id}}", app1); code != 0 || strings.TrimSpace(out) != a {
		t.Fatalf("inspect image fallback: %q %d", out, code)
	}
	if _, code := call("", "image", "inspect", "fg-log"); code == 0 {
		t.Fatal("image inspect resolved a container")
	}
	if _, code := call("", "container", "inspect", app1); code == 0 {
		t.Fatal("container inspect resolved an image")
	}
	var inspectedC []store.ContainerRecord
	if out, code := call("", "container", "inspect", "fg-log"); code != 0 || json.Unmarshal([]byte(out), &inspectedC) != nil || len(inspectedC) != 1 || inspectedC[0].BareName() != "fg-log" {
		t.Fatalf("container inspect JSON: %s %d", out, code)
	}

	// --rm deletes container directory on exit.
	if out, code := call("", "run", "--rm", "--name", "ephem-rm", app1, "fixture.exe", "log", "bye", ""); code != 0 {
		t.Fatalf("run --rm: %s %d", out, code)
	}
	if out, code := call("", "ps", "-a"); code != 0 || strings.Contains(out, "ephem-rm") {
		t.Fatalf("--rm container still listed: %s %d", out, code)
	}
	if _, err := ctx.Store.ResolveContainer("ephem-rm"); !errors.Is(err, store.ErrContainerNotFound) {
		t.Fatalf("--rm container still in store: %v", err)
	}

	// Detached container: run -d, wait for readiness, test ps, logs -f, rm refused, stop -t, rm.
	ready := filepath.Join(buildDir, "detached-ready")
	cidOut, code := call("", "run", "-d", "--name", "bg-holder", app1, "fixture.exe", "hold", ready)
	if code != 0 {
		t.Fatalf("run -d: %s %d", cidOut, code)
	}
	cid := strings.TrimSpace(cidOut)
	if len(cid) != 64 {
		t.Fatalf("expected 64-hex container ID, got %q", cid)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !store.Exists(ready) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !store.Exists(ready) {
		t.Fatal("detached container did not reach child execution")
	}
	if out, code := call("", "ps"); code != 0 || !strings.Contains(out, "bg-holder") {
		t.Fatalf("running detached container missing in ps: %s %d", out, code)
	}
	if out, code := call("", "rm", "bg-holder"); code == 0 || !strings.Contains(out, "running") {
		t.Fatalf("rm succeeded on running container: %q %d", out, code)
	}

	// Start logs -f in background, stop container, and confirm logs -f exits.
	followCmd := exec.Command(binary, "logs", "-f", "bg-holder")
	followCmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
	var followOut bytes.Buffer
	followCmd.Stdout = &followOut
	if err := followCmd.Start(); err != nil {
		t.Fatal(err)
	}
	followDone := make(chan error, 1)
	go func() { followDone <- followCmd.Wait() }()

	if out, code := call("", "stop", "-t", "1", "bg-holder"); code != 0 {
		t.Fatalf("stop bg-holder: %s %d", out, code)
	}
	select {
	case err := <-followDone:
		if err != nil {
			t.Fatalf("logs -f exited with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = followCmd.Process.Kill()
		t.Fatal("logs -f did not exit after container stopped")
	}
	if out, code := call("", "ps"); code != 0 || strings.Contains(out, "bg-holder") {
		t.Fatalf("stopped container still in ps: %s %d", out, code)
	}

	// Test rm -f on another running detached container.
	ready2 := filepath.Join(buildDir, "detached-ready-2")
	if out, code := call("", "run", "-d", "--name", "bg-force", app1, "fixture.exe", "hold", ready2); code != 0 {
		t.Fatalf("run -d bg-force: %s %d", out, code)
	}
	deadline = time.Now().Add(10 * time.Second)
	for !store.Exists(ready2) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if out, code := call("", "rm", "-f", "bg-force"); code != 0 {
		t.Fatalf("rm -f bg-force: %s %d", out, code)
	}
	if out, code := call("", "ps", "-a"); code != 0 || strings.Contains(out, "bg-force") {
		t.Fatalf("rm -f left container behind: %s %d", out, code)
	}

	// Clear remaining stopped containers via rm.
	if out, code := call("", "rm", "no-stdin", "with-stdin", "fg-log", "fg-lines", "bg-holder"); code != 0 {
		t.Fatalf("rm stopped containers: %s %d", out, code)
	}

	// Test quasidock prune collects stopped containers first, then unnamed pinned image.
	if out, code := call("", "pull", reg.Host()+"/app:latest"); code != 0 {
		t.Fatalf("pull latest: %s %d", out, code)
	}
	if out, code := call("", "run", "--name", "pin-latest", reg.Host()+"/app:latest"); code != 0 {
		t.Fatalf("run latest: %s %d", out, code)
	}
	// Replace :latest on registry with v3 and pull it, leaving previous manifest unnamed.
	oldLatest, _ := call("", "image", "inspect", "-f", "{{.Id}}", reg.Host()+"/app:latest")
	oldLatest = strings.TrimSpace(oldLatest)
	fixtureImage(t, reg, "3", payload)
	if out, code := call("", "pull", reg.Host()+"/app:latest"); code != 0 {
		t.Fatalf("pull new latest: %s %d", out, code)
	}
	// Unnamed old manifest must still be pinned by stopped container pin-latest.
	if out, code := call("", "rmi", oldLatest); code == 0 || !strings.Contains(out, "in use") {
		t.Fatalf("unnamed pinned image removed unexpectedly: %q %d", out, code)
	}
	// Running quasidock prune removes stopped container pin-latest AND unpinned oldLatest.
	if out, code := call("", "prune"); code != 0 {
		t.Fatalf("quasidock prune: %s %d", out, code)
	}
	if _, err := ctx.Store.ResolveContainer("pin-latest"); !errors.Is(err, store.ErrContainerNotFound) {
		t.Fatalf("prune did not remove stopped container: %v", err)
	}
	if out, code := call("", "image", "inspect", oldLatest); code == 0 {
		t.Fatalf("prune did not collect newly unpinned unnamed image: %s", out)
	}
}
