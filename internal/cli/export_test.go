package cli

import (
	"archive/tar"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestExportOptions(t *testing.T) {
	for _, args := range [][]string{{"source"}, {"--", "source"}, {"-p", "source"}, {"--pause=true", "source"}} {
		opts, err := parseExportOptions(args)
		if err != nil || opts.Container != "source" || !opts.Pause || opts.output != "-" {
			t.Fatalf("%v: %+v %v", args, opts, err)
		}
	}
	for _, args := range [][]string{{"-o", "out.tar", "--pause=false", "source"}, {"--output=out.tar", "-p=false", "source"}} {
		opts, err := parseExportOptions(args)
		if err != nil || opts.Container != "source" || opts.Pause || opts.output != "out.tar" {
			t.Fatalf("%v: %+v %v", args, opts, err)
		}
	}
	for _, args := range [][]string{nil, {""}, {"one", "two"}, {"source", "-o", "out"}, {"-o"}, {"--output=", "source"}, {"--pause=maybe", "source"}, {"--unknown", "source"}} {
		if _, err := parseExportOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, arg := range []string{"-h", "--help"} {
		if _, err := parseExportOptions([]string{arg}); !errors.Is(err, flag.ErrHelp) {
			t.Fatal("missing help", err)
		}
	}
}

func exportMembers(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		result[h.Name], err = io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportCommands(t *testing.T) {
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	reg := testregistry.New()
	digest, _ := fixtureImage(t, reg, "1", []byte("\x00payload\xff"))
	if _, err := imageCommand(ctx, []string{"pull", reg.Host() + "/app:1"}); err != nil {
		reg.Close()
		t.Fatal(err)
	}
	fixtureImage(t, reg, "2", []byte("different default image"))
	if _, err := imageCommand(ctx, []string{"pull", reg.Host() + "/app:2"}); err != nil {
		reg.Close()
		t.Fatal(err)
	}
	// No registry is available during any export.
	reg.Close()
	c, err := ctx.Store.CreateContainer("export-source", store.ContainerRecord{Image: digest})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.DataDir, "note"), []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--verbose", "export", c.Name}, {"container", "export", "-o", "-", c.ShortID()}} {
		out.Reset()
		if code, err := Dispatch(ctx, args); code != 0 || err != nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
		files := exportMembers(t, out.Bytes())
		if string(files["app/fixture.exe"]) != "\x00payload\xff" || string(files["data/note"]) != "private" {
			t.Fatal("stdout was not a clean filesystem tar", files)
		}
	}
	path := filepath.Join(t.TempDir(), "export.tar")
	out.Reset()
	if code, err := Dispatch(ctx, []string{"export", "--output=" + path, c.ID}); code != 0 || err != nil || out.Len() != 0 {
		t.Fatal("file export output", code, err, out.String())
	}
	for _, args := range [][]string{{"export", "--help"}, {"container", "export", "-h"}} {
		out.Reset()
		if code, err := Dispatch(ctx, args); code != 0 || err != nil || !strings.Contains(out.String(), exportUsage) {
			t.Fatal("export help", code, err, out.String())
		}
	}
}

func TestExportLiveStateAndRegistry(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	key := `Software\QuasidockExportTest` + fmt.Sprint(time.Now().UnixNano())
	t.Cleanup(func() { root := regkey.Key{Handle: win32.HKEY_CURRENT_USER}; _ = root.DeleteTree(key) })
	fixtureImage(t, reg, "1", payload, func(cfg *oci.AppConfig) { cfg.Registry = []string{key} })
	must := func(args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return out
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "export.tar")
	must("run", "-d", "--name", "live-export", reg.Host()+"/app:1", "fixture.exe", "state-hold", `$HOME\count`, key)
	c, err := st.ResolveContainer("live-export")
	if err != nil {
		t.Fatal(err)
	}
	awaitPauseCondition(t, "registry fixture did not start", func() bool { return store.Exists(filepath.Join(c.DataDir, "home", "count")) })
	readArchive := func() map[string][]byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return exportMembers(t, data)
	}
	checkHive := func() {
		t.Helper()
		files := readArchive()
		if string(files["data/home/count"]) != "1" || len(files["data/registry/user.dat"]) == 0 {
			t.Fatal("private state missing")
		}
		hiveDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(hiveDir, "user.dat"), files["data/registry/user.dat"], 0o644); err != nil {
			t.Fatal(err)
		}
		hive, status := win32.RegLoadAppKey(filepath.Join(hiveDir, "user.dat"), win32.KEY_READ)
		if status != 0 {
			t.Fatalf("loading captured hive: %d", status)
		}
		defer win32.RegCloseKey(hive)
		k, err := regkey.Open(hive, key, win32.KEY_READ)
		if err != nil || k == nil {
			t.Fatal("captured hive key missing", err)
		}
		defer k.Close()
		value, err := k.GetString("Value")
		if err != nil || value == nil || *value != "1" {
			t.Fatal("live HKCU value missing", value, err)
		}
	}
	must("export", "-o", path, c.ID)
	checkHive()
	state, _ := st.ReadContainer(c.ID)
	if !state.State.Running || state.State.Paused {
		t.Fatal("export did not resume running source", state.State)
	}
	protocol := state.State.CommitProtocol
	if _, err := st.UpdateContainer(c.ID, func(c *store.ContainerRecord) error { c.State.CommitProtocol = 0; return nil }); err != nil {
		t.Fatal(err)
	}
	if out, code := call(st.Root, "export", "-o", path, c.ID); code == 0 || !strings.Contains(out, "--pause=false") {
		t.Fatal("older supervisor did not require explicit best-effort capture", code, out)
	}
	must("export", "--pause=false", "-o", path, c.ID)
	checkHive()
	if _, err := st.UpdateContainer(c.ID, func(c *store.ContainerRecord) error { c.State.CommitProtocol = protocol; return nil }); err != nil {
		t.Fatal(err)
	}
	// Capture errors restore the source and leave an existing archive intact.
	original, _ := os.ReadFile(path)
	link := mounts.Mount{Source: t.TempDir(), Destination: filepath.Join(c.DataDir, "unexpected")}
	if err := mounts.Attach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	if out, code := call(st.Root, "export", "-o", path, c.ID); code == 0 {
		t.Fatal("export accepted an unexpected link", out)
	}
	if err := mounts.Detach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	state, _ = st.ReadContainer(c.ID)
	if !state.State.Running || state.State.Paused {
		t.Fatal("failed export did not restore source", state.State)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
		t.Fatal("failed capture replaced archive", err)
	}
	must("pause", c.ID)
	for _, pause := range []string{"--pause=true", "--pause=false"} {
		must("container", "export", "-o", path, pause, c.ID)
		checkHive()
		state, _ = st.ReadContainer(c.ID)
		if !state.State.Paused {
			t.Fatal("export resumed manually paused source")
		}
	}
	must("kill", c.ID)
	must("export", "-o", path, c.ID)
	checkHive()
	must("create", "--name", "created-export", reg.Host()+"/app:1")
	must("export", "-o", path, "created-export")
	if _, ok := readArchive()["data/registry/user.dat"]; ok {
		t.Fatal("unstarted source captured host HKCU")
	}
	for i, target := range []string{"${data}", "${data}/hidden", "${data}/registry", "${data}/registry/child"} {
		name := fmt.Sprintf("mounted-export-%d", i)
		source := "export-volume"
		if i%2 == 0 {
			source = t.TempDir()
		}
		must("create", "--name", name, "-v", source+":"+target, reg.Host()+"/app:1")
		mounted, err := st.ResolveContainer(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(mounted.DataDir, "hidden"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mounted.DataDir, "hidden", "underlying"), []byte("hidden private data"), 0o644); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(target, "/child") {
			if out, code := call(st.Root, "export", "-o", path, name); code == 0 || !strings.Contains(out, "mounts inside its hive") {
				t.Fatal("export accepted a partial registry mount", code, out)
			}
			continue
		}
		must("export", "-o", path, name)
		for member := range readArchive() {
			if member != "data/" && strings.HasPrefix(member, "data/") && (target == "${data}" || strings.HasPrefix(member, "data/registry/") || target == "${data}/hidden" && strings.HasPrefix(member, "data/hidden/")) {
				t.Fatal("captured mounted data", member)
			}
		}
	}
}

