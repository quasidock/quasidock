package main

import (
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/win32"
)

func TestVariable(t *testing.T) {
	// =C:=C:\x\x00Path=a;b\x00PATHEXT=.EXE\x00\x00
	str := "=C:=C:\\x\x00Path=a;b\x00PATHEXT=.EXE\x00\x00"
	block := win32.UTF8ToUTF16(str)

	val, ok := variable(block, "PATH")
	if !ok || val != "a;b" {
		t.Fatalf("expected 'a;b', got %q, ok=%v", val, ok)
	}

	val, ok = variable(block, "pathext")
	if !ok || val != ".EXE" {
		t.Fatalf("expected '.EXE', got %q, ok=%v", val, ok)
	}

	_, ok = variable(block, "PAT")
	if ok {
		t.Fatal("expected PAT not to be found")
	}
}

func TestIsBatch(t *testing.T) {
	if !isBatch("C:\\x\\a.CMD") {
		t.Error("expected C:\\x\\a.CMD to be batch")
	}
	if !isBatch("a.bat") {
		t.Error("expected a.bat to be batch")
	}
	if isBatch("a.exe") {
		t.Error("expected a.exe not to be batch")
	}
	if isBatch("a.cmd.exe") {
		t.Error("expected a.cmd.exe not to be batch")
	}
}
