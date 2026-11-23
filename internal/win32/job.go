package win32

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

const jobObjectFreezeInformation int32 = 18

// Private Windows ABI, as defined in System Informer's phnt/ntpsapi.h:
// https://github.com/winsiderss/phnt/blob/master/ntpsapi.h
// BOOLEAN fields are single bytes, unlike Win32 BOOL.
type jobFreezeInformation struct {
	Flags          uint32
	Freeze         uint8
	Swap           uint8
	Reserved       [2]uint8
	HighEdgeFilter uint32
	LowEdgeFilter  uint32
}

// SetJobFrozen freezes or thaws every process in a job, including nested jobs.
// The owner must remain outside the job so it can thaw or terminate it.
func SetJobFrozen(job HANDLE, frozen bool) error {
	info := jobFreezeInformation{Flags: 1} // FreezeOperation; preserve other settings.
	if frozen {
		info.Freeze = 1
	}
	err := SetInformationJobObject(job, jobObjectFreezeInformation, unsafe.Pointer(&info), DWORD(unsafe.Sizeof(info)))
	if errors.Is(err, syscall.Errno(50)) || errors.Is(err, syscall.Errno(87)) {
		return fmt.Errorf("Windows job freezing is unsupported: %w", err)
	}
	return err
}

// IsJobFrozen queries the job's own freeze state.
func IsJobFrozen(job HANDLE) (bool, error) {
	var info jobFreezeInformation
	err := QueryInformationJobObject(job, jobObjectFreezeInformation, unsafe.Pointer(&info), DWORD(unsafe.Sizeof(info)))
	return info.Freeze != 0, err
}
