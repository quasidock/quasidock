package pe_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/pe"
)

func testImage(value uint16) []byte {
	const ntLen = 4 + 20 + 70
	b := make([]byte, 0x80+ntLen)
	b[0] = 'M'
	b[1] = 'Z'
	binary.LittleEndian.PutUint32(b[0x3c:0x40], 0x80)
	copy(b[0x80:0x84], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[0x80+4+20+68:0x80+4+20+70], value)
	return b
}

func TestSubsystem(t *testing.T) {
	tempDir := t.TempDir()

	cases := []struct {
		name    string
		data    []byte
		wantSub pe.Subsystem
		wantOk  bool
	}{
		{name: "gui.exe", data: testImage(2), wantSub: pe.SubsystemGUI, wantOk: true},
		{name: "console.exe", data: testImage(3), wantSub: pe.SubsystemConsole, wantOk: true},
		{name: "native.exe", data: testImage(1), wantSub: pe.SubsystemUnknown, wantOk: false},
		{name: "script.cmd", data: []byte("@echo off\r\n"), wantSub: pe.SubsystemUnknown, wantOk: false},
		{name: "truncated.exe", data: testImage(2)[:0x90], wantSub: pe.SubsystemUnknown, wantOk: false},
		{name: "empty.exe", data: []byte{}, wantSub: pe.SubsystemUnknown, wantOk: false},
	}

	for _, c := range cases {
		p := filepath.Join(tempDir, c.name)
		if err := os.WriteFile(p, c.data, 0644); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		subFile, okFile, err := pe.SubsystemOfFile(p)
		if err != nil {
			t.Fatalf("SubsystemOfFile error on %s: %v", c.name, err)
		}
		if subFile != c.wantSub || okFile != c.wantOk {
			t.Errorf("SubsystemOfFile(%s) = (%v, %v), want (%v, %v)", c.name, subFile, okFile, c.wantSub, c.wantOk)
		}

		subBytes, okBytes := pe.SubsystemOfBytes(c.data)
		if subBytes != c.wantSub || okBytes != c.wantOk {
			t.Errorf("SubsystemOfBytes(%s) = (%v, %v), want (%v, %v)", c.name, subBytes, okBytes, c.wantSub, c.wantOk)
		}
	}

	_, _, err := pe.SubsystemOfFile(filepath.Join(tempDir, "missing.exe"))
	if err == nil {
		t.Fatalf("expected error for missing file")
	}

	// Bytes that end before headers do
	subShort, okShort := pe.SubsystemOfBytes(testImage(2)[:100])
	if subShort != pe.SubsystemUnknown || okShort != false {
		t.Fatalf("expected unknown for short byte slice")
	}
}
