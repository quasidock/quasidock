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
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestRunMountOptions(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	for _, flag := range []string{"-v", "--volume", "-v=", "-v" + `work:${data}`} {
		args := []string{flag, `work:${data}`, "app", "--mount=app-argument"}
		if flag == "-v=" {
			args = []string{flag + `work:${data}`, "app", "--mount=app-argument"}
		}
		if strings.HasPrefix(flag, "-vwork") {
			args = []string{flag, "app", "--mount=app-argument"}
		}
		opts, err := parseRunOptions(ctx, args)
		if err != nil || len(opts.Mounts) != 1 || len(opts.Args) != 1 {
			t.Fatalf("%v: %+v %v", args, opts, err)
		}
	}
	if _, err := parseRunOptions(ctx, []string{"-v", `a:${data}`, "--mount", `type=volume,source=b,target=${data}/x`, "app"}); err == nil {
		t.Fatal("accepted overlapping targets")
	}
}

func mountCLI(t *testing.T) (*store.Store, func(string, ...string) (string, int), *testregistry.Registry, []byte) {
	t.Helper()
	ctx, _ := fixtureCLI(t)
	binary, payload, _ := buildContainerTestBinaries(t)
	ledger := `Software\Quasidock\Tests\Mounts` + fmt.Sprint(time.Now().UnixNano())
	call := func(home string, args ...string) (string, int) {
		t.Helper()
		cctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, binary, args...)
		cmd.Dir = filepath.Dir(home)
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+home, "QUASIDOCK_GRANTS_KEY="+ledger)
		var out, errout bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errout
		err := cmd.Run()
		if err == nil {
			return out.String(), 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out.String() + errout.String(), exit.ExitCode()
		}
		t.Fatalf("CLI %v: %v (%s)", args, err, errout.String())
		return "", 1
	}
	t.Cleanup(func() {
		all, _ := ctx.Store.ListContainers(true)
		for _, c := range all {
			_, _ = call(ctx.Store.Root, "rm", "-f", c.ID)
		}
		_, _ = call(ctx.Store.Root, "repair")
		k := regkey.Key{Handle: win32.HKEY_CURRENT_USER}
		_ = k.DeleteTree(ledger)
	})
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	return ctx.Store, call, reg, payload
}

