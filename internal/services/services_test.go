package services

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

func TestContainerReservations(t *testing.T) {
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ports := []Ports{{Service: oci.Service{Name: "amqp", Port: 5672}, Port: 5672}, {Service: oci.Service{Name: "management", Port: 15672}, Port: 15672}}
	const count = 8
	results := make([][]uint16, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range count {
		if err := os.MkdirAll(st.ContainerDir(fmt.Sprint(i)), 0755); err != nil {
			t.Fatal(err)
		}
		wg.Go(func() { results[i], errs[i] = internalPorts(st, fmt.Sprint(i), ports) })
	}
	wg.Wait()
	seen := map[uint16]bool{}
	for i := range count {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		for _, p := range results[i] {
			if seen[p] {
				t.Fatalf("containers share reserved port %d", p)
			}
			seen[p] = true
		}
	}
	again, err := internalPorts(st, "0", ports)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(again) != fmt.Sprint(results[0]) {
		t.Fatal("restart lost reservations")
	}
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", again[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	replaced, err := internalPorts(st, "0", ports)
	if err != nil {
		t.Fatal(err)
	}
	if replaced[0] == again[0] || replaced[1] != again[1] {
		t.Fatalf("occupied port not replaced independently: %v", replaced)
	}
	if err := os.Remove(filepath.Join(st.ContainerDir("0"), "ports.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.ContainerDir("1"), "ports.txt"), []byte("amqp invalid\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := internalPorts(st, "0", ports); err == nil {
		t.Fatal("accepted corrupt reservations")
	}
}

func TestUsedBy(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg := oci.AppConfig{
		ID:         "app",
		Version:    "1",
		Entrypoint: []string{"x.exe"},
		Permissions: oci.Permissions{
			Uses: []oci.Use{{Name: "db", Port: 5432}},
		},
		From: map[string]oci.Runtime{
			"php": {
				ID:    "php",
				Image: digest,
				Layer: digest,
				Uses: []oci.Use{
					{Name: "db", Port: 5433},
					{Name: "cache", Port: 6379},
					{Name: "dbgp", Port: 9003},
				},
			},
		},
	}

	used := UsedBy(cfg)
	if len(used) != 3 {
		t.Fatalf("len(used) = %d; want 3", len(used))
	}
	if used[0].Port != 5432 {
		t.Errorf("used[0].Port = %d; want 5432", used[0].Port)
	}
	if used[1].Port != 6379 {
		t.Errorf("used[1].Port = %d; want 6379", used[1].Port)
	}
	if used[2].Name != "dbgp" {
		t.Errorf("used[2].Name = %q; want dbgp", used[2].Name)
	}

	targets := Targets(used, []string{"dbgp=9100", "other=1"})
	if targets[2].Port != 9100 || targets[2].Connect != 9100 {
		t.Errorf("targets[2] = %+v", targets[2])
	}
	if targets[0].Port != 5432 {
		t.Errorf("targets[0].Port = %d", targets[0].Port)
	}

	placeholders := UsePlaceholders(targets)
	if placeholders[2].Port != "9100" {
		t.Errorf("placeholders[2].Port = %q", placeholders[2].Port)
	}
}

func TestParsePort(t *testing.T) {
	name, port, ok := ParsePort("postgresql=5433")
	if !ok || name != "postgresql" || port != 5433 {
		t.Fatalf("unexpected ParsePort: %s %d %v", name, port, ok)
	}
	for _, bad := range []string{"postgresql", "=5433", "pg=0", "pg=65536", "pg=x"} {
		if _, _, ok := ParsePort(bad); ok {
			t.Fatalf("expected false for %s", bad)
		}
	}
}

func TestParseKept(t *testing.T) {
	text := "http 23817\r\nbad\nadmin 0\ndb 30001\n"
	kept := ParseKept(text)
	if len(kept) != 2 {
		t.Fatalf("len(kept) = %d; want 2", len(kept))
	}
	if kept[0].Name != "http" || kept[0].Port != 23817 {
		t.Errorf("kept[0] = %+v", kept[0])
	}
	if kept[1].Name != "db" || kept[1].Port != 30001 {
		t.Errorf("kept[1] = %+v", kept[1])
	}
}

func TestServes(t *testing.T) {
	cfg := oci.AppConfig{
		ID:         "app",
		Version:    "1",
		Entrypoint: []string{"bin/server.exe"},
	}

	byCmd := oci.Service{Name: "db", Port: 5432, Command: "bin/server.exe"}
	if !Serves(cfg, byCmd, "bin/server.exe") {
		t.Error("expected Serves(bin/server.exe) true")
	}
	if !Serves(cfg, byCmd, "server") {
		t.Error("expected Serves(server) true")
	}
	if !Serves(cfg, byCmd, "app") {
		t.Error("expected Serves(app) true")
	}
	if !Serves(cfg, byCmd, "") {
		t.Error("expected Serves(empty) true")
	}
	if Serves(cfg, byCmd, "client") {
		t.Error("expected Serves(client) false")
	}

	byDefault := oci.Service{Name: "db", Port: 5432}
	if !Serves(cfg, byDefault, "") {
		t.Error("expected Serves(empty) true")
	}
	if !Serves(cfg, byDefault, "server") {
		t.Error("expected Serves(server) true")
	}
	if Serves(cfg, byDefault, "client") {
		t.Error("expected Serves(client) false")
	}

	multiCfg := oci.AppConfig{
		ID:         "kanboard",
		Version:    "1",
		Entrypoint: []string{"${php}\\php.exe", "${app}\\serve.php"},
	}
	if !Serves(multiCfg, byDefault, "") {
		t.Error("expected multi Serves(empty) true")
	}
	if !Serves(multiCfg, byDefault, "kanboard") {
		t.Error("expected multi Serves(kanboard) true")
	}
	if Serves(multiCfg, byDefault, "${php}\\php.exe") {
		t.Error("expected multi Serves(${php}\\php.exe) false")
	}
}
