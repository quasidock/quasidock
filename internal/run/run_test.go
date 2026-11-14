package run

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/oci"
)

func TestFilesystemFor(t *testing.T) {
	base := []string{"cwd:ro", `C:\Shared`, "documents:ro"}
	s := Settings{
		Filesystem:   []string{"cwd", `C:\Extra:ro`},
		NoFilesystem: []string{`c:\shared\`},
	}
	got := s.FilesystemFor(base)
	want := []string{"cwd", "documents:ro", `C:\Extra:ro`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilesystemFor = %v, want %v", got, want)
	}
}

func TestSandboxOf(t *testing.T) {
	if got := SandboxOf(Settings{}, oci.Permissions{}); got != oci.SandboxLow {
		t.Fatalf("default SandboxOf = %v, want low", got)
	}
	if got := SandboxOf(Settings{}, oci.Permissions{Sandbox: oci.SandboxSoft}); got != oci.SandboxSoft {
		t.Fatalf("image SandboxOf = %v, want soft", got)
	}
	ac := oci.SandboxAppContainer
	if got := SandboxOf(Settings{Sandbox: &ac}, oci.Permissions{Sandbox: oci.SandboxSoft}); got != oci.SandboxAppContainer {
		t.Fatalf("override SandboxOf = %v, want appcontainer", got)
	}
}

func TestWarnUnsupported(t *testing.T) {
	why := "the test says it can't"
	cfg := oci.AppConfig{
		ID: "test.warn.app",
		Permissions: oci.Permissions{
			Sandbox:     oci.SandboxSoft,
			Unsupported: &oci.Unsupported{AppContainer: &why},
		},
	}
	var notes []string
	WarnUnsupported(cfg, oci.SandboxAppContainer, func(format string, args ...any) {
		notes = append(notes, fmt.Sprintf(format, args...))
	})
	if len(notes) != 1 || !strings.Contains(notes[0], "warning: test.warn.app can't run under the appcontainer sandbox: the test says it can't") {
		t.Fatalf("unexpected warnings: %v", notes)
	}

	notes = nil
	WarnUnsupported(cfg, oci.SandboxSoft, func(format string, args ...any) {
		notes = append(notes, fmt.Sprintf(format, args...))
	})
	if len(notes) != 0 {
		t.Fatalf("expected no warning for supported sandbox, got %v", notes)
	}
}

func TestLabelRefusal(t *testing.T) {
	home := `C:\Users\Ann`
	own := grants.Own{
		Store:   `C:\Users\Ann\AppData\Local\quasidock`,
		SelfDir: `C:\Users\Ann\AppData\Local\quasidock\bin`,
	}

	for _, root := range []string{`C:\`, `C:`, `D:\`, `d:/`, `\\server\share\`} {
		got := LabelRefusal(root, home, own)
		if got != "it's the root of a drive" {
			t.Fatalf("expected 'it's the root of a drive' for %q, got %q", root, got)
		}
	}

	if got := LabelRefusal(`c:\users\ann\`, home, own); got != "it's your user profile" {
		t.Fatalf("expected 'it's your user profile', got %q", got)
	}
	if got := LabelRefusal(`C:/Users/Ann`, home+`\`, own); got != "it's your user profile" {
		t.Fatalf("expected 'it's your user profile', got %q", got)
	}
	if got := LabelRefusal(`C:\Users`, home, own); got != "your user profile is in it" {
		t.Fatalf("expected 'your user profile is in it', got %q", got)
	}
	if got := LabelRefusal(`C:\Users\Ann\AppData`, home, own); got != "quasidock's store is in it" {
		t.Fatalf("expected 'quasidock's store is in it', got %q", got)
	}

	for _, ok := range []string{`C:\Users\Ann\src`, `C:\Users\Anna`, `C:\Users\An`, `D:\src`, `\\server\share\dir`} {
		if got := LabelRefusal(ok, home, own); got != "" {
			t.Fatalf("expected empty for %q, got %q", ok, got)
		}
	}

	if got := LabelRefusal(`C:\Users`, "", own); got != "quasidock's store is in it" {
		t.Fatalf("expected 'quasidock's store is in it', got %q", got)
	}
}
