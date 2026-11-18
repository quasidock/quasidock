package cli

import (
	"fmt"
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

func TestExecSharesMountsRegistryAndSandbox(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	id := fmt.Sprintf("exec-sandbox-%d", time.Now().UnixNano())
	key := `Software\QuasidockExec\` + id
	fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) { c.ID = id; c.Registry = []string{key} })
	t.Cleanup(func() { _ = appcontainer.DeleteProfile(id) })
	app := reg.Host() + "/app:1"
	must := func(t *testing.T, args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return out
	}
	must(t, "pull", app)
	for _, sandbox := range []string{"soft", "low", "appcontainer"} {
		t.Run(sandbox, func(t *testing.T) {
			name := "exec-" + sandbox
			outside := filepath.Join(t.TempDir(), "outside.txt")
			must(t, "run", "-d", "--rm", "--name", name, "--sandbox="+sandbox, "-v", name+":${data}/workspace", app, "fixture.exe", "state-hold", `$HOME\..\workspace\counter`, key)
			rec, err := st.ResolveContainer(name)
			if err != nil {
				t.Fatal(err)
			}
			source := rec.Mounts[0].Source
			waitFile := func(path string) string {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
						return string(data)
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatalf("fixture not ready: %s", path)
				return ""
			}
			waitFile(filepath.Join(source, "counter"))
			if got := must(t, "exec", name, "fixture.exe", "registry-read", key); got != "1" {
				t.Fatalf("registry wasn't shared: %q", got)
			}
			must(t, "exec", name, "fixture.exe", "registry-write", key, "41")
			if got := must(t, "exec", name, "fixture.exe", "registry-read", key); got != "41" {
				t.Fatalf("registry write lost: %q", got)
			}
			must(t, "exec", "-w", "${data}/workspace", name, "fixture.exe", "write-path", "exec.txt", sandbox)
			if got := waitFile(filepath.Join(source, "exec.txt")); got != sandbox {
				t.Fatal(got)
			}
			_, code := call(st.Root, "exec", name, "fixture.exe", "write-path", outside, "permission probe")
			if (code == 0) != (sandbox == "soft") {
				t.Fatalf("exec changed sandbox restrictions: code %d", code)
			}
			must(t, "exec", "-d", name, "fixture.exe", "tree-hold", `$HOME\..\workspace\exec-pid`, "ignore")
			var pid uint32
			fmt.Sscan(waitFile(filepath.Join(source, "exec-pid.child")), &pid)
			child, err := win32.OpenProcess(win32.SYNCHRONIZE, false, win32.DWORD(pid))
			if err != nil {
				t.Fatal(err)
			}
			defer win32.CloseHandle(child)
			must(t, "restart", "-t", "0", name)
			if win32.WaitForSingleObject(child, 5000) != win32.WAIT_OBJECT_0 {
				t.Fatal("restart left exec descendant")
			}
			if got := must(t, "exec", name, "fixture.exe", "registry-read", key); got != "42" {
				t.Fatalf("registry wasn't retained through restart: %q", got)
			}
			after, err := st.ResolveContainer(name)
			if err != nil || after.State.RunID == rec.State.RunID || after.State.ExecProtocol != 1 {
				t.Fatalf("invalid replacement: %+v %v", after, err)
			}
			if pipe, err := win32.DialPipe(fmt.Sprintf(`\\.\pipe\quasidock-exec-%s-%s`, rec.ID, rec.State.RunID), time.Now().Add(100*time.Millisecond)); err == nil {
				pipe.Close()
				t.Fatal("outgoing exec endpoint survived restart")
			}
			must(t, "exec", "-d", name, "fixture.exe", "tree-hold", `$HOME\..\workspace\last-pid`, "ignore")
			fmt.Sscan(waitFile(filepath.Join(source, "last-pid.child")), &pid)
			last, err := win32.OpenProcess(win32.SYNCHRONIZE, false, win32.DWORD(pid))
			if err != nil {
				t.Fatal(err)
			}
			defer win32.CloseHandle(last)
			must(t, "rm", "-f", name)
			if win32.WaitForSingleObject(last, 5000) != win32.WAIT_OBJECT_0 {
				t.Fatal("removal left exec descendant")
			}
			if _, err := st.ResolveContainer(name); err == nil {
				t.Fatal("automatic container was retained")
			}
			if data, err := os.ReadFile(filepath.Join(source, "exec.txt")); err != nil || strings.TrimSpace(string(data)) != sandbox {
				t.Fatalf("removal lost mounted exec writes: %q %v", data, err)
			}
			if !store.Exists(source) {
				t.Fatal("removed mounted volume")
			}
		})
	}
}
