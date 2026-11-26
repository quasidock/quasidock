package cli

import (
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
)

func TestParsePublishingOptions(t *testing.T) {
	for _, flag := range [][]string{{"-p", "8080:80"}, {"-p8080:80"}, {"-p=8080:80"}, {"--publish", "8080:80"}, {"--publish=8080:80"}} {
		for _, command := range []string{"run", "create"} {
			args := append(append([]string{}, flag...), "--publish=http", "image", "-p", "application-option")
			opts, err := parseContainerOptions(context.New(), args, command)
			if err != nil || len(opts.Publish) != 2 || opts.Publish[0].HostPort != "8080" || opts.Publish[1].Target != "http" || !reflect.DeepEqual(opts.Args, []string{"-p", "application-option"}) {
				t.Fatalf("%s %v: %+v %v", command, args, opts, err)
			}
		}
	}
	for _, args := range [][]string{{"-p"}, {"--publish"}, {"-p", "80/udp", "image"}, {"-dp8080:80", "image"}, {"-P", "image"}} {
		if _, err := parseRunOptions(context.New(), args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestPortCommandSnapshots(t *testing.T) {
	ctx, out := fixtureCLI(t)
	reg := testregistry.New()
	defer reg.Close()
	fixtureImage(t, reg, "1", []byte("unused"), func(c *oci.AppConfig) { c.Permissions.Services = []oci.Service{{Name: "http", Port: 80}} })
	image := reg.Host() + "/app:1"
	if code, err := createCommand(ctx, []string{"--name", "ports", "-p", "http", image}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	rec, err := ctx.Store.ResolveContainer("ports")
	if err != nil {
		t.Fatal(err)
	}
	if rec.HostConfig.PortBindings["80/tcp"][0].HostPort != "" || rec.NetworkSettings != nil {
		t.Fatalf("created metadata: %+v", rec)
	}
	if _, err := os.Stat(ctx.Store.Path("containers", rec.ID, "ports.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("create allocated ports: %v", err)
	}
	out.Reset()
	if code, err := portCommand(ctx, []string{"ports"}); code != 0 || err != nil || out.Len() != 0 {
		t.Fatal(code, err, out)
	}
	lock, err := store.OpenLock(ctx.Store.ContainerRunLockPath(rec.ID), true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	_, err = ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
		c.State.Running, c.State.Paused, c.State.Status = true, true, "running"
		c.NetworkSettings = &store.ContainerNetworkSettings{Ports: map[string][]store.PortBinding{
			"443/tcp": {{HostIP: "127.0.0.1", HostPort: "9443"}},
			"80/tcp":  {{HostIP: "::1", HostPort: "8080"}, {HostIP: "127.0.0.1", HostPort: "10000"}, {HostIP: "127.0.0.1", HostPort: "8080"}},
		}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := reg.Count("/manifests/")
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"ports"}, "80/tcp -> 127.0.0.1:8080\n80/tcp -> 127.0.0.1:10000\n80/tcp -> [::1]:8080\n443/tcp -> 127.0.0.1:9443\n"},
		{[]string{rec.ID[:12], "80/tcp"}, "127.0.0.1:8080\n127.0.0.1:10000\n[::1]:8080\n"},
		{[]string{rec.ID, "HTTP"}, "127.0.0.1:8080\n127.0.0.1:10000\n[::1]:8080\n"},
	} {
		out.Reset()
		if code, err := portCommand(ctx, test.args); code != 0 || err != nil || out.String() != test.want {
			t.Fatalf("%v: %d %v %q", test.args, code, err, out.String())
		}
	}
	if reg.Count("/manifests/") != before {
		t.Fatal("port query accessed registry")
	}
	for _, args := range [][]string{nil, {"ports", "81"}, {"ports", "80/udp"}, {"ports", "0"}, {"ports", "missing"}, {"ports", "80", "443"}, {"missing"}, {"--format=json"}} {
		out.Reset()
		if code, err := portCommand(ctx, args); code != 1 || err == nil || out.Len() != 0 {
			t.Fatalf("invalid %v: %d %v %q", args, code, err, out)
		}
	}
	ctx.Stdout = statsFailWriter{}
	if code, err := portCommand(ctx, []string{"ports"}); code != 1 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(code, err)
	}
	ctx.Stdout = out
	_, _ = ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error { c.NetworkSettings = nil; return nil })
	if code, err := portCommand(ctx, []string{"ports"}); code != 1 || err == nil || !strings.Contains(err.Error(), "stop and start") {
		t.Fatal(code, err)
	}
	_, _ = ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error { c.State.Running = false; c.State.Status = "exited"; return nil })
	out.Reset()
	if code, err := portCommand(ctx, []string{"ports"}); code != 0 || err != nil || out.Len() != 0 {
		t.Fatal(code, err, out)
	}
	if code, err := portCommand(ctx, []string{"ports", "http"}); code != 1 || err == nil {
		t.Fatal(code, err)
	}
}

func TestPortCommandHelp(t *testing.T) {
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	for _, args := range [][]string{{"port", "-h"}, {"container", "port", "--help"}} {
		out.Reset()
		if code, err := Dispatch(ctx, args); code != 0 || err != nil || !strings.Contains(out.String(), portUsage) {
			t.Fatal(code, err, out)
		}
	}
}
