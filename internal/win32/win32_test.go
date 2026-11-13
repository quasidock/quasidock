package win32_test

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/quasidock/quasidock/internal/win32"
)

func TestWTF8Encoding(t *testing.T) {
	tests := []string{
		"Hello, World!",
		"",
		"C:\\Program Files\\Quasidock\\bin.exe",
		"日本語とEmoji 🎉🚀",
		"Path with spaces and symbols: (foo) [bar] {baz} ~!@#$%^&*()_+",
	}

	for _, s := range tests {
		u16 := win32.UTF8ToUTF16(s)
		back := win32.UTF16ToUTF8(u16)
		if back != s {
			t.Fatalf("UTF8 roundtrip mismatch: expected %q, got %q", s, back)
		}
	}
}

func TestGetModuleFileName(t *testing.T) {
	path, err := win32.GetModuleFileName()
	if err != nil {
		t.Fatalf("GetModuleFileName failed: %v", err)
	}
	if path == "" {
		t.Fatalf("GetModuleFileName returned empty string")
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("GetModuleFileName returned non-absolute path: %s", path)
	}
}

func TestJobObject(t *testing.T) {
	job, err := win32.CreateJobObject("")
	if err != nil {
		t.Fatalf("CreateJobObject failed: %v", err)
	}
	defer win32.CloseHandle(job)

	var limit win32.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limit.BasicLimitInformation.LimitFlags = win32.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	err = win32.SetInformationJobObject(job, win32.JobObjectExtendedLimitInformation, unsafe.Pointer(&limit), win32.DWORD(unsafe.Sizeof(limit)))
	if err != nil {
		t.Fatalf("SetInformationJobObject failed: %v", err)
	}
}

func TestGetFileAttributes(t *testing.T) {
	tempDir := t.TempDir()
	attrs, err := win32.GetFileAttributes(tempDir)
	if err != nil || attrs == win32.INVALID_FILE_ATTRIBUTES {
		t.Fatalf("GetFileAttributes failed on tempDir: %v", err)
	}
	if (attrs & win32.FILE_ATTRIBUTE_DIRECTORY) == 0 {
		t.Fatalf("tempDir expected to have FILE_ATTRIBUTE_DIRECTORY")
	}
}

func TestSynchronousIo(t *testing.T) {
	// Create a temp file and test SynchronousIo
	tempFile := filepath.Join(t.TempDir(), "test.txt")
	err := os.WriteFile(tempFile, []byte("hello"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	h, err := win32.CreateFile(tempFile, win32.GENERIC_READ, win32.FILE_SHARE_READ, nil, win32.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatalf("CreateFile failed: %v", err)
	}
	defer win32.CloseHandle(h)

	sync, err := win32.SynchronousIo(h)
	if err != nil {
		t.Fatalf("SynchronousIo failed: %v", err)
	}
	if !sync {
		t.Fatalf("expected synchronous IO on normal file handle")
	}
}
