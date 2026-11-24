package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/builder"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestCommitOptions(t *testing.T) {
	opts, err := parseCommitOptions([]string{"-p=false", "-a", "author", "--message=message", "-c", `CMD ["app.exe"]`, "container", "image:v1"})
	if err != nil || opts.Pause || opts.Author != "author" || opts.Message != "message" || len(opts.Changes) != 1 || opts.Container != "container" || opts.Target != "image:v1" {
		t.Fatalf("options: %+v %v", opts, err)
	}
	if opts, err := parseCommitOptions([]string{"container"}); err != nil || !opts.Pause {
		t.Fatalf("default pause: %+v %v", opts, err)
	}
	for _, args := range [][]string{nil, {""}, {"-"}, {"-x"}, {"--unknown"}, {"--pause=perhaps", "container"}, {"-a"}, {"container", "--pause=false"}, {"container", "image", "extra"}} {
		if _, err := parseCommitOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestCommitRoundTripAndDiff(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	parent, _ := fixtureImage(t, reg, "1", payload)
	app := reg.Host() + "/app:1"
	must := func(home string, args ...string) string {
		t.Helper()
		out, code := call(home, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return strings.TrimSpace(out)
	}
	bind := t.TempDir()
	if err := os.WriteFile(filepath.Join(bind, "secret.txt"), []byte("mounted secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOVE_ME", "")
	if err := os.Unsetenv("REMOVE_ME"); err != nil {
		t.Fatal(err)
	}
	must(st.Root, "create", "--name", "source", "-e", "SAVED=original", "-e", "REMOVE_ME", "--mount", "type=bind,source="+bind+",target=${data}/hidden", app)
	source, err := st.ResolveContainer("source")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"home", "empty", "hidden"} {
		if err := os.MkdirAll(filepath.Join(source.DataDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{"home/note.txt": "original", "removed.txt": "delete me", "hidden/underlying.txt": "exclude this too"} {
		if err := os.WriteFile(filepath.Join(source.DataDir, filepath.FromSlash(name)), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	target := reg.Host() + "/captured:saved"
	digest := must(st.Root, "commit", "-a", "test author", "-m", "saved state", "-c", `CMD ["log","committed"]`, "-c", `LABEL purpose="round trip"`, "source", target)
	if oci.DigestHex(digest) == "" {
		t.Fatalf("digest output: %q", digest)
	}
	image, err := st.ReadImage("captured", digest)
	if err != nil {
		t.Fatal(err)
	}
	if image.Config.ID != "captured" || image.Config.Version != "saved" || image.Config.Env["SAVED"] != "original" || !reflect.DeepEqual(image.Config.UnsetEnv, []string{"REMOVE_ME"}) || image.Config.Labels["purpose"] != "round trip" || image.Manifest.Annotations["io.quasidock.commit.parent"] != parent || image.Manifest.Annotations["org.opencontainers.image.authors"] != "test author" || image.Manifest.Annotations["io.quasidock.commit.message"] != "saved state" {
		t.Fatalf("image: %+v", image)
	}
	for _, name := range []string{"copy-one", "copy-two"} {
		must(st.Root, "create", "--name", name, target)
	}
	one, _ := st.ResolveContainer("copy-one")
	two, _ := st.ResolveContainer("copy-two")
	if diff := must(st.Root, "diff", "copy-one"); diff != "" {
		t.Fatalf("seed is not baseline: %s", diff)
	}
	if _, err := os.Stat(filepath.Join(one.DataDir, "hidden")); !os.IsNotExist(err) {
		t.Fatalf("mount destination captured: %v", err)
	}
	if _, err := os.Stat(filepath.Join(one.DataDir, "empty")); err != nil {
		t.Fatal("empty directory missing", err)
	}
	if err := os.WriteFile(filepath.Join(one.DataDir, "home", "note.txt"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(one.DataDir, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(one.DataDir, "added.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if diff := must(st.Root, "container", "diff", "copy-one"); diff != "A /added.txt\nC /home/note.txt\nD /removed.txt" {
		t.Fatalf("changes: %q", diff)
	}
	if data, err := os.ReadFile(filepath.Join(two.DataDir, "home", "note.txt")); err != nil || string(data) != "original" {
		t.Fatalf("copies share writable bytes: %s %v", data, err)
	}
	unnamed := must(st.Root, "container", "commit", "copy-one")
	second, err := st.ReadImage("second", unnamed)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Manifest.Layers) != 2 || oci.OwnLayer(&second.Manifest).Digest != oci.OwnLayer(&image.Manifest).Digest || second.Config.DataLayer == image.Config.DataLayer {
		t.Fatal("recommit did not replace just the data layer")
	}
	ref, _ := st.ResolveLocalImage("", target)
	if ref.Manifest != digest {
		t.Fatal("unnamed commit moved named reference")
	}
	must(st.Root, "create", "--name", "unnamed", unnamed)
	if diff := must(st.Root, "diff", "unnamed"); diff != "" {
		t.Fatalf("recommit baseline: %s", diff)
	}
	// New mounts mask snapshot subtrees without copying into external storage.
	must(st.Root, "create", "--name", "masked", "--mount", "type=bind,source="+bind+",target=${data}/home", target)
	masked, _ := st.ResolveContainer("masked")
	if _, err := os.Stat(filepath.Join(masked.DataDir, "home", "note.txt")); !os.IsNotExist(err) {
		t.Fatalf("seed copied under mount: %v", err)
	}
	if diff := must(st.Root, "diff", "masked"); diff != "" {
		t.Fatalf("mounted baseline was not excluded: %s", diff)
	}
	must(st.Root, "rm", "source")
	must(st.Root, "rmi", parent)
	if diff := must(st.Root, "diff", "copy-two"); diff != "" {
		t.Fatalf("source operations changed other snapshot: %s", diff)
	}
	must(st.Root, "prune")
	must(st.Root, "push", target)
	before := reg.Count("/v2/")
	if out, code := call(st.Root, "push", "--sources", target); code == 0 || !strings.Contains(out, "no reproducible recipe") {
		t.Fatalf("sources: %d %s", code, out)
	}
	if reg.Count("/v2/") != before {
		t.Fatal("sources rejection made registry requests")
	}
	for _, kind := range []builder.DependencyKind{builder.DependencyRuntime, builder.DependencySDK, builder.DependencySource} {
		_, err := builder.ResolveDependency(&context.Context{Store: st}, "quasidock.json", kind, "snapshot", target+"@"+digest)
		if err == nil || !strings.Contains(err.Error(), "not supported as recipe dependencies") {
			t.Fatalf("recipe dependency %s: %v", kind, err)
		}
	}
	fresh, _ := fixtureCLI(t)
	must(fresh.Store.Root, "pull", target)
	if out := must(fresh.Store.Root, "run", "--rm", target); out != "committed" {
		t.Fatalf("fresh run: %q", out)
	}
	must(st.Root, "create", "--name", "after-prune", target)
	if diff := must(st.Root, "diff", "after-prune"); diff != "" {
		t.Fatalf("prune damaged snapshot: %s", diff)
	}
	// V4 startup supports explicit system shells and executables saved in data.
	must(st.Root, "commit", "-c", `ENTRYPOINT ["cmd.exe"]`, "-c", `CMD ["/d","/c","echo","system-shell"]`, "after-prune", reg.Host()+"/shell-copy:1")
	if out := must(st.Root, "run", "--rm", reg.Host()+"/shell-copy:1"); out != "system-shell" {
		t.Fatalf("system shell: %q", out)
	}
	if out := must(st.Root, "run", "--rm", reg.Host()+"/shell-copy:1", "shell-copy", "/d", "/c", "echo", "alias"); out != "alias" {
		t.Fatalf("system command alias: %q", out)
	}
	afterPrune, _ := st.ResolveContainer("after-prune")
	if err := os.WriteFile(filepath.Join(afterPrune.DataDir, "runner.exe"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	must(st.Root, "commit", "-c", `ENTRYPOINT []`, "-c", `CMD ["${data}/runner.exe","log","data-command"]`, "-c", `WORKDIR ${data}/home`, "after-prune", reg.Host()+"/data-copy:1")
	if out := must(st.Root, "run", "--rm", reg.Host()+"/data-copy:1"); out != "data-command" {
		t.Fatalf("data command: %q", out)
	}
	if out := must(st.Root, "run", "--rm", reg.Host()+"/data-copy:1", "data-copy", "env", "SAVED", "REMOVE_ME"); out != "SAVED=original:true\nREMOVE_ME=:false" {
		t.Fatalf("saved env: %q", out)
	}
}

func TestCommitLiveFailureAndPause(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	fixtureImage(t, reg, "1", payload)
	must := func(args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return strings.TrimSpace(out)
	}
	must("run", "-d", "--name", "live", reg.Host()+"/app:1", "fixture.exe", "heartbeat", `$HOME\ready`, "ignore")
	c, _ := st.ResolveContainer("live")
	ticks := filepath.Join(c.DataDir, "home", "ready.ticks")
	awaitPauseCondition(t, "heartbeat did not start", func() bool { info, err := os.Stat(ticks); return err == nil && info.Size() > 0 })
	outside := t.TempDir()
	link := mounts.Mount{Source: outside, Destination: filepath.Join(c.DataDir, "unexpected")}
	if err := mounts.Attach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounts.Detach(c.DataDir, link) })
	fail := func(target string) {
		t.Helper()
		out, code := call(st.Root, "commit", "live", target)
		if code == 0 {
			t.Fatalf("capture unexpectedly succeeded: %s", out)
		}
		state, _ := st.ReadContainer(c.ID)
		if !state.State.Running || state.State.Paused {
			t.Fatalf("capture failed to restore state: %+v", state.State)
		}
		if _, err := st.ResolveLocalImage("", target); err == nil {
			t.Fatal("failed capture registered an image")
		}
	}
	fail(reg.Host() + "/failed:link")
	if err := mounts.Detach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(c.DataDir, "blocked")
	if err := os.WriteFile(blocked, []byte("locked"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := win32.CreateFile(blocked, win32.GENERIC_READ, 0, nil, win32.OPEN_EXISTING, win32.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	blockedFile := os.NewFile(uintptr(h), blocked)
	defer func() { _ = blockedFile.Close() }()
	fail(reg.Host() + "/failed:locked")
	if err := blockedFile.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(ticks)
	must("commit", "live", reg.Host()+"/live-copy:running")
	awaitPauseCondition(t, "heartbeat did not resume", func() bool { after, _ := os.Stat(ticks); return after.Size() > before.Size() })
	must("pause", "live")
	must("commit", "live", reg.Host()+"/live-copy:paused")
	state, _ := st.ReadContainer(c.ID)
	if !state.State.Paused {
		t.Fatal("commit resumed a manually paused source")
	}
	must("commit", "--pause=false", "live", reg.Host()+"/live-copy:unfrozen")
	state, _ = st.ReadContainer(c.ID)
	if !state.State.Paused {
		t.Fatal("--pause=false changed pause state")
	}
	must("kill", "live")
	must("commit", "live", reg.Host()+"/live-copy:exited")
}

func TestCommitPrivateRegistry(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	key := `Software\QuasidockCommitTest` + fmt.Sprint(time.Now().UnixNano())
	t.Cleanup(func() { root := regkey.Key{Handle: win32.HKEY_CURRENT_USER}; _ = root.DeleteTree(key) })
	host, err := regkey.Create(win32.HKEY_CURRENT_USER, key)
	if err != nil {
		t.Fatal(err)
	}
	valueName := "Value"
	if err := host.SetString(&valueName, "host"); err != nil {
		t.Fatal(err)
	}
	host.Close()
	fixtureImage(t, reg, "1", payload, func(cfg *oci.AppConfig) { cfg.Registry = []string{key} })
	must := func(args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return strings.TrimSpace(out)
	}
	for _, mounted := range []bool{false, true} {
		name := fmt.Sprintf("registry-%t", mounted)
		args := []string{"run", "-d", "--name", name}
		if mounted {
			args = append(args, "-v", `registry-volume:${data}/registry`)
		}
		args = append(args, reg.Host()+"/app:1", "fixture.exe", "state-hold", `$HOME\count`, key)
		must(args...)
		c, _ := st.ResolveContainer(name)
		awaitPauseCondition(t, "registry app did not initialize", func() bool { _, err := os.Stat(filepath.Join(c.DataDir, "home", "count")); return err == nil })
		target := reg.Host() + "/registry-copy:" + fmt.Sprint(mounted)
		digest := must("commit", name, target)
		must("kill", name)
		if mounted {
			seed, err := st.DataSeed(digest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(seed, "registry")); !os.IsNotExist(err) {
				t.Fatalf("mounted registry was captured: %v", err)
			}
			continue
		}
		// Stopped capture reads only the saved private hive, not the host key
		// restored when the original session ended.
		must("commit", name, reg.Host()+"/registry-copy:stopped")
		target = reg.Host() + "/registry-copy:stopped"
		must("run", "-d", "--name", "registry-clone", target)
		clone, _ := st.ResolveContainer("registry-clone")
		awaitPauseCondition(t, "clone did not restore registry", func() bool {
			data, err := os.ReadFile(filepath.Join(clone.DataDir, "home", "count"))
			return err == nil && string(data) == "2"
		})
		if logs := must("logs", "registry-clone"); !strings.Contains(logs, "state file=2 registry=2") {
			t.Fatalf("registry was not captured from HKCU: %s", logs)
		}
		must("kill", "registry-clone")
	}
	must("create", "--name", "registry-created", reg.Host()+"/app:1")
	created := must("commit", "registry-created", reg.Host()+"/registry-copy:created")
	seed, err := st.DataSeed(created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(seed, "registry")); !os.IsNotExist(err) {
		t.Fatalf("unstarted container captured host registry: %v", err)
	}
	restored, err := regkey.Open(win32.HKEY_CURRENT_USER, key, win32.KEY_READ)
	if err != nil || restored == nil {
		t.Fatalf("host key missing: %v", err)
	}
	defer restored.Close()
	value, err := restored.GetString(valueName)
	if err != nil || value == nil || *value != "host" {
		t.Fatalf("host state changed: %v %v", value, err)
	}
}
