package exports

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/oci"
)

func TestOnPath(t *testing.T) {
	bin := `C:\Users\me\AppData\Local\zigsaw\bin`
	if !OnPath(`C:\Windows;c:\users\ME\AppData\Local\zigsaw\bin\;D:\x`, bin) {
		t.Fatalf("expected on path")
	}
	if !OnPath(`"C:\Users\me\AppData\Local\zigsaw\bin"`, bin) {
		t.Fatalf("expected on path in quotes")
	}
	if OnPath(`C:\Windows;C:\Users\me\AppData\Local\zigsaw`, bin) {
		t.Fatalf("expected not on path")
	}
}

func TestCommandDirs(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	c := oci.AppConfig{
		ID:      "x",
		Version: "1",
		Command: "x.exe",
		Path:    []string{".", `${data}\npm`, `${cache}\bin`, `${app}\tools`, "bin"},
		Runtimes: map[string]oci.Runtime{
			"node": {
				ID:      "org.nodejs.node",
				Version: "24",
				Image:   digest,
				Layer:   digest,
				Path:    []string{".", `${data}/cargo/bin`, `${data}\npm`},
			},
		},
	}
	if !HasCommandDirs(c) {
		t.Fatalf("expected HasCommandDirs to be true")
	}
	dirs := CommandDirs(c, `C:\z\data\x`)
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d: %v", len(dirs), dirs)
	}
	if dirs[0] != `C:\z\data\x\npm` {
		t.Fatalf("expected C:\\z\\data\\x\\npm, got %q", dirs[0])
	}
	if dirs[1] != `C:\z\data\x\cargo\bin` {
		t.Fatalf("expected C:\\z\\data\\x\\cargo\\bin, got %q", dirs[1])
	}

	plain := oci.AppConfig{
		ID:      "x",
		Version: "1",
		Command: "x.exe",
		Path:    []string{".", `${cache}`},
	}
	if HasCommandDirs(plain) {
		t.Fatalf("expected HasCommandDirs to be false for plain")
	}
	if len(CommandDirs(plain, `C:\z\data\x`)) != 0 {
		t.Fatalf("expected 0 dirs for plain")
	}
}

func TestFindCommands(t *testing.T) {
	tmpDir := t.TempDir()
	files := []string{
		"one/tsc.cmd",
		"one/tsc",
		"one/tsc.ps1",
		"one/Semver.CMD",
		"one/a.exe",
		"one/.hidden.exe",
		"one/zigsaw.exe",
		"one/b.bat",
		"one/notes.txt",
		"two/a.cmd",
		"two/c.com",
	}
	for _, f := range files {
		p := filepath.Join(tmpDir, f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(""), 0o755)
	}
	_ = os.MkdirAll(filepath.Join(tmpDir, "one", "d.exe"), 0o755)

	dirs := []string{
		filepath.Join(tmpDir, "one"),
		filepath.Join(tmpDir, "missing"),
		filepath.Join(tmpDir, "two"),
	}
	found, err := FindCommands(dirs)
	if err != nil {
		t.Fatalf("FindCommands failed: %v", err)
	}
	want := []string{"a", "b", "Semver", "tsc", "c"}
	if len(found) != len(want) {
		t.Fatalf("expected %d found, got %d: %+v", len(want), len(found), found)
	}
	for i, w := range want {
		if found[i].Name != w {
			t.Fatalf("expected %s at index %d, got %s", w, i, found[i].Name)
		}
	}
	if found[3].Path != filepath.Join(tmpDir, "one", "tsc.cmd") {
		t.Fatalf("expected one/tsc.cmd, got %s", found[3].Path)
	}
	if found[4].Path != filepath.Join(tmpDir, "two", "c.com") {
		t.Fatalf("expected two/c.com, got %s", found[4].Path)
	}
}