func TestPersistentMountsCLI(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	id := "mount-fixture-" + fmt.Sprint(time.Now().UnixNano())
	fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) { c.ID = id })
	fixtureImage(t, reg, "2", payload, func(c *oci.AppConfig) { c.ID = id })
	t.Cleanup(func() { _ = appcontainer.DeleteProfile(id) })
	app := reg.Host() + "/app:1"
	ok := func(args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return out
	}
	ok("pull", app)
	for _, sb := range []string{"soft", "low", "appcontainer"} {
		t.Run(sb, func(t *testing.T) {
			volume := "data-" + sb
			ok("run", "--rm", "--sandbox="+sb, "-v", volume+":${data}", app, "fixture.exe", "write", "persist-"+sb)
			if out := ok("run", "--rm", "--sandbox="+sb, "-v", volume+":${data}", app, "fixture.exe", "read"); out != "persist-"+sb {
				t.Fatalf("root persistence: %q", out)
			}
			spec := "type=volume,source=sub-" + sb + ",target=${data}/workspace"
			ok("run", "--rm", "--sandbox="+sb, "--mount", spec, app, "fixture.exe", "write-path", `$HOME\..\workspace\sentinel`, "sub-data")
			if out := ok("run", "--rm", "--sandbox="+sb, "--mount", spec, app, "fixture.exe", "read-path", `$HOME\..\workspace\sentinel`); out != "sub-data" {
				t.Fatalf("subpath persistence: %q", out)
			}
			bind := t.TempDir()
			ok("run", "--rm", "--sandbox="+sb, "-v", bind+":${data}/workspace", app, "fixture.exe", "write-path", `$HOME\..\workspace\bind.txt`, "host-data")
			if b, err := os.ReadFile(filepath.Join(bind, "bind.txt")); err != nil || string(b) != "host-data" {
				t.Fatalf("bind: %s %v", b, err)
			}
		})
	}
	ok("run", "--name", "retained", "-v", `data-soft:${data}`, app, "fixture.exe", "read")
	relativeSource := filepath.Join(filepath.Dir(st.Root), "relative-source")
	if err := os.Mkdir(relativeSource, 0o755); err != nil {
		t.Fatal(err)
	}
	ok("run", "-d", "--rm", "--name", "relative-bind", "--mount", `type=bind,source=relative-source,target=${data}`, app, "fixture.exe", "hold-path", `$HOME\ready`)
	deadline := time.Now().Add(5 * time.Second)
	for !store.Exists(filepath.Join(relativeSource, "home", "ready")) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !store.Exists(filepath.Join(relativeSource, "home", "ready")) {
		t.Fatal("detached bind lost the original caller's relative path")
	}
	ok("stop", "-t", "0", "relative-bind")
	if out := ok("run", "--rm", "-v", `data-soft:${data}`, reg.Host()+"/app:2", "fixture.exe", "read"); out != "persist-soft" {
		t.Fatalf("data lost across image versions: %q", out)
	}
	if out, code := call(st.Root, "volume", "rm", "data-soft"); code == 0 || !strings.Contains(out, "referenced") {
		t.Fatalf("volume not pinned: %s %d", out, code)
	}
	out := ok("inspect", "retained")
	var recs []store.ContainerRecord
	if err := json.Unmarshal([]byte(out), &recs); err != nil || len(recs) != 1 || len(recs[0].Mounts) != 1 || !recs[0].Mounts[0].RW {
		t.Fatalf("mount inspect: %s %v", out, err)
	}
	ok("container", "prune")
	ok("prune", "--data")
	if out := ok("volume", "inspect", "-f", "{{.Name}}", "data-soft"); strings.TrimSpace(out) != "data-soft" {
		t.Fatal(out)
	}
	if out, code := call(st.Root, "run", "--rm", "--entrypoint=missing.exe", "-v", `data-soft:${data}`, app); code == 0 {
		t.Fatalf("missing executable launched: %s", out)
	}
	if _, err := st.ReadVolume("data-soft"); err != nil {
		t.Fatal("launch failure deleted volume")
	}
	for _, source := range []string{st.Root, filepath.Dir(st.Root)} {
		if out, code := call(st.Root, "run", "--rm", "-v", source+":${data}", app); code == 0 {
			t.Fatalf("accepted protected source: %s", out)
		}
	}
	ok("volume", "prune", "--dry-run")
	if _, err := st.ReadVolume("data-soft"); err != nil {
		t.Fatal("dry run deleted volume")
	}
	ok("volume", "prune")
}

