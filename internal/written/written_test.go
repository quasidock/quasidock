package written

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestIsProgram(t *testing.T) {
	for _, n := range []string{"a.exe", "B.EXE", "x.Com", "s.scr", `dir\tool.exe`} {
		if !IsProgram(n) {
			t.Errorf("expected IsProgram(%q) to be true", n)
		}
	}

	for _, n := range []string{"a.dll", "a.exe.txt", "exe", "a.bat", "a.ps1"} {
		if IsProgram(n) {
			t.Errorf("expected IsProgram(%q) to be false", n)
		}
	}
}

func TestSettleRestoresProgramLabelAfterParentCleanup(t *testing.T) {
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := acl.LabelLow(dir); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "created.exe")
	if err := os.WriteFile(program, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Record(st, "test.parent-cleanup", []string{program}); err != nil {
		t.Fatal(err)
	}
	if err := acl.Unlabel(dir); err != nil {
		t.Fatal(err)
	}
	if err := Settle(st, "test.parent-cleanup"); err != nil {
		t.Fatal(err)
	}
	if label, err := acl.OwnLabel(program); err != nil || label == nil || *label != win32.SECURITY_MANDATORY_MEDIUM_RID {
		t.Fatalf("recorded executable has no explicit medium label: %v %v", label, err)
	}
}
