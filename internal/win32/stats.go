package win32

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

var (
	procGetProcessMemoryInfo    = modKernel32.NewProc("K32GetProcessMemoryInfo")
	procGetActiveProcessorCount = modKernel32.NewProc("GetActiveProcessorCount")
)

// JobStats contains current memory/process usage and lifetime CPU/I/O counters.
// CPUTime counts 100-nanosecond ticks, including processes that have exited.
type JobStats struct {
	CPUTime       uint64
	PrivateMemory uint64
	ReadBytes     uint64
	WriteBytes    uint64
	PIDs          uint32
}

type jobAccounting struct {
	Basic JOBOBJECT_BASIC_ACCOUNTING_INFORMATION
	IO    IO_COUNTERS
}

type processMemoryCounters struct {
	Size, PageFaults                                         DWORD
	PeakWorkingSet, WorkingSet                               uintptr
	PeakPagedPool, PagedPool, PeakNonPagedPool, NonPagedPool uintptr
	Pagefile, PeakPagefile, PrivateUsage                     uintptr
}

// QueryJobProcessIDs includes processes in nested jobs. Grow and retry when
// processes arrive during enumeration, with a bound on allocation and churn.
func QueryJobProcessIDs(job HANDLE) ([]uint32, error) {
	for capacity := 64; capacity <= 1<<20; capacity *= 2 {
		header := int(8 / unsafe.Sizeof(uintptr(0)))
		buffer := make([]uintptr, header+capacity)
		err := QueryInformationJobObject(job, 3, unsafe.Pointer(&buffer[0]), DWORD(len(buffer)*int(unsafe.Sizeof(uintptr(0)))))
		counts := (*[2]uint32)(unsafe.Pointer(&buffer[0]))
		if errors.Is(err, syscall.ERROR_MORE_DATA) || (err == nil && counts[1] < counts[0]) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if counts[1] > uint32(capacity) {
			return nil, errors.New("invalid job process list size")
		}
		ids := make([]uint32, counts[1])
		for i := range ids {
			ids[i] = uint32(buffer[header+i])
		}
		return ids, nil
	}
	return nil, errors.New("job process list keeps growing")
}

func privateProcessMemory(job HANDLE, pid uint32) (uint64, error) {
	process, err := OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION|SYNCHRONIZE, false, pid)
	if err != nil {
		if errors.Is(err, syscall.Errno(87)) { // PID exited before OpenProcess.
			return 0, nil
		}
		// Exiting processes can also deny access before their PID disappears.
		ids, listErr := QueryJobProcessIDs(job)
		if listErr == nil {
			found := false
			for _, id := range ids {
				found = found || id == pid
			}
			if !found {
				return 0, nil
			}
		}
		return 0, err
	}
	defer CloseHandle(process)
	if WaitForSingleObject(process, 0) == WAIT_OBJECT_0 {
		return 0, nil
	}
	var member BOOL
	ok, _, err := procIsProcessInJob.Call(process, job, uintptr(unsafe.Pointer(&member)))
	if ok == 0 {
		return 0, err
	}
	if member == 0 { // The PID was reused by a process outside this job.
		return 0, nil
	}
	var memory processMemoryCounters
	memory.Size = DWORD(unsafe.Sizeof(memory))
	ok, _, err = procGetProcessMemoryInfo.Call(process, uintptr(unsafe.Pointer(&memory)), uintptr(memory.Size))
	if ok == 0 {
		if WaitForSingleObject(process, 0) == WAIT_OBJECT_0 {
			return 0, nil
		}
		return 0, err
	}
	return uint64(memory.PrivateUsage), nil
}

// QueryJobStats samples the parent job once: nested exec jobs are already
// included. Memory is a best-effort point-in-time sum, not an atomic snapshot.
func QueryJobStats(job HANDLE) (JobStats, error) {
	var accounting jobAccounting
	if err := QueryInformationJobObject(job, 8, unsafe.Pointer(&accounting), DWORD(unsafe.Sizeof(accounting))); err != nil {
		return JobStats{}, fmt.Errorf("query job accounting: %w", err)
	}
	stats := JobStats{
		CPUTime:   uint64(accounting.Basic.TotalUserTime + accounting.Basic.TotalKernelTime),
		ReadBytes: accounting.IO.ReadTransferCount, WriteBytes: accounting.IO.WriteTransferCount,
		PIDs: accounting.Basic.ActiveProcesses,
	}
	ids, err := QueryJobProcessIDs(job)
	if err != nil {
		return JobStats{}, fmt.Errorf("list job processes: %w", err)
	}
	for _, pid := range ids {
		memory, err := privateProcessMemory(job, pid)
		if err != nil {
			return JobStats{}, fmt.Errorf("query process %d memory: %w", pid, err)
		}
		stats.PrivateMemory += memory
	}
	return stats, nil
}

// ActiveProcessorCount covers every processor group, regardless of Go's
// GOMAXPROCS setting or the CLI process's affinity.
func ActiveProcessorCount() (uint32, error) {
	n, _, err := procGetActiveProcessorCount.Call(0xffff) // ALL_PROCESSOR_GROUPS
	if n == 0 {
		return 0, err
	}
	return uint32(n), nil
}
