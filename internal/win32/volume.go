package win32

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// RequireLocalNTFS deliberately excludes mapped drives and UNC directories.
func RequireLocalNTFS(path string) error {
	if strings.HasPrefix(path, `\\`) {
		return fmt.Errorf("mount storage must be on a local NTFS volume: %s", path)
	}
	var root [32768]uint16
	r, _, e := modKernel32.NewProc("GetVolumePathNameW").Call(uintptr(unsafe.Pointer(UTF16Ptr(path))), uintptr(unsafe.Pointer(&root[0])), uintptr(len(root)))
	if r == 0 {
		return fmt.Errorf("checking mount filesystem: %w", e)
	}
	drive, _, _ := modKernel32.NewProc("GetDriveTypeW").Call(uintptr(unsafe.Pointer(&root[0])))
	var fs [64]uint16
	r, _, e = modKernel32.NewProc("GetVolumeInformationW").Call(uintptr(unsafe.Pointer(&root[0])), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&fs[0])), uintptr(len(fs)))
	if r == 0 {
		return fmt.Errorf("checking mount filesystem: %w", e)
	}
	if drive == 4 || !strings.EqualFold(syscall.UTF16ToString(fs[:]), "NTFS") {
		return fmt.Errorf("mount storage must be on a local NTFS volume: %s", path)
	}
	return nil
}
