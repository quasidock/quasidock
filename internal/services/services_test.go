package services

import (
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
)

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
