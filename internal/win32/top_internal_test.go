package win32

import (
	"os"
	"testing"
	"unsafe"
)

func TestTopRejectsProcessOutsideJob(t *testing.T) {
	job, err := CreateJobObject("")
	if err != nil {
		t.Fatal(err)
	}
	defer CloseHandle(job)
	// An enumerated PID reused by a host process must not expose its details.
	info, err := queryJobProcess(job, uint32(os.Getpid()), nil, 0)
	if err != nil || info != nil {
		t.Fatalf("outside member: %+v %v", info, err)
	}
	if info, err := queryJobProcess(job, 0, nil, 0); err != nil || info != nil {
		t.Fatalf("vanished PID: %+v %v", info, err)
	}
	if _, _, _, err := GetProcessTimes(0); err == nil {
		t.Fatal("invalid process accepted")
	}
	count := func() DWORD {
		t.Helper()
		var n DWORD
		ok, _, err := modKernel32.NewProc("GetProcessHandleCount").Call(GetCurrentProcess(), uintptr(unsafe.Pointer(&n)))
		if ok == 0 {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	for range 100 {
		if _, err := queryJobProcess(job, uint32(os.Getpid()), nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if after := count(); after > before+2 {
		t.Fatalf("process handles leaked: before %d, after %d", before, after)
	}
}
