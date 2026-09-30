package regkey_test

import (
	"encoding/binary"
	"fmt"
	"os"
	"testing"
	"unsafe"

	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestCopyFrom(t *testing.T) {
	path := fmt.Sprintf("Software\\QuasidockTest\\regkey-%d", os.Getpid())
	root, err := regkey.CreateWith(win32.HKEY_CURRENT_USER, path, win32.KEY_ALL_ACCESS)
	if err != nil {
		t.Fatalf("Create root failed: %v", err)
	}
	defer func() {
		root.Close()
		_ = win32.RegDeleteTree(win32.HKEY_CURRENT_USER, path)
		_ = win32.RegDeleteKey(win32.HKEY_CURRENT_USER, "Software\\QuasidockTest")
	}()

	src, err := regkey.CreateWith(root.Handle, "src\\deep\\er", win32.KEY_ALL_ACCESS)
	if err != nil {
		t.Fatalf("Create src\\deep\\er failed: %v", err)
	}
	src.Close()

	top, err := regkey.Open(root.Handle, "src", win32.KEY_ALL_ACCESS)
	if err != nil || top == nil {
		t.Fatalf("Open src failed: %v", err)
	}
	defer top.Close()

	if err := top.SetString(nil, "default"); err != nil {
		t.Fatalf("SetString default failed: %v", err)
	}
	strKey := "string"
	if err := top.SetString(&strKey, "välue"); err != nil {
		t.Fatalf("SetString string failed: %v", err)
	}

	dwordBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(dwordBytes, 7)
	dwordName := win32.UTF16Ptr("dword")
	status := win32.RegSetValueEx(top.Handle, dwordName, win32.REG_DWORD, dwordBytes)
	if status != 0 {
		t.Fatalf("RegSetValueEx dword failed: %d", status)
	}

	emptyName := win32.UTF16Ptr("empty")
	status = win32.RegSetValueEx(top.Handle, emptyName, win32.REG_BINARY, nil)
	if status != 0 {
		t.Fatalf("RegSetValueEx empty failed: %d", status)
	}

	dest, err := regkey.CreateWith(root.Handle, "dest", win32.KEY_ALL_ACCESS)
	if err != nil {
		t.Fatalf("Create dest failed: %v", err)
	}
	defer dest.Close()

	if err := dest.CopyFrom(*top); err != nil {
		t.Fatalf("CopyFrom failed: %v", err)
	}

	defVal, err := dest.GetString("")
	if err != nil || defVal == nil || *defVal != "default" {
		t.Fatalf("expected default value 'default', got %v, err=%v", defVal, err)
	}

	strVal, err := dest.GetString("string")
	if err != nil || strVal == nil || *strVal != "välue" {
		t.Fatalf("expected 'välue', got %v, err=%v", strVal, err)
	}

	var gotDword uint32
	size := win32.DWORD(4)
	status = win32.RegQueryValueEx(dest.Handle, dwordName, nil, unsafe.Slice((*byte)(unsafe.Pointer(&gotDword)), 4), &size)
	if status != 0 || gotDword != 7 {
		t.Fatalf("expected dword 7, got %d, status=%d", gotDword, status)
	}

	exists, err := regkey.Exists(dest.Handle, "deep\\er")
	if err != nil || !exists {
		t.Fatalf("expected deep\\er to exist, got %v, err=%v", exists, err)
	}

	var kind win32.DWORD
	size = 99
	status = win32.RegQueryValueEx(dest.Handle, emptyName, &kind, nil, &size)
	if status != 0 || kind != win32.REG_BINARY || size != 0 {
		t.Fatalf("expected REG_BINARY with size 0, got kind=%d, size=%d, status=%d", kind, size, status)
	}
}
