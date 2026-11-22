package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/oci"
)

func TestRestartPreservesMountedDataAndRegistry(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	id := fmt.Sprintf("restart-mount-%d", time.Now().UnixNano())
	key := `Software\QuasidockLifecycle\` + id
	fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) {
		c.ID = id
		c.Registry = []string{key}
	})
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
	for _, tc := range []struct {
		name, sandbox, target string
		bind                  bool
	}{
		{"root-volume", "soft", "${data}", false},
		{"subpath-volume", "low", "${data}/workspace", false},
		{"root-bind", "appcontainer", "${data}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "restart-" + tc.name
			source := name
			if tc.bind {
				source = t.TempDir()
			}
			counter := `$HOME\counter`
			if tc.target != "${data}" {
				counter = `$HOME\..\workspace\counter`
			}
			t.Cleanup(func() { _, _ = call(st.Root, "rm", "-f", name) })
			must(t, "create", "--rm", "--name", name, "--sandbox="+tc.sandbox, "-v", source+":"+tc.target, app, "fixture.exe", "state-hold", counter, key)
			unstarted, err := st.ResolveContainer(name)
			if err != nil || unstarted.State.Running || unstarted.State.Launched {
				t.Fatalf("create launched container: %+v %v", unstarted, err)
			}
			must(t, "start", name)
			before, err := st.ResolveContainer(name)
			if err != nil {
				t.Fatal(err)
			}
			if before.ID != unstarted.ID {
				t.Fatal("start replaced the created container")
			}
			counterPath := filepath.Join(before.Mounts[0].Source, "home", "counter")
			if tc.target != "${data}" {
				counterPath = filepath.Join(before.Mounts[0].Source, "counter")
			}
			waitCounter := func(want string) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if data, err := os.ReadFile(counterPath); err == nil && string(data) == want {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				t.Fatalf("mounted counter did not reach %s", want)
			}
			waitCounter("1")
			must(t, "restart", "-t", "0", name)
			waitCounter("2")
			after, err := st.ResolveContainer(name)
			if err != nil {
				t.Fatal(err)
			}
			if after.ID != before.ID || !reflect.DeepEqual(after.Mounts, before.Mounts) || !reflect.DeepEqual(after.HostConfig, before.HostConfig) {
				t.Fatal("restart replaced the container or its mounts")
			}
			logs := must(t, "logs", name)
			if !strings.Contains(logs, "state file=1 registry=1") || !strings.Contains(logs, "state file=2 registry=2") {
				t.Fatalf("restart lost data, registry state, or logs: %s", logs)
			}
			must(t, "kill", name)
			if data, err := os.ReadFile(counterPath); err != nil || string(data) != "2" {
				t.Fatalf("automatic removal deleted mounted data: %s %v", data, err)
			}
		})
	}
}
