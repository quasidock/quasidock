package run

import (
	"testing"

	"github.com/quasidock/quasidock/internal/grants"
)

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
