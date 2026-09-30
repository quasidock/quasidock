package oci_test

import (
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
)

func TestValidID(t *testing.T) {
	valid := []string{"org.nodejs.node", "net.frippery.busybox", "app-1", "a.b_c"}
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

func TestValidShortcutName(t *testing.T) {
	valid := []string{"Text Editor", "GTK 4 Demo", "Éditeur de texte", "a", "Notes (beta)", "x.y", "COM10", "Console"}
	for _, name := range valid {
		if !oci.IsValidShortcutName(name) {
			t.Errorf("expected %q to be valid shortcut name", name)
		}
	}
	invalid := []string{"", " lead", "trail ", "dot.", ".dot", "a/b", "a\\b", "a:b", "what?", "tab\there", "con", "Nul.txt", "COM1", "lpt9"}
	for _, name := range invalid {
		if oci.IsValidShortcutName(name) {
			t.Errorf("expected %q to be invalid shortcut name", name)
		}
	}
}

func TestValidExtension(t *testing.T) {
	valid := []string{".rnote", ".xopp", ".TXT", ".c++", ".tar-gz", ".7z"}
	for _, ext := range valid {
		if !oci.IsValidExtension(ext) {
			t.Errorf("expected %q to be valid extension", ext)
		}
	}
	invalid := []string{"", ".", "rnote", "..x", ".a.b", ".a b", ".a\\b", "." + strings.Repeat("x", 33)}
	for _, ext := range invalid {
		if oci.IsValidExtension(ext) {
			t.Errorf("expected %q to be invalid extension", ext)
		}
	}
}

func TestValidScheme(t *testing.T) {
	valid := []string{"vscodium", "http", "https", "mailto", "web+x", "a.b-c2"}
	for _, s := range valid {
		if !oci.IsValidScheme(s) {
			t.Errorf("expected %q to be valid scheme", s)
		}
	}
	invalid := []string{"", "Vscodium", "1a", "-a", "a b", "a:b", "a\\b", "file", "ms-settings", "microsoft-edge", "windows-defender", "search-ms", "shell", "javascript", "zigsaw-x"}
	for _, s := range invalid {
		if oci.IsValidScheme(s) {
			t.Errorf("expected %q to be invalid scheme", s)
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
		ID:      "org.example.app",
		Version: "1.0.0",
		Command: "bin/app.exe",
		Path:    []string{"."},
		Permissions: oci.Permissions{
			Sandbox:    oci.SandboxLow,
			Network:    true,
			Filesystem: []string{"cwd", "documents:ro"},
		},
		Exports: map[string]oci.Export{
			"app": {Command: "bin/app.exe"},
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
	if parsed.ID != cfg.ID || parsed.Command != cfg.Command {
		t.Fatalf("Parsed config mismatch")
	}
}
