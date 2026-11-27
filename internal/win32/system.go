package win32

import (
	"fmt"
	"unsafe"
)

var (
	procRtlGetVersion        = modNtdll.NewProc("RtlGetVersion")
	procGlobalMemoryStatusEx = modKernel32.NewProc("GlobalMemoryStatusEx")
)

func OSVersion() (string, error) {
	var info struct {
		Size, Major, Minor, Build, Platform uint32
		ServicePack                         [128]uint16
	}
	info.Size = uint32(unsafe.Sizeof(info))
	status, _, _ := procRtlGetVersion.Call(uintptr(unsafe.Pointer(&info)))
	if status != 0 {
		return "", fmt.Errorf("RtlGetVersion returned NTSTATUS %#x", uint32(status))
	}
	return fmt.Sprintf("%d.%d.%d", info.Major, info.Minor, info.Build), nil
}

func TotalPhysicalMemory() (uint64, error) {
	var info struct {
		Size, Load                       uint32
		TotalPhysical, AvailablePhysical uint64
		TotalPageFile, AvailablePageFile uint64
		TotalVirtual, AvailableVirtual   uint64
		AvailableExtendedVirtual         uint64
	}
	info.Size = uint32(unsafe.Sizeof(info))
	ok, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return 0, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	return info.TotalPhysical, nil
}
