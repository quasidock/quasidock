package services

import (
	"fmt"
	stdnet "net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

func TestParsePublish(t *testing.T) {
	for _, test := range []struct{ input, target, ip, port string }{
		{"8080:80", "80", "", "8080"}, {"80", "80", "", ""},
		{":http", "http", "", ""}, {"0:HTTP/tcp", "http", "", ""},
		{"127.0.0.1::80", "80", "127.0.0.1", ""},
		{"[::1]:8080:80/tcp", "80", "::1", "8080"},
		{"0.0.0.0:08080:080", "80", "0.0.0.0", "8080"},
	} {
		got, err := ParsePublish(test.input)
		if err != nil || got.Target != test.target || got.HostIP != test.ip || got.HostPort != test.port {
			t.Errorf("%q: %+v %v", test.input, got, err)
		}
	}
	for _, raw := range []string{"", "0", "65536", "1/udp", "1/sctp", "1/tcp/udp", "70000:80", "-1:80", "localhost:80:80", "::1:8080:80", "[fe80::1%3]:80:80", "80:", "80:bad name"} {
		if _, err := ParsePublish(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestNormalizePublishing(t *testing.T) {
	cfg := oci.AppConfig{ID: "test", Permissions: oci.Permissions{Services: []oci.Service{{Name: "http", Port: 80}}}}
	parse := func(values ...string) []PublishSpec {
		t.Helper()
		var out []PublishSpec
		for _, value := range values {
			p, err := ParsePublish(value)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		return out
	}
	bindings, err := NormalizePublishing(cfg, parse("8080:HTTP", "8081:80", "9000:9001"), nil)
	if err != nil || len(bindings["80/tcp"]) != 2 || bindings["80/tcp"][0].HostPort != "8080" || len(bindings["9001/tcp"]) != 1 {
		t.Fatalf("bindings: %+v %v", bindings, err)
	}
	for _, values := range [][]string{{"missing"}, {"8000:8000"}, {"8080:80", "8080:http"}, {"80-81"}} {
		if _, err := NormalizePublishing(cfg, parse(values...), nil); err == nil {
			t.Errorf("accepted %v", values)
		}
	}
	if _, err := NormalizePublishing(cfg, parse("8080:80"), []string{"HTTP=9000"}); err == nil {
		t.Fatal("accepted conflicting overrides")
	}
	if _, err := NormalizePublishing(cfg, parse("8080:80"), []string{"db=9000"}); err != nil {
		t.Fatal(err)
	}
}

func TestPreparePublishingAndCleanup(t *testing.T) {
	cfg := oci.AppConfig{ID: "server", Entrypoint: []string{"server"}}
	requests := map[string][]store.PortBinding{"80/tcp": {{}, {HostIP: "127.0.0.1"}}}
	p, err := PreparePublish(cfg, nil, requests, "", oci.SandboxSoft, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if len(p.Ports["80/tcp"]) < 2 || len(p.Args) != len(p.Sockets) {
		t.Fatalf("published: %+v", p)
	}
	first := p.Ports["80/tcp"][0]
	if len(p.Ports["80/tcp"]) == 3 && p.Ports["80/tcp"][1].HostPort != first.HostPort {
		t.Fatal("loopback families have different ports")
	}
	// A later failed binding must roll back every earlier listener.
	block, err := stdnet.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer block.Close()
	port := strconv.Itoa(block.Addr().(*stdnet.TCPAddr).Port)
	free, err := stdnet.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := strconv.Itoa(free.Addr().(*stdnet.TCPAddr).Port)
	_ = free.Close()
	_, err = PreparePublish(cfg, nil, map[string][]store.PortBinding{"81/tcp": {{HostIP: "127.0.0.1", HostPort: freePort}, {HostIP: "127.0.0.1", HostPort: port}}}, "", oci.SandboxSoft, t.Logf)
	if err == nil {
		t.Fatal("accepted occupied host port")
	}
	reopened, err := stdnet.Listen("tcp4", "127.0.0.1:"+freePort)
	if err != nil {
		t.Fatalf("leaked earlier listener: %v", err)
	}
	_ = reopened.Close()
	p.Close()
	p.Close() // Ownership release is idempotent.
	reopened, err = stdnet.Listen("tcp4", stdnet.JoinHostPort(first.HostIP, first.HostPort))
	if err != nil {
		t.Fatalf("close leaked listener: %v", err)
	}
	_ = reopened.Close()
}

func TestPublishingServicesAndReservations(t *testing.T) {
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := oci.AppConfig{ID: "server", Entrypoint: []string{"server"}, Permissions: oci.Permissions{Services: []oci.Service{{Name: "http", Port: 80}, {Name: "other", Port: 81}}}}
	for _, sandbox := range []oci.Sandbox{oci.SandboxSoft, oci.SandboxLow, oci.SandboxAppContainer} {
		id := string(sandbox)
		if err := os.MkdirAll(st.ContainerDir(id), 0700); err != nil {
			t.Fatal(err)
		}
		requests := map[string][]store.PortBinding{"80/tcp": {{HostIP: "127.0.0.1"}}}
		ports, err := ResolvePublishing(st, id, cfg, nil, nil, sandbox, requests)
		if err != nil || ports[0].Listen == 80 {
			t.Fatalf("%s: %+v %v", sandbox, ports, err)
		}
		p, err := PreparePublish(cfg, ports, requests, "", sandbox, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		if ports[0].Port == 0 || strconv.Itoa(int(ports[0].Port)) != p.Ports["80/tcp"][0].HostPort || len(p.Ports["81/tcp"]) == 0 {
			t.Fatalf("service endpoints: %+v %+v", ports, p.Ports)
		}
		p.Close()
		if sandbox != oci.SandboxAppContainer && !reflect.DeepEqual(p.Direct["81/tcp"], p.Ports["81/tcp"]) {
			t.Fatal("lost direct endpoint")
		}
	}
	// A raw backend cannot be chosen as an allocated service backend, even when
	// that port is a saved reservation from a previous run.
	ports, err := ResolvePublishing(st, "soft", cfg, nil, nil, oci.SandboxSoft, map[string][]store.PortBinding{"80/tcp": {{}}})
	if err != nil {
		t.Fatal(err)
	}
	raw := ports[0].Listen
	requests := map[string][]store.PortBinding{"80/tcp": {{}}, PortKey(raw): {{}}}
	ports, err = ResolvePublishing(st, "soft", cfg, nil, nil, oci.SandboxSoft, requests)
	if err != nil || ports[0].Listen == raw {
		t.Fatalf("allocated raw backend %d: %+v %v", raw, ports, err)
	}
	block, err := stdnet.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer block.Close()
	busy := uint16(block.Addr().(*stdnet.TCPAddr).Port)
	var notes []string
	p, err := PreparePublish(cfg, []Ports{{Service: cfg.Permissions.Services[0], Port: busy, Listen: 12345}}, nil, "", oci.SandboxAppContainer, func(f string, args ...any) { notes = append(notes, fmt.Sprintf(f, args...)) })
	if err != nil || len(p.Ports) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "http") {
		t.Fatalf("legacy failure: %+v %v %v", p, err, notes)
	}
	p.Close()
	p, err = PreparePublish(cfg, []Ports{{Service: cfg.Permissions.Services[0], Port: 80, Listen: 80}}, nil, "client", oci.SandboxSoft, t.Logf)
	if err != nil || len(p.Ports) != 0 {
		t.Fatal("published automatic service for auxiliary command")
	}
	p.Close()
}
