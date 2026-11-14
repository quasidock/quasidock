package oci_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
)

func TestRuntimeLayersAndDirectoriesDoNotDependOnMapOrder(t *testing.T) {
	a, b, own := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64)
	cfg := oci.AppConfig{From: map[string]oci.Runtime{"alpha": {Layer: a}, "beta": {Layer: b}}}
	m := oci.Manifest{Layers: []oci.Descriptor{{MediaType: oci.MediaTypeLayerTar, Digest: b}, {MediaType: oci.MediaTypeLayerTar, Digest: a}, {MediaType: oci.MediaTypeLayerTar, Digest: own}}}
	for range 25 {
		if err := oci.ValidateLayers("cached", &m, &cfg); err != nil {
			t.Fatal(err)
		}
		dirs := oci.RuntimeDirectories(cfg.From, []string{filepath.Join("deploy", oci.DigestHex(b)), filepath.Join("deploy", oci.DigestHex(a))})
		if len(dirs) != 2 || dirs[0].Alias != "alpha" || filepath.Base(dirs[0].Path) != oci.DigestHex(a) || dirs[1].Alias != "beta" || filepath.Base(dirs[1].Path) != oci.DigestHex(b) {
			t.Fatalf("misassigned runtime directories: %+v", dirs)
		}
	}
	m.Layers[0].Digest = a
	if err := oci.ValidateLayers("duplicate", &m, &cfg); err == nil {
		t.Fatal("accepted duplicate layers in place of a runtime")
	}
}

func TestValidID(t *testing.T) {
	valid := []string{"node", "busybox", "app-1", "a.b_c", "org.nodejs.node"}
	for _, id := range valid {
		if !oci.IsValidID(id) {
			t.Errorf("expected %q to be valid ID", id)
		}
	}
	invalid := []string{"", ".hidden", "has space", "a/b", strings.Repeat("x", 58)}
	for _, id := range invalid {
		if oci.IsValidID(id) {
			t.Errorf("expected %q to be invalid ID", id)
		}
	}
}

func TestPlaceholdersExpand(t *testing.T) {
	p := oci.Placeholders{
		App:   "C:\\Apps\\myapp",
		Data:  "C:\\Data\\myapp",
		Cache: "C:\\Data\\myapp\\cache",
		Roots: "C:\\Data\\roots.pem",
		Runtimes: []oci.DirPlaceholder{
			{Alias: "node", Path: "C:\\Apps\\node"},
		},
		Services: []oci.ServicePorts{
			{Name: "web", Listen: "8080", Port: "80"},
		},
		Uses: []oci.UsedPort{
			{Name: "db", Port: "5432"},
		},
	}

	tests := []struct {
		input string
		want  string
	}{
		{"${app}\\bin\\run.exe", "C:\\Apps\\myapp\\bin\\run.exe"},
		{"${node}\\node.exe", "C:\\Apps\\node\\node.exe"},
		{"--port=${listen:web}", "--port=8080"},
		{"--connect=${uses:db}", "--connect=5432"},
		{"${unknown}", "${unknown}"},
	}

	for _, tt := range tests {
		got := p.Expand(tt.input)
		if got != tt.want {
			t.Errorf("Expand(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	cfg := oci.AppConfig{
		ID:         "app",
		Version:    "1.0.0",
		Entrypoint: []string{"bin/app.exe"},
		Cmd:        []string{"--serve"},
		Path:       []string{"."},
		Permissions: oci.Permissions{
			Sandbox:    oci.SandboxLow,
			Network:    true,
			Filesystem: []string{"cwd", `C:\Data:ro`},
		},
	}

	if err := oci.ValidateConfig("test", &cfg); err != nil {
		t.Fatalf("ValidateConfig failed: %v", err)
	}

	data, err := oci.ToJSON(cfg)
	if err != nil {
		t.Fatalf("ToJSON failed: %v", err)
	}

	parsed, err := oci.ParseConfig("test", oci.MediaTypeConfig, data)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	if parsed.ID != cfg.ID || len(parsed.Entrypoint) != 1 || parsed.Entrypoint[0] != cfg.Entrypoint[0] || len(parsed.Cmd) != 1 || parsed.Cmd[0] != cfg.Cmd[0] {
		t.Fatalf("Parsed config mismatch")
	}
}
