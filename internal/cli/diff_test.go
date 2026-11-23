package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

type diffErrorWriter struct{ err error }

func (w diffErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestDiffCommands(t *testing.T) {
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	c, err := ctx.Store.CreateContainer("example", store.ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.DataDir, "example.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"diff", "example"}, {"container", "diff", c.ShortID()}, {"diff", c.ID}} {
		out.Reset()
		if code, err := Dispatch(ctx, args); code != 0 || err != nil || out.String() != "A /example.txt\n" {
			t.Fatalf("%v: %d %v %q", args, code, err, out.String())
		}
	}
	for _, args := range [][]string{nil, {""}, {"--help"}, {"--format=json", "example"}, {"example", "other"}, {"example", "--unknown"}} {
		out.Reset()
		if code, err := diffCommand(ctx, args); code != 1 || err == nil || !strings.Contains(err.Error(), "usage:") || out.Len() != 0 {
			t.Fatalf("invalid %v: %d %v %q", args, code, err, out.String())
		}
	}
	out.Reset()
	if code, err := diffCommand(ctx, []string{"missing"}); code != 1 || !errors.Is(err, store.ErrContainerNotFound) || out.Len() != 0 {
		t.Fatalf("unknown container: %d %v %q", code, err, out.String())
	}
	writeErr := errors.New("output closed")
	ctx.Stdout = diffErrorWriter{writeErr}
	if code, err := diffCommand(ctx, []string{"example"}); code != 1 || !errors.Is(err, writeErr) {
		t.Fatalf("output failure: %d %v", code, err)
	}
	ctx.Stdout = out
	if err := os.Remove(filepath.Join(c.DataDir, "example.txt")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code, err := diffCommand(ctx, []string{"example"}); code != 0 || err != nil || out.Len() != 0 {
		t.Fatalf("empty diff: %d %v %q", code, err, out.String())
	}
}

func TestDiffScanErrorHasNoOutput(t *testing.T) {
	for _, failure := range []string{"sharing", "permissions"} {
		t.Run(failure, func(t *testing.T) {
			testDiffScanError(t, failure)
		})
	}
}

func testDiffScanError(t *testing.T, failure string) {
	t.Helper()
	ctx, out := fixtureCLI(t)
	c, err := ctx.Store.CreateContainer("example", store.ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.DataDir, "a.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(c.DataDir, "z-blocked")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if failure == "sharing" {
		h, err := win32.CreateFile(blocked, win32.GENERIC_READ, 0, nil, win32.OPEN_EXISTING, win32.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer win32.CloseHandle(h)
	} else {
		_, _, oldACL, _, sd, err := win32.GetNamedSecurityInfo(blocked, win32.SE_FILE_OBJECT, win32.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		defer win32.LocalFree(sd)
		sid, err := acl.CurrentUserSid()
		if err != nil {
			t.Fatal(err)
		}
		deniedACL, err := win32.SetEntriesInAcl([]win32.EXPLICIT_ACCESS_W{{
			GrfAccessPermissions: win32.FILE_LIST_DIRECTORY,
			GrfAccessMode:        win32.DENY_ACCESS,
			Trustee:              win32.TRUSTEE_W{TrusteeForm: win32.TRUSTEE_IS_SID, PtstrName: sid},
		}}, oldACL)
		if err != nil {
			t.Fatal(err)
		}
		defer win32.LocalFree(uintptr(unsafe.Pointer(deniedACL)))
		if err := win32.SetNamedSecurityInfo(blocked, win32.SE_FILE_OBJECT, win32.DACL_SECURITY_INFORMATION, 0, 0, deniedACL, nil); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := win32.SetNamedSecurityInfo(blocked, win32.SE_FILE_OBJECT, win32.DACL_SECURITY_INFORMATION, 0, 0, oldACL, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	if code, err := diffCommand(ctx, []string{"example"}); code != 1 || err == nil || out.Len() != 0 {
		t.Fatalf("partial scan output: %d %v %q", code, err, out.String())
	}
}

func TestDiffContainerLifecycle(t *testing.T) {
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
	must("create", "--name", "diff-live", reg.Host()+"/app:1", "fixture.exe", "signal-hold", `$HOME\ready`, "ignore")
	if out := must("diff", "diff-live"); out != "" {
		t.Fatalf("created container has additions: %q", out)
	}
	must("start", "diff-live")
	c, err := st.ResolveContainer("diff-live")
	if err != nil {
		t.Fatal(err)
	}
	awaitPauseCondition(t, "container did not write ready file", func() bool {
		_, err := os.Stat(filepath.Join(c.DataDir, "home", "ready"))
		return err == nil
	})
	check := func(status string, args ...string) {
		t.Helper()
		out := must(args...)
		if !strings.Contains(out, "A /home/ready\n") || strings.Contains(out, "container.log") {
			t.Fatalf("%s diff: %q", status, out)
		}
		c, err := st.ResolveContainer("diff-live")
		if err != nil || c.State.Status != status {
			t.Fatalf("diff changed %s state: %+v %v", status, c, err)
		}
	}
	check("running", "diff", "diff-live")
	must("pause", "diff-live")
	check("paused", "container", "diff", c.ShortID())
	must("unpause", "diff-live")
	must("kill", "diff-live")
	check("exited", "diff", c.ID)
}
