package folders

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/quasidock/quasidock/internal/environment"
)

func TestExpand(t *testing.T) {
	env := []environment.Var{
		{Name: "USERPROFILE", Value: `D:\store\data\app\home`},
		{Name: "SystemDrive", Value: "C:"},
	}

	cases := []struct {
		tmpl string
		want string
	}{
		{`%USERPROFILE%\Documents`, `D:\store\data\app\home\Documents`},
		{`%userprofile%\Documents`, `D:\store\data\app\home\Documents`},
		{`C:\Users\Ann\OneDrive\Documents`, `C:\Users\Ann\OneDrive\Documents`},
		{`%OneDrive%\Documents`, `%OneDrive%\Documents`},
		{`50%%SystemDrive%\x`, `50%C:\x`},
		{`a%b`, `a%b`},
	}

	for _, c := range cases {
		got := Expand(c.tmpl, env)
		if got != c.want {
			t.Errorf("Expand(%q) = %q; want %q", c.tmpl, got, c.want)
		}
	}
}

func TestRealFoldersAllowed(t *testing.T) {
	trueVal := true
	falseVal := false

	if !RealFoldersAllowed(true, nil) {
		t.Error("expected true")
	}
	if RealFoldersAllowed(false, nil) {
		t.Error("expected false")
	}
	if !RealFoldersAllowed(false, &trueVal) {
		t.Error("expected true")
	}
	if RealFoldersAllowed(true, &falseVal) {
		t.Error("expected false")
	}
}

func TestIsInside(t *testing.T) {
	if !IsInside(`D:\s\data\app\home\Documents`, `D:\s\data\app`) {
		t.Error("expected true")
	}
	if !IsInside(`d:/S/data/APP/home/Documents`, `D:\s\data\app\`) {
		t.Error("expected true")
	}
	if IsInside(`D:\s\data\app`, `D:\s\data\app`) {
		t.Error("expected false")
	}
	if IsInside(`D:\s\data\apple\x`, `D:\s\data\app`) {
		t.Error("expected false")
	}
	if IsInside(`C:\Users\Ann\Documents`, `D:\s\data\app`) {
		t.Error("expected false")
	}
	if !SamePath(`C:\Users\Ann\Documents\`, `c:/users/ann/documents`) {
		t.Error("expected true")
	}
	if SamePath(`C:\Users\Ann\Documents2`, `C:\Users\Ann\Documents`) {
		t.Error("expected false")
	}
}

func TestJunctions(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "quasidock-junction-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	docs := filepath.Join(tmpDir, "docs")
	other := filepath.Join(tmpDir, "other")
	at := filepath.Join(tmpDir, "home", "Documents")

	_ = os.MkdirAll(docs, 0755)
	_ = os.MkdirAll(other, 0755)
	_ = os.WriteFile(filepath.Join(docs, "keep.txt"), []byte("x"), 0644)
	_ = os.WriteFile(filepath.Join(other, "keep.txt"), []byte("x"), 0644)

	place, _, err := Inspect(at)
	if err != nil || place != PlaceMissing {
		t.Fatalf("expected PlaceMissing, got %v (%v)", place, err)
	}

	if err := Link(at, docs, "documents"); err != nil {
		t.Fatalf("Link failed: %v", err)
	}

	place, target, err := Inspect(at)
	if err != nil || place != PlaceLink {
		t.Fatalf("expected PlaceLink, got %v (%v)", place, err)
	}
	if !SamePath(target, docs) {
		t.Fatalf("target = %q, want %q", target, docs)
	}

	// Read file through junction
	data, err := os.ReadFile(filepath.Join(at, "keep.txt"))
	if err != nil || string(data) != "x" {
		t.Fatalf("reading keep.txt through junction failed: %v", err)
	}

	// Link again to docs (no-op)
	if err := Link(at, docs, "documents"); err != nil {
		t.Fatal(err)
	}

	// Link to other (replaces junction)
	if err := Link(at, other, "documents"); err != nil {
		t.Fatal(err)
	}
	_, target, err = Inspect(at)
	if err != nil || !SamePath(target, other) {
		t.Fatalf("target = %q, want %q", target, other)
	}

	// Unlink
	if err := Unlink(at); err != nil {
		t.Fatal(err)
	}
	place, _, _ = Inspect(at)
	if place != PlaceMissing {
		t.Fatalf("expected PlaceMissing after Unlink, got %v", place)
	}

	// Verify original targets still exist!
	if _, err := os.Stat(filepath.Join(docs, "keep.txt")); err != nil {
		t.Error("docs/keep.txt was deleted!")
	}
	if _, err := os.Stat(filepath.Join(other, "keep.txt")); err != nil {
		t.Error("other/keep.txt was deleted!")
	}
}
