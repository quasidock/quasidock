package services

import (
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
)

func TestUsedBy(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg := oci.AppConfig{
		ID:      "app",
		Version: "1",
		Command: "x.exe",
		Permissions: oci.Permissions{
			Uses: []oci.Use{{Name: "db", Port: 5432}},
		},
		Runtimes: map[string]oci.Runtime{
			"php": {
				ID:    "php",
				Image: digest,
				Layer: digest,
				Uses: []oci.Use{
					{Name: "db", Port: 5433},
					{Name: "cache", Port: 6379},
				},
			},
		},
	}

	ext := oci.AppConfig{
		ID:      "ext",
		Version: "1",
		Command: "x.exe",
		Permissions: oci.Permissions{
			Uses: []oci.Use{
				{Name: "dbgp", Port: 9003},
				{Name: "cache", Port: 1},
			},
		},
	}

	used := UsedBy(cfg, []oci.AppConfig{ext})
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
		ID:      "app",
		Version: "1",
		Command: "bin/server.exe",
		Exports: map[string]oci.Export{
			"server":         {Command: "bin/server.exe"},
			"client":         {Command: "bin/client.exe"},
			"verbose-server": {Command: "bin/server.exe", Args: []string{"-v"}},
		},
	}

	byExport := oci.Service{Name: "db", Port: 5432, Command: "server"}
	if !Serves(cfg, byExport, "server") {
		t.Error("expected Serves(server) true")
	}
	if !Serves(cfg, byExport, "Server") {
		t.Error("expected Serves(Server) true")
	}
	if !Serves(cfg, byExport, "") {
		t.Error("expected Serves(empty) true")
	}
	if Serves(cfg, byExport, "client") {
		t.Error("expected Serves(client) false")
	}
	if Serves(cfg, byExport, "verbose-server") {
		t.Error("expected Serves(verbose-server) false")
	}
	if Serves(cfg, byExport, "bin/server.exe") {
		t.Error("expected Serves(bin/server.exe) false")
	}

	byApp := oci.Service{Name: "db", Port: 5432}
	if !Serves(cfg, byApp, "") {
		t.Error("expected Serves(empty) true")
	}
	if !Serves(cfg, byApp, "server") {
		t.Error("expected Serves(server) true")
	}
	if Serves(cfg, byApp, "client") {
		t.Error("expected Serves(client) false")
	}
}
