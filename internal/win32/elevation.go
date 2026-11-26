package win32

import (
	"fmt"
	"runtime"
	"unsafe"
)

const (
	PROCESS_DUP_HANDLE DWORD = 0x0040
	HighIntegrityRID   DWORD = 0x3000
)

var (
	procShellExecuteExW       = modShell32.NewProc("ShellExecuteExW")
	procGetNamedPipeClientPID = modKernel32.NewProc("GetNamedPipeClientProcessId")
	procGetNamedPipeServerPID = modKernel32.NewProc("GetNamedPipeServerProcessId")
	procDuplicateRemoteHandle = modKernel32.NewProc("DuplicateHandle")
)

// StartElevated opens the Windows consent dialog for an absolute executable.
// The caller owns the returned process handle.
func StartElevated(exe, parameters, directory string) (HANDLE, error) {
	const (
		seeMaskNoCloseProcess = 0x40
		seeMaskNoAsync        = 0x100
		seeMaskFlagNoUI       = 0x400
		coinitDisableOLE1DDE  = 4
	)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if hr := CoInitializeEx(0, COINIT_APARTMENTTHREADED|coinitDisableOLE1DDE); hr >= 0 {
		defer CoUninitialize()
	}
	info := struct {
		size, mask                        DWORD
		window                            HANDLE
		verb, file, parameters, directory *uint16
		show                              int32
		instance, idList                  HANDLE
		class                             *uint16
		classKey                          HANDLE
		hotkey                            DWORD
		monitor, process                  HANDLE
	}{
		mask:       seeMaskNoCloseProcess | seeMaskNoAsync | seeMaskFlagNoUI,
		verb:       UTF16Ptr("runas"),
		file:       UTF16Ptr(exe),
		parameters: UTF16Ptr(parameters),
		directory:  UTF16Ptr(directory),
	}
	info.size = DWORD(unsafe.Sizeof(info))
	result, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	runtime.KeepAlive(info)
	if result == 0 {
		return 0, err
	}
	if info.process == 0 {
		return 0, fmt.Errorf("elevated startup did not return a process handle")
	}
	return info.process, nil
}

func PipeClientProcessID(pipe HANDLE) (DWORD, error) {
	var pid DWORD
	if ok, _, err := procGetNamedPipeClientPID.Call(pipe, uintptr(unsafe.Pointer(&pid))); ok == 0 {
		return 0, err
	}
	return pid, nil
}

func PipeServerProcessID(pipe HANDLE) (DWORD, error) {
	var pid DWORD
	if ok, _, err := procGetNamedPipeServerPID.Call(pipe, uintptr(unsafe.Pointer(&pid))); ok == 0 {
		return 0, err
	}
	return pid, nil
}

func DuplicateProcessHandle(process, source HANDLE) (HANDLE, error) {
	const duplicateSameAccess = 2
	var handle HANDLE
	if ok, _, err := procDuplicateRemoteHandle.Call(process, source, GetCurrentProcess(), uintptr(unsafe.Pointer(&handle)), 0, 0, duplicateSameAccess); ok == 0 {
		return 0, err
	}
	return handle, nil
}