func TestDetachedMountSharingAndRecovery(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	id := "mount-share-" + fmt.Sprint(time.Now().UnixNano())
	for _, v := range []string{"1", "2"} {
		fixtureImage(t, reg, v, payload, func(c *oci.AppConfig) { c.ID = id + v })
	}
	t.Cleanup(func() {
		for _, v := range []string{"1", "2"} {
			_ = appcontainer.DeleteProfile(id + v)
		}
	})
	other, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		entries, _ := os.ReadDir(other.Path("deploy"))
		for _, entry := range entries {
			if entry.IsDir() {
				_, _ = other.DeleteDeployment("sha256:" + entry.Name())
			}
		}
	})
	source := t.TempDir()
	ok := func(home string, args ...string) string {
		t.Helper()
		out, code := call(home, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return out
	}
	waitFile := func(path string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("fixture not ready: %s", path)
	}
	for _, home := range []string{st.Root, other.Root} {
		ok(home, "pull", reg.Host()+"/app:1")
		ok(home, "pull", reg.Host()+"/app:2")
	}
	t.Cleanup(func() { _, _ = call(other.Root, "rm", "-f", "second") })
	ok(st.Root, "run", "-d", "--name", "first", "--sandbox=appcontainer", "-v", source+":${data}", reg.Host()+"/app:1", "fixture.exe", "hold-path", `$HOME\first`)
	ok(other.Root, "run", "-d", "--name", "second", "--sandbox=appcontainer", "-v", source+":${data}", reg.Host()+"/app:2", "fixture.exe", "hold-path", `$HOME\second`)
	waitFile(filepath.Join(source, "home", "first"))
	waitFile(filepath.Join(source, "home", "second"))
	ok(st.Root, "rm", "-f", "first")
	ok(st.Root, "rmi", reg.Host()+"/app:1")
	ok(st.Root, "run", "--rm", "--sandbox=low", "-v", source+":${data}", reg.Host()+"/app:2", "fixture.exe", "write-path", `$HOME\mixed`, "mixed-sandbox")
	time.Sleep(350 * time.Millisecond)
	if out := ok(other.Root, "inspect", "-f", "{{.State.Running}}", "second"); strings.TrimSpace(out) != "true" {
		t.Fatalf("other consumer lost access: %s", out)
	}
	// Kill the supervisor, then repair stale ownership without touching data.
	c, err := other.ResolveContainer("second")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := win32.CreateToolhelp32Snapshot(win32.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	var entry win32.PROCESSENTRY32W
	parent := uint32(0)
	for more := win32.Process32First(snapshot, &entry); more; more = win32.Process32Next(snapshot, &entry) {
		if entry.Th32ProcessID == c.State.Pid {
			parent = entry.Th32ParentProcessID
			break
		}
	}
	win32.CloseHandle(snapshot)
	if parent == 0 {
		t.Fatal("supervisor not found")
	}
	p, err := os.FindProcess(int(parent))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for other.IsContainerRunning(c.ID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	ok(other.Root, "repair")
	if _, err := os.ReadFile(filepath.Join(source, "home", "second")); err != nil {
		t.Fatal("recovery deleted bind data")
	}
	ok(other.Root, "rm", "second")
	// Cleanup hook accepts an already removed container.
}

func TestMountedRegistryPersistsWithAutoRemove(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	id := "mount-hive-" + fmt.Sprint(time.Now().UnixNano())
	key := `Software\QuasidockMountTests\` + id
	fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) { c.ID = id; c.Registry = []string{key} })
	app := reg.Host() + "/app:1"
	ok := func(args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return out
	}
	ok("pull", app)
	for _, target := range []string{"${data}", "${data}/registry"} {
		name := "hive-root"
		if target != "${data}" {
			name = "hive-sub"
		}
		ok("run", "--rm", "-v", name+":"+target, app, "fixture.exe", "registry-write", key, name)
		if out := ok("run", "--rm", "-v", name+":"+target, app, "fixture.exe", "registry-read", key); out != name {
			t.Fatalf("registry not persisted: %q", out)
		}
		if exists, err := regkey.Exists(win32.HKEY_CURRENT_USER, key); err != nil || exists {
			t.Fatalf("HKCU key not restored: %v %v", exists, err)
		}
	}
	ok("run", "-d", "--rm", "--name", "hive-holder", "-v", `hive-root:${data}`, app, "fixture.exe", "hold-path", `$HOME\ready`)
	if out := ok("run", "--rm", "-v", `hive-root:${data}`, app, "fixture.exe", "registry-read", key); out != "hive-root" {
		t.Fatalf("shared hive read: %q", out)
	}
	if out, code := call(st.Root, "run", "--rm", "-v", `different-hive:${data}`, app, "fixture.exe", "registry-read", key); code == 0 || !strings.Contains(out, "is using") {
		t.Fatalf("conflicting hive accepted: %s %d", out, code)
	}
	ok("stop", "-t", "0", "hive-holder")
	broken, err := st.CreateVolume("broken-hive")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(broken.Mountpoint, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	hiveFile := filepath.Join(broken.Mountpoint, "registry", "user.dat")
	if err := os.WriteFile(hiveFile, []byte("invalid hive sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := call(st.Root, "run", "--rm", "-v", `broken-hive:${data}`, app, "fixture.exe", "registry-write", key, "overwrite"); code == 0 || !strings.Contains(out, "loading registry hive") {
		t.Fatalf("corrupt hive accepted: %s %d", out, code)
	}
	if b, err := os.ReadFile(hiveFile); err != nil || string(b) != "invalid hive sentinel" {
		t.Fatalf("corrupt hive overwritten: %s %v", b, err)
	}
	// Sources reached through aliases still receive protected-path checks.
	aliasBase := t.TempDir()
	aliasData := filepath.Join(aliasBase, "data")
	_ = os.Mkdir(aliasData, 0o755)
	if err := mounts.Attach(aliasData, mounts.Mount{Source: st.Root, Destination: filepath.Join(aliasData, "store")}); err != nil {
		t.Fatal(err)
	}
	if out, code := call(st.Root, "run", "--rm", "-v", filepath.Join(aliasData, "store")+":${data}", app); code == 0 {
		t.Fatalf("protected source alias accepted: %s", out)
	}
	if err := mounts.RemoveTree(aliasData); err != nil {
		t.Fatal(err)
	}
}
