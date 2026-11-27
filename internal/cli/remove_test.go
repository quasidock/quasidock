package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

func TestRemoveAnonymousVolumesCLI(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	fixtureImage(t, reg, "remove-volumes", payload)
	image := reg.Host() + "/app:remove-volumes"
	ok := func(args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return out
	}
	resolve := func(name string) *store.ContainerRecord {
		t.Helper()
		rec, err := st.ResolveContainer(name)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	ok("pull", image)
	bind := t.TempDir()
	if err := os.WriteFile(filepath.Join(bind, "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	ok("create", "--name", "retained", "-v", "${data}", image)
	retainedVolume := resolve("retained").Mounts[0].Name
	ok("rm", "retained")
	if v, err := st.ReadVolume(retainedVolume); err != nil || !v.Anonymous {
		t.Fatalf("plain rm lost anonymous volume: %+v %v", v, err)
	}
	ok("create", "--name", "remove",
		"-v", "${data}/anonymous",
		"--mount", "type=volume,target=${data}/shared",
		"-v", "work:${data}/named",
		"-v", bind+":${data}/bind", image)
	rec := resolve("remove")
	anonymous, shared := rec.Mounts[0].Name, rec.Mounts[1].Name
	ok("create", "--name", "sharer", "-v", shared+":${data}", image)
	out, code := call(st.Root, "container", "remove", "--volumes", "missing", "remove", "sharer")
	if code == 0 || !strings.HasPrefix(out, "remove\nsharer\n") || !strings.Contains(out, "missing") {
		t.Fatalf("multi-target remove: %d %s", code, out)
	}
	if _, err := st.ReadVolume(anonymous); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("anonymous volume survived removal: %v", err)
	}
	for _, name := range []string{shared, "work", retainedVolume} {
		if _, err := st.ReadVolume(name); err != nil {
			t.Fatalf("retained volume %s: %v", name, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(bind, "sentinel")); err != nil || string(b) != "keep" {
		t.Fatalf("bind contents lost: %q %v", b, err)
	}
	ok("run", "-d", "--name", "forced", "-v", "${data}/anon", image, "fixture.exe", "hold-path", "$HOME/ready")
	volume := resolve("forced").Mounts[0].Name
	if out, code := call(st.Root, "rm", "-v", "forced"); code == 0 || !strings.Contains(out, "running") {
		t.Fatalf("rm -v removed a running container: %d %s", code, out)
	}
	ok("rm", "-fv", "forced")
	if _, err := st.ReadVolume(volume); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("forced removal retained anonymous volume: %v", err)
	}
	before := ok("volume", "ls", "-q")
	ok("run", "--rm", "-v", "${data}", image, "fixture.exe", "write", "temporary")
	awaitPauseCondition(t, "run --rm removes its anonymous volume", func() bool {
		return ok("volume", "ls", "-q") == before
	})
}

func TestRemoveContainerLinkCLI(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	base := "link-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	targetID, sourceID := base+"-target", base+"-source"
	fixtureImage(t, reg, "link-target", payload, func(c *oci.AppConfig) {
		c.ID = targetID
		c.Cmd = []string{"tcp-serve", "127.0.0.1:$TEST_LISTEN", "$HOME/linked-ready"}
		c.Env["TEST_LISTEN"] = "${listen:echo}"
		c.Permissions.Services = []oci.Service{{Name: "echo", Port: 12345, Command: "other.exe"}}
	})
	fixtureImage(t, reg, "link-source", payload, func(c *oci.AppConfig) { c.ID = sourceID })
	t.Cleanup(func() {
		for _, name := range []string{"client", "server"} {
			_, _ = call(st.Root, "rm", "-f", name)
		}
		_ = appcontainer.DeleteProfile(sourceID)
		_ = appcontainer.DeleteProfile(targetID)
	})
	targetImage, sourceImage := reg.Host()+"/app:link-target", reg.Host()+"/app:link-source"
	ok := func(args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return out
	}
	fail := func(message string, args ...string) {
		t.Helper()
		if out, code := call(st.Root, args...); code == 0 || !strings.Contains(out, message) {
			t.Fatalf("%v: %d %s", args, code, out)
		}
	}
	resolve := func(name string) *store.ContainerRecord {
		t.Helper()
		rec, err := st.ResolveContainer(name)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	ok("pull", targetImage)
	ok("pull", sourceImage)
	ok("run", "-d", "--sandbox=appcontainer", "--name", "server", targetImage)
	server := resolve("server")
	awaitPauseCondition(t, "linked service ready", func() bool {
		return store.Exists(filepath.Join(server.DataDir, "home", "linked-ready"))
	})
	fail("requires an AppContainer", "create", "--sandbox=soft", "--link=server", sourceImage)
	fail("duplicate link alias", "create", "--sandbox=appcontainer", "--share=network",
		"--link=server:db-peer", "--link=server:DB_PEER", sourceImage)
	ok("create", "--sandbox=appcontainer", "--share=network", "--name", "client",
		"--env", "SPARE_PORT=explicit", "--link", "server:db", "--link=server:spare",
		sourceImage, "fixture.exe", "hold-path", "$HOME/ready")
	client := resolve("client")
	if len(client.HostConfig.Links) != 2 || client.HostConfig.Links[0].TargetID != server.ID {
		t.Fatalf("saved links: %+v", client.HostConfig.Links)
	}
	ok("--verbose", "start", "client")
	ping := func(alias string) (string, int) {
		return call(st.Root, "exec", "client", "fixture.exe", "tcp-ping", "127.0.0.1:$"+alias+"_PORT_12345_TCP_PORT")
	}
	if out, code := ping("DB"); code != 0 || strings.TrimSpace(out) != "echo" {
		t.Fatalf("linked TCP echo: %d %s\nclient log: %s\nserver log: %s", code, out, ok("logs", "client"), ok("logs", "server"))
	}
	if out := ok("port", "server"); out != "" {
		t.Fatalf("link added host publication: %q", out)
	}
	env := ok("exec", "client", "fixture.exe", "env", "DB_PORT_12345_TCP_PORT", "SPARE_PORT")
	var port string
	for _, line := range strings.Fields(env) {
		if value, found := strings.CutPrefix(line, "DB_PORT_12345_TCP_PORT="); found {
			port = strings.TrimSuffix(value, ":true")
		}
	}
	if _, err := strconv.Atoi(port); err != nil || !strings.Contains(env, "SPARE_PORT=explicit") {
		t.Fatalf("link environment: %q", env)
	}
	if socket, err := net.DialTimeout("tcp4", "127.0.0.1:"+port, 300*time.Millisecond); err == nil {
		socket.Close()
		t.Fatal("host connected to the link listener")
	}
	ok("exec", "-d", "client", "fixture.exe", "tcp-ping", "127.0.0.1:$DB_PORT_12345_TCP_PORT", "$HOME/link-stream")
	stream := filepath.Join(client.DataDir, "home", "link-stream")
	awaitPauseCondition(t, "link stream opened", func() bool { b, _ := os.ReadFile(stream); return string(b) == "ready" })
	fail("cannot be combined", "rm", "-l", "-f", "client/db")
	if out := ok("container", "rm", "--link", "/client/db"); out != "/client/db\n" {
		t.Fatalf("unlink output: %q", out)
	}
	awaitPauseCondition(t, "open link stream closed", func() bool { b, _ := os.ReadFile(stream); return string(b) == "closed" })
	if env := ok("exec", "client", "fixture.exe", "env", "DB_PORT", "DB_PORT_12345_TCP_PORT", "SPARE_PORT"); strings.Contains(env, "DB_PORT=tcp:") || strings.Contains(env, "DB_PORT_12345_TCP_PORT="+port) {
		t.Fatalf("exec still has removed link: %s", env)
	}
	if out, code := ping("SPARE"); code != 0 || !resolve("server").State.Running || !resolve("client").State.Running {
		t.Fatalf("unlink affected another route or stopped a container: %d %s", code, out)
	}
	ok("restart", "-t", "0", "server")
	awaitPauseCondition(t, "link recovers after target restart", func() bool { _, code := ping("SPARE"); return code == 0 })
	ok("rm", "-f", "server")
	ok("run", "-d", "--sandbox=appcontainer", "--name", "server", targetImage)
	if out, code := ping("SPARE"); code == 0 {
		t.Fatalf("link reached same-name replacement: %s", out)
	}
	ok("stop", "-t", "0", "client")
	ok("rm", "-l", client.ShortID()+"/spare")
	if links := resolve("client").HostConfig.Links; len(links) != 0 {
		t.Fatalf("stopped unlink retained links: %+v", links)
	}
	ok("rm", "client")
	ok("rm", "-f", "server")
	host, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := host.Addr().(*net.TCPAddr).Port
	host.Close()
	ok("run", "-d", "--sandbox=low", "--name", "server", fmt.Sprintf("--port=echo=%d", hostPort), targetImage)
	ok("run", "-d", "--sandbox=appcontainer", "--share=network", "--name", "client", "--link=server",
		sourceImage, "fixture.exe", "hold-path", "$HOME/ready")
	awaitPauseCondition(t, "default link alias reaches low-sandbox target", func() bool { _, code := ping("SERVER"); return code == 0 })
}
