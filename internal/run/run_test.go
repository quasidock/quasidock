package run

import (
	"strings"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/grants"
	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

func TestLabelRefusal(t *testing.T) {
	home := `C:\Users\Ann`
	own := grants.Own{
		Store:   `C:\Users\Ann\AppData\Local\zigsaw`,
		SelfDir: `C:\Users\Ann\AppData\Local\zigsaw\bin`,
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
	if got := LabelRefusal(`C:\Users\Ann\AppData`, home, own); got != "zigsaw's store is in it" {
		t.Fatalf("expected 'zigsaw's store is in it', got %q", got)
	}

	for _, ok := range []string{`C:\Users\Ann\src`, `C:\Users\Anna`, `C:\Users\An`, `D:\src`, `\\server\share\dir`} {
		if got := LabelRefusal(ok, home, own); got != "" {
			t.Fatalf("expected empty for %q, got %q", ok, got)
		}
	}

	if got := LabelRefusal(`C:\Users`, "", own); got != "zigsaw's store is in it" {
		t.Fatalf("expected 'zigsaw's store is in it', got %q", got)
	}
}

func TestAliasProviders(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	cfg := oci.AppConfig{
		ID:      "app",
		Version: "1",
		Command: "x.exe",
		Runtimes: map[string]oci.Runtime{
			"b": {ID: "rt.b", Version: "1", Image: digest, Layer: digest},
			"a": {ID: "rt.a", Version: "1", Image: digest, Layer: digest},
		},
	}
	image := &store.Image{
		Ref:         store.Ref{ID: "app", Version: "1", Manifest: digest},
		Config:      cfg,
		DeployDir:   `D:\app`,
		RuntimeDirs: []string{`D:\b`, `D:\a`},
	}
	providers := aliasProviders(image)
	if len(providers) < 1 {
		t.Fatalf("expected at least 1 provider, got %d", len(providers))
	}
	if providers[0].ID != "app" || providers[0].Dir != `D:\app` {
		t.Fatalf("expected app / D:\\app, got %+v", providers[0])
	}
}
