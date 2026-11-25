package win32

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"syscall"
	"unsafe"
)

var procGetProcessTimes = modKernel32.NewProc("GetProcessTimes")

// ProcessInfo describes a live job member. Nil counters mean that Windows did
// not provide that detail. CPUTime is cumulative kernel/user time in 100ns ticks.
type ProcessInfo struct {
	PID     uint32
	PPID    *uint32
	CPUTime *uint64
	Command string
}

func filetimeTicks(t FILETIME) uint64 {
	return uint64(t.DwHighDateTime)<<32 | uint64(t.DwLowDateTime)
}

func GetProcessTimes(process HANDLE) (created, kernel, user FILETIME, err error) {
	var exited FILETIME
	ok, _, callErr := procGetProcessTimes.Call(process,
		uintptr(unsafe.Pointer(&created)), uintptr(unsafe.Pointer(&exited)),
		uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if ok == 0 {
		err = callErr
	}
	return
}

// processEntries retains the snapshot's start time to avoid applying metadata
// from an old PID to a process created after the snapshot began.
func processEntries() (map[uint32]PROCESSENTRY32W, uint64, error) {
	var started syscall.Filetime
	syscall.GetSystemTimeAsFileTime(&started)
	snapshot, err := CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, 0, err
	}
	defer CloseHandle(snapshot)
	entries := make(map[uint32]PROCESSENTRY32W)
	var entry PROCESSENTRY32W
	entry.DwSize = DWORD(unsafe.Sizeof(entry))
	ok, _, callErr := procProcess32FirstW.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	for ok != 0 {
		entries[entry.Th32ProcessID] = entry
		ok, _, callErr = procProcess32NextW.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	}
	if !errors.Is(callErr, syscall.ERROR_NO_MORE_FILES) {
		return nil, 0, callErr
	}
	return entries, uint64(started.HighDateTime)<<32 | uint64(started.LowDateTime), nil
}

// queryJobProcess uses a handle to verify membership before reading details;
// PIDs can disappear or be reused between enumeration and OpenProcess.
func queryJobProcess(job HANDLE, pid uint32, entries map[uint32]PROCESSENTRY32W, snapshotAt uint64) (*ProcessInfo, error) {
	handle, err := OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION|SYNCHRONIZE, false, pid)
	if err != nil {
		if errors.Is(err, syscall.Errno(87)) {
			return nil, nil // Exited before OpenProcess.
		}
		ids, listErr := QueryJobProcessIDs(job)
		if listErr == nil && !slices.Contains(ids, pid) {
			return nil, nil // Exiting processes may temporarily deny access.
		}
		return nil, err
	}
	defer CloseHandle(handle)
	if WaitForSingleObject(handle, 0) == WAIT_OBJECT_0 {
		return nil, nil
	}
	var member BOOL
	ok, _, err := procIsProcessInJob.Call(handle, job, uintptr(unsafe.Pointer(&member)))
	if ok == 0 {
		return nil, err
	}
	if member == 0 {
		return nil, nil
	}
	info := &ProcessInfo{PID: pid, Command: "--"}
	created, kernel, user, timeErr := GetProcessTimes(handle)
	if timeErr == nil {
		ticks := filetimeTicks(kernel) + filetimeTicks(user)
		info.CPUTime = &ticks
	}
	// Only use snapshot metadata when process creation confirms its identity.
	if entry, found := entries[pid]; found && timeErr == nil && filetimeTicks(created) <= snapshotAt {
		parent := uint32(entry.Th32ParentProcessID)
		info.PPID = &parent
		info.Command = syscall.UTF16ToString(entry.SzExeFile[:])
	}
	if command, known := QueryProcessCommandLine(handle); known {
		info.Command = command
	}
	if WaitForSingleObject(handle, 0) == WAIT_OBJECT_0 {
		return nil, nil
	}
	return info, nil
}

// QueryJobProcesses samples job members, including nested jobs, without freezing
// the application. Individual processes may exit during this best-effort scan.
func QueryJobProcesses(cancel context.Context, job HANDLE) ([]ProcessInfo, error) {
	if err := cancel.Err(); err != nil {
		return nil, err
	}
	ids, err := QueryJobProcessIDs(job)
	if err != nil {
		return nil, fmt.Errorf("list job processes: %w", err)
	}
	processes := make([]ProcessInfo, 0, len(ids))
	if len(ids) == 0 {
		return processes, nil
	}
	entries, snapshotAt, err := processEntries()
	if err != nil {
		return nil, fmt.Errorf("snapshot processes: %w", err)
	}
	for _, pid := range ids {
		if err := cancel.Err(); err != nil {
			return nil, err
		}
		info, err := queryJobProcess(job, pid, entries, snapshotAt)
		if err != nil {
			return nil, fmt.Errorf("query process %d: %w", pid, err)
		}
		if info != nil {
			processes = append(processes, *info)
		}
	}
	slices.SortFunc(processes, func(a, b ProcessInfo) int { return cmp.Compare(a.PID, b.PID) })
	return processes, nil
}
