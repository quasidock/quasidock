package sidecar

import (
	"bytes"
	"errors"
	"testing"
)

func TestHasScheme(t *testing.T) {
	if !HasScheme("http,https", "https://example.com/") {
		t.Error("expected true")
	}
	if !HasScheme("http,https", "HTTP://example.com/") {
		t.Error("expected true")
	}
	if !HasScheme("vscodium", "vscodium:x") {
		t.Error("expected true")
	}
	if HasScheme("http", "https://example.com/") {
		t.Error("expected false")
	}
	if HasScheme("http", "--gpu-launcher=calc") {
		t.Error("expected false")
	}
	if HasScheme("http", `C:\x\http:y`) {
		t.Error("expected false")
	}
	if HasScheme("http", "") {
		t.Error("expected false")
	}
	if HasScheme("", "http:x") {
		t.Error("expected false")
	}
}

func TestAliasesRoundTrip(t *testing.T) {
	want := Alias{
		Exe:         `C:\z\deploy\ab\zig.exe`,
		CommandLine: `C:\z\deploy\ab\zig.exe ar`,
	}
	var buf bytes.Buffer
	if err := want.Format(&buf); err != nil {
		t.Fatal(err)
	}

	got, err := ParseAlias(buf.Bytes())
	if err != nil {
		t.Fatalf("ParseAlias failed: %v", err)
	}
	if !want.Equal(got) {
		t.Errorf("got %+v; want %+v", got, want)
	}

	dropping := Alias{
		Exe:         want.Exe,
		CommandLine: `C:\z\deploy\ab\zig.exe cc -c`,
		Drop:        []string{"--64", "-x c"},
	}
	buf.Reset()
	if err := dropping.Format(&buf); err != nil {
		t.Fatal(err)
	}
	gotDrop, err := ParseAlias(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !dropping.Equal(gotDrop) {
		t.Errorf("got %+v; want %+v", gotDrop, dropping)
	}
}

func TestSidecarRoundTrips(t *testing.T) {
	want := Sidecar{
		Zigsaw:  `D:\tools\zigsaw.exe`,
		Home:    `C:\Users\me\AppData\Local\zigsaw`,
		App:     "org.nodejs.node",
		Command: "npm",
	}

	var buf bytes.Buffer
	if err := want.Format(&buf); err != nil {
		t.Fatal(err)
	}

	got, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if !want.Equal(got) {
		t.Errorf("got %+v; want %+v", got, want)
	}

	_, err = Parse([]byte("app = x\r\n"))
	if !errors.Is(err, ErrInvalidShimFile) {
		t.Errorf("expected ErrInvalidShimFile, got %v", err)
	}

	gui := want
	gui.GUI = true
	buf.Reset()
	_ = gui.Format(&buf)
	gotGUI, err := Parse(buf.Bytes())
	if err != nil || !gui.Equal(gotGUI) {
		t.Errorf("gui mismatch")
	}

	opens := gui
	opens.Files = true
	buf.Reset()
	_ = opens.Format(&buf)
	gotOpens, err := Parse(buf.Bytes())
	if err != nil || !opens.Equal(gotOpens) {
		t.Errorf("opens mismatch")
	}

	old, err := Parse([]byte("zigsaw = z.exe\r\nhome = h\r\napp = a\r\ncommand = c\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if old.GUI {
		t.Error("old should have GUI false")
	}
}