func TestExportInterruptBlockedOutput(t *testing.T) {
	binary, payload, buildDir := buildContainerTestBinaries(t)
	ctx, _ := fixtureCLI(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	digest, _ := fixtureImage(t, reg, "1", payload)
	if _, err := imageCommand(ctx, []string{"pull", reg.Host() + "/app:1"}); err != nil {
		t.Fatal(err)
	}
	c, err := ctx.Store.CreateContainer("export-interrupt", store.ContainerRecord{Image: digest})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	var output pauseOutput
	console, input := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), []string{
		"console-run-check-mode-output-pipe", binary, "export", c.ID,
	}, &output)
	awaitPauseCondition(t, "export did not reach output", func() bool {
		staged, err := filepath.Glob(ctx.Store.Path("tmp", "container-export-*", "root", "app", "fixture.exe"))
		return err == nil && len(staged) == 1 && !store.IsLocked(ctx.Store.ContainerOperationLockPath(c.ID))
	})
	if _, err := win32.WriteFile(input, []byte{3}); err != nil {
		t.Fatal(err)
	}
	if win32.WaitForSingleObject(console, 5000) != win32.WAIT_OBJECT_0 {
		t.Fatal("Ctrl+C did not interrupt blocked export output")
	}
	if code, err := win32.GetExitCodeProcess(console); err != nil || code != 130 {
		output.mu.Lock()
		defer output.mu.Unlock()
		t.Fatalf("interrupted exit: %d %v; output: %s", code, err, output.b.String())
	}
	if store.IsLocked(ctx.Store.Path("lock")) || store.IsLocked(ctx.Store.ContainerOperationLockPath(c.ID)) {
		t.Fatal("interrupted export retained locks")
	}
	if staged, err := filepath.Glob(ctx.Store.Path("tmp", "container-export-*")); err != nil || len(staged) != 0 {
		t.Fatal("interrupted export leaked staging", staged, err)
	}
}
