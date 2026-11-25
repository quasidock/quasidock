package cli

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestCpCommands(t *testing.T) {
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	c, err := ctx.Store.CreateContainer("copy-example", store.ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	host := t.TempDir()
	source := filepath.Join(host, "input")
	if err := os.WriteFile(source, []byte("\x00payload\xff"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"cp", source, "copy-example:/file"},
		{"container", "cp", c.ShortID() + ":file", filepath.Join(host, "out")},
	} {
		out.Reset()
		if code, err := Dispatch(ctx, args); code != 0 || err != nil || out.Len() != 0 {
			t.Fatalf("%v: %d %v %q", args, code, err, out.String())
		}
	}
	if data, err := os.ReadFile(filepath.Join(host, "out")); err != nil || string(data) != "\x00payload\xff" {
		t.Fatal(string(data), err)
	}
	out.Reset()
	if code, err := Dispatch(ctx, []string{"--verbose", "container", "cp", c.ID + ":/file", "-"}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	r := tar.NewReader(bytes.NewReader(out.Bytes()))
	h, err := r.Next()
	if err != nil || h.Name != "file" {
		t.Fatal("stdout is not a clean tar stream", h, err)
	}
	content, err := io.ReadAll(r)
	if err != nil || string(content) != "\x00payload\xff" {
		t.Fatal(content, err)
	}
	input, err := os.CreateTemp(host, "input-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	if _, err := input.Write(out.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(c.DataDir, "restore"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = previous }()
	out.Reset()
	if code, err := Dispatch(ctx, []string{"cp", "-", "copy-example:/restore"}); code != 0 || err != nil || out.Len() != 0 {
		t.Fatal(code, err, out.String())
	}
	if data, err := os.ReadFile(filepath.Join(c.DataDir, "restore", "file")); err != nil || string(data) != "\x00payload\xff" {
		t.Fatal(data, err)
	}
	for _, args := range [][]string{{"cp", "--help"}, {"container", "cp", "-h"}} {
		out.Reset()
		if code, err := Dispatch(ctx, args); code != 0 || err != nil || !strings.Contains(out.String(), "CONTAINER:") {
			t.Fatal(code, err, out.String())
		}
	}
	for _, args := range [][]string{{}, {source}, {source, source}, {"copy-example:/file", "copy-example:/other"}, {"-a", source, "copy-example:/file"}, {"--unknown", "copy-example:/file"}, {"copy-example:", source}, {"missing:/file", source}} {
		out.Reset()
		if code, err := cpCommand(ctx, args); code != 1 || err == nil || out.Len() != 0 {
			t.Fatalf("invalid %v: %d %v %q", args, code, err, out.String())
		}
	}
	t.Chdir(host)
	if err := os.WriteFile("-filename", []byte("escaped"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := cpCommand(ctx, []string{"--", "-filename", "copy-example:/escaped"}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
}

func TestCpContainerLifecycle(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	fixtureImage(t, reg, "1", payload)
	must := func(args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return out
	}
	must("create", "--name", "copy-live", "-v", "copy-work:${data}/work", reg.Host()+"/app:1", "fixture.exe", "signal-hold", `$HOME\ready`, "ignore")
	host := t.TempDir()
	source := filepath.Join(host, "input")
	if err := os.WriteFile(source, []byte("copy payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	check := func(status string) {
		t.Helper()
		before, err := st.ResolveContainer("copy-live")
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []string{"/file", "/work/file"} {
			if out := must("cp", source, "copy-live:"+target); out != "" {
				t.Fatal("copy printed output", out)
			}
			out := filepath.Join(host, status+strings.ReplaceAll(target, "/", "-"))
			must("container", "cp", before.ShortID()+":"+target, out)
			if data, err := os.ReadFile(out); err != nil || string(data) != "copy payload" {
				t.Fatal(data, err)
			}
		}
		after, err := st.ResolveContainer("copy-live")
		if err != nil || after.State != before.State || after.State.Status != status {
			t.Fatalf("copy changed %s state: %+v %v", status, after, err)
		}
	}
	check("created")
	must("start", "copy-live")
	c, err := st.ResolveContainer("copy-live")
	if err != nil {
		t.Fatal(err)
	}
	awaitPauseCondition(t, "container did not start", func() bool { return store.Exists(filepath.Join(c.DataDir, "home", "ready")) })
	check("running")
	if got := must("exec", "copy-live", "fixture.exe", "read-path", `$HOME\..\file`); got != "copy payload" {
		t.Fatal("container cannot read copied file", got)
	}
	must("pause", "copy-live")
	check("paused")
	must("kill", "copy-live")
	check("exited")
}

func TestCpSandboxPermissions(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	id := fmt.Sprintf("copy-sandbox-%d", time.Now().UnixNano())
	fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) { c.ID = id })
	t.Cleanup(func() { _ = appcontainer.DeleteProfile(id) })
	must := func(args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return out
	}
	host := t.TempDir()
	input := filepath.Join(host, "file")
	if err := os.WriteFile(input, []byte("inherited access"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sandbox := range []string{"low", "appcontainer"} {
		t.Run(sandbox, func(t *testing.T) {
			name := "copy-" + sandbox
			must("run", "-d", "--name", name, "--sandbox="+sandbox, "-v", name+":${data}/work", reg.Host()+"/app:1", "fixture.exe", "signal-hold", `$HOME\ready`, "ignore")
			for _, target := range []string{"/file", "/work/file"} {
				for i := 0; i < 2; i++ {
					must("cp", input, name+":"+target)
					if got := must("exec", name, "fixture.exe", "read-path", `$HOME\..`+strings.ReplaceAll(target, "/", `\`)); got != "inherited access" {
						t.Fatal("container cannot read replacement", got)
					}
				}
			}
			must("rm", "-f", name)
		})
	}
}

func TestCpInterruptBlockedInput(t *testing.T) {
	binary, _, buildDir := buildContainerTestBinaries(t)
	for _, mode := range []string{"console-run-check-mode", "console-run-check-mode-pipe"} {
		t.Run(mode, func(t *testing.T) {
			checkCpInterruptBlockedInput(t, binary, buildDir, mode)
		})
	}
}

func checkCpInterruptBlockedInput(t *testing.T, binary, buildDir, mode string) {
	t.Helper()
	ctx, _ := fixtureCLI(t)
	c, err := ctx.Store.CreateContainer("copy-interrupt", store.ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	var output pauseOutput
	console, input := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), []string{
		mode, binary, "cp", "-", "copy-interrupt:/",
	}, &output)
	awaitPauseCondition(t, "copy did not begin staging stdin", func() bool {
		staged, err := filepath.Glob(ctx.Store.Path("tmp", "container-copy-*"))
		if err != nil || len(staged) != 1 {
			return false
		}
		return mode != "console-run-check-mode-pipe" || store.Exists(filepath.Join(staged[0], "pending"))
	})
	if _, err := win32.WriteFile(input, []byte{3}); err != nil {
		t.Fatal(err)
	}
	if win32.WaitForSingleObject(console, 5000) != win32.WAIT_OBJECT_0 {
		t.Fatal("Ctrl+C did not interrupt stalled tar input")
	}
	if code, err := win32.GetExitCodeProcess(console); err != nil || code != 130 {
		output.mu.Lock()
		defer output.mu.Unlock()
		t.Fatalf("interrupted exit code: %d %v; output: %s", code, err, output.b.String())
	}
	if store.IsLocked(ctx.Store.ContainerOperationLockPath(c.ID)) {
		t.Fatal("interrupted copy retained its lock")
	}
	if files, err := os.ReadDir(c.DataDir); err != nil || len(files) != 0 {
		t.Fatal("interrupted import modified data", files, err)
	}
	if staged, err := filepath.Glob(ctx.Store.Path("tmp", "container-copy-*")); err != nil || len(staged) != 0 {
		t.Fatal("interrupted import leaked staging", staged, err)
	}
}
