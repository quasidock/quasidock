package cli

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestContainerPortPublishing(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	binary, payload, buildDir := buildContainerTestBinaries(t)
	reg := testregistry.New()
	defer reg.Close()
	fixtureImage(t, reg, "ports", payload, func(c *oci.AppConfig) {
		c.Cmd = []string{"tcp-serve", "127.0.0.1:$TEST_LISTEN", "$HOME/port-ready"}
		c.Env["TEST_LISTEN"] = "${listen:http}"
		c.Env["TEST_PUBLIC"] = "${port:http}"
		c.Permissions.Services = []oci.Service{{Name: "http", Port: 80}}
	})
	fixtureImage(t, reg, "raw", payload)
	image, rawImage := reg.Host()+"/app:ports", reg.Host()+"/app:raw"
	command := func(args ...string) *exec.Cmd {
		cancel, stop := stdcontext.WithTimeout(stdcontext.Background(), 30*time.Second)
		t.Cleanup(stop)
		cmd := exec.CommandContext(cancel, binary, args...)
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		return cmd
	}
	must := func(args ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		cmd := command(args...)
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v: %v\n%s\n%s", args, err, &out, &stderr)
		}
		return out.String()
	}
	fail := func(contains string, args ...string) {
		t.Helper()
		out, err := command(args...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), contains) {
			t.Fatalf("%v: %s %v", args, out, err)
		}
	}
	t.Cleanup(func() {
		all, _ := ctx.Store.ListContainers(true)
		for _, c := range all {
			if out, err := command("rm", "-f", c.ID).CombinedOutput(); err != nil {
				t.Errorf("cleanup: %s %v", out, err)
			}
		}
	})
	inspect := func(name string) store.ContainerRecord {
		t.Helper()
		var records []store.ContainerRecord
		if err := json.Unmarshal([]byte(must("container", "inspect", name)), &records); err != nil || len(records) != 1 {
			t.Fatalf("inspect: %v", err)
		}
		return records[0]
	}
	ping := func(endpoint string) {
		t.Helper()
		awaitPauseCondition(t, "TCP echo at "+endpoint, func() bool {
			conn, err := net.DialTimeout("tcp", endpoint, 200*time.Millisecond)
			if err != nil {
				return false
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
			_, _ = conn.Write([]byte("ping"))
			var reply [4]byte
			_, err = io.ReadFull(conn, reply[:])
			return err == nil && string(reply[:]) == "ping"
		})
	}
	endpoints := func(name, selector string) []string {
		t.Helper()
		text := must("port", name, selector)
		return strings.Fields(text)
	}
	freePort := func() string {
		t.Helper()
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		_ = ln.Close()
		return port
	}
	for _, sandbox := range []string{"soft", "low", "appcontainer"} {
		t.Run(sandbox, func(t *testing.T) {
			name := "ports-" + sandbox
			must("create", "--name", name, "--sandbox="+sandbox, "-p", "http", "-p", "127.0.0.1::80", image)
			if text := must("port", name); text != "" {
				t.Fatalf("created endpoints: %q", text)
			}
			rec := inspect(name)
			if rec.NetworkSettings != nil || len(rec.HostConfig.PortBindings["80/tcp"]) != 2 || rec.HostConfig.PortBindings["80/tcp"][0].HostPort != "" {
				t.Fatalf("requests: %+v", rec)
			}
			must("start", name)
			eps := endpoints(name, "http")
			if len(eps) < 2 {
				t.Fatalf("endpoints: %v", eps)
			}
			for _, endpoint := range eps {
				ping(endpoint)
			}
			peer := name + "-peer"
			must("run", "-d", "--name", peer, "--sandbox="+sandbox, "-phttp", image)
			ping(endpoints(peer, "http")[0])
			ping(eps[0])
			must("rm", "-f", peer)
			if got := must("container", "port", name, "80/tcp"); got != strings.Join(eps, "\n")+"\n" {
				t.Fatalf("selector: %q", got)
			}
			_, primaryPort, _ := net.SplitHostPort(eps[0])
			if text := must("exec", name, "fixture.exe", "env", "TEST_PUBLIC", "TEST_LISTEN"); !strings.Contains(text, primaryPort) {
				t.Fatalf("placeholder: %q", text)
			}
			must("pause", name)
			if text := must("port", name); !strings.Contains(text, "80/tcp -> ") {
				t.Fatal("paused mapping missing")
			}
			must("unpause", name)
			must("restart", "-t", "0", name)
			for _, endpoint := range endpoints(name, "80") {
				ping(endpoint)
			}
			must("stop", "-t", "0", name)
			if text := must("port", name); text != "" {
				t.Fatalf("stopped endpoints: %q", text)
			}
			rec = inspect(name)
			if rec.NetworkSettings == nil || len(rec.NetworkSettings.Ports) != 0 || len(rec.HostConfig.PortBindings) == 0 {
				t.Fatal("stop lost requests or retained active mappings")
			}
			fail("no public port", "port", name, "http")
			// A raw server uses a fixed backend without service placeholders.
			raw, host := freePort(), freePort()
			rawName := "raw-" + sandbox
			must("run", "-d", "--name", rawName, "--sandbox="+sandbox, "-p", "127.0.0.1:"+host+":"+raw, rawImage, "fixture.exe", "tcp-serve", "127.0.0.1:"+raw)
			ping("127.0.0.1:" + host)
			must("rm", "-f", rawName)
			if conn, err := net.DialTimeout("tcp4", "127.0.0.1:"+host, 100*time.Millisecond); err == nil {
				conn.Close()
				t.Fatal("listener survived rm")
			}
		})
	}
	// Explicit wildcard addresses retain their identity in reporting.
	must("run", "-d", "--name", "ports-wildcard", "-p0.0.0.0::http", image)
	wildcard := endpoints("ports-wildcard", "http")
	if len(wildcard) != 1 || !strings.HasPrefix(wildcard[0], "0.0.0.0:") {
		t.Fatalf("wildcard: %v", wildcard)
	}
	_, wildcardPort, _ := net.SplitHostPort(wildcard[0])
	ping("127.0.0.1:" + wildcardPort)
	must("rm", "-f", "ports-wildcard")
	// Required explicit publication fails before starting a payload on conflicts.
	block, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer block.Close()
	port := strconv.Itoa(block.Addr().(*net.TCPAddr).Port)
	fail("cannot listen", "run", "-d", "--name", "ports-busy", "-p", "127.0.0.1:"+port+":80", image)
	if rec := inspect("ports-busy"); rec.State.Running || rec.State.Launched || rec.NetworkSettings != nil {
		t.Fatalf("failed launch: %+v", rec)
	}
	fail("cannot combine", "create", "--port=http=9000", "-p8080:http", image)
	fail("overlaps", "create", "-p8000:8000", rawImage)
	// Runtime relay loss removes endpoints without killing the application.
	must("run", "-d", "--name", "relay-loss", "-phttp", image)
	ping(endpoints("relay-loss", "http")[0])
	var rows []struct{ PID, Command string }
	for _, line := range strings.Split(strings.TrimSpace(must("top", "--format=json", "relay-loss")), "\n") {
		var row struct{ PID, Command string }
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	found := false
	for _, row := range rows {
		if !strings.Contains(row.Command, "quasidock-relay-") {
			continue
		}
		pid, _ := strconv.Atoi(row.PID)
		h, err := win32.OpenProcess(win32.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			t.Fatal(err)
		}
		if !win32.TerminateProcess(h, 1) {
			t.Fatal("terminate relay")
		}
		win32.CloseHandle(h)
		found = true
	}
	if !found {
		t.Fatalf("relay absent from top: %+v", rows)
	}
	awaitPauseCondition(t, "relay mappings removed", func() bool { return must("port", "relay-loss") == "" })
	if !inspect("relay-loss").State.Running {
		t.Fatal("relay loss stopped payload")
	}
	awaitPauseCondition(t, "relay loss diagnostic", func() bool {
		return strings.Contains(must("logs", "relay-loss"), "port relay exited")
	})
	must("rm", "-f", "relay-loss")
	// A foreground launch shares the same publication lifecycle.
	fg := command("run", "--name", "ports-foreground", "-phttp", image)
	var output bytes.Buffer
	fg.Stdout, fg.Stderr = &output, &output
	if err := fg.Start(); err != nil {
		t.Fatal(err)
	}
	awaitPauseCondition(t, "foreground started", func() bool {
		rec, err := ctx.Store.ResolveContainer("ports-foreground")
		return err == nil && rec.State.Running
	})
	ping(endpoints("ports-foreground", "http")[0])
	must("stop", "-t", "0", "ports-foreground")
	_ = fg.Wait()
	if text := must("port", "ports-foreground"); text != "" {
		t.Fatal(text)
	}
	// Missing relay must fail required publication rather than run unpublished.
	if err := os.Rename(filepath.Join(buildDir, "quasidock-relay.exe"), filepath.Join(buildDir, "missing-relay.exe")); err != nil {
		t.Fatal(err)
	}
	fail("preparing port relay", "run", "-d", "--name", "ports-no-relay", "-phttp", image)
	if rec := inspect("ports-no-relay"); rec.State.Running {
		t.Fatal(fmt.Sprint(rec.State))
	}
	// An existing but invalid relay also fails startup and releases its port.
	if err := os.WriteFile(filepath.Join(buildDir, "quasidock-relay.exe"), []byte("invalid executable"), 0600); err != nil {
		t.Fatal(err)
	}
	failedPort := freePort()
	fail("starting port relay", "run", "-d", "--name", "ports-bad-relay", "-p127.0.0.1:"+failedPort+":http", image)
	if rec := inspect("ports-bad-relay"); rec.State.Running || rec.State.Launched || rec.NetworkSettings != nil {
		t.Fatalf("relay launch failure: %+v", rec)
	}
	available, err := net.Listen("tcp4", "127.0.0.1:"+failedPort)
	if err != nil {
		t.Fatalf("failed relay leaked listener: %v", err)
	}
	_ = available.Close()
}
