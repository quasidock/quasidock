package win32

import (
	"syscall"
	"unsafe"
)

var (
	procSetEnvironmentVariableW = modKernel32.NewProc("SetEnvironmentVariableW")
	procExitProcess             = modKernel32.NewProc("ExitProcess")
	procCreateProcessWithLogonW = modAdvapi32.NewProc("CreateProcessWithLogonW")
	procIsProcessInJob          = modKernel32.NewProc("IsProcessInJob")
	procAssignProcessToJobObject = modKernel32.NewProc("AssignProcessToJobObject")
	procResumeThread            = modKernel32.NewProc("ResumeThread")
	procCreateToolhelp32Snapshot = modKernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = modKernel32.NewProc("Process32FirstW")
	procProcess32NextW           = modKernel32.NewProc("Process32NextW")
	procIsTokenRestricted        = modAdvapi32.NewProc("IsTokenRestricted")
	procNtQueryInformationProcess = modNtdll.NewProc("NtQueryInformationProcess")
)

const (
	LOGON_WITH_PROFILE DWORD = 1
	CREATE_SUSPENDED   DWORD = 0x00000004
	TH32CS_SNAPPROCESS DWORD = 0x00000002
	ProcessCommandLineInformation int32 = 60
)

type PROCESSENTRY32W struct {
	DwSize              DWORD
	CntUsage            DWORD
	Th32ProcessID       DWORD
	Th32DefaultHeapID   uintptr
	Th32ModuleID        DWORD
	CntThreads          DWORD
	Th32ParentProcessID DWORD
	PcPriClassBase      int32
	DwFlags             DWORD
	SzExeFile           [260]uint16
}

type UNICODE_STRING struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

func CreateToolhelp32Snapshot(flags, pid DWORD) (HANDLE, error) {
	r, _, err := procCreateToolhelp32Snapshot.Call(uintptr(flags), uintptr(pid))
	if HANDLE(r) == INVALID_HANDLE_VALUE {
		return 0, err
	}
	return HANDLE(r), nil
}

func Process32First(snapshot HANDLE, entry *PROCESSENTRY32W) bool {
	entry.DwSize = DWORD(unsafe.Sizeof(*entry))
	r, _, _ := procProcess32FirstW.Call(snapshot, uintptr(unsafe.Pointer(entry)))
	return r != 0
}

func Process32Next(snapshot HANDLE, entry *PROCESSENTRY32W) bool {
	entry.DwSize = DWORD(unsafe.Sizeof(*entry))
	r, _, _ := procProcess32NextW.Call(snapshot, uintptr(unsafe.Pointer(entry)))
	return r != 0
}

func IsTokenRestricted(token HANDLE) bool {
	r, _, _ := procIsTokenRestricted.Call(token)
	return r != 0
}

func NtQueryInformationProcess(process HANDLE, class int32, info unsafe.Pointer, length uint32, returnLength *uint32) int32 {
	r, _, _ := procNtQueryInformationProcess.Call(
		process,
		uintptr(class),
		uintptr(info),
		uintptr(length),
		uintptr(unsafe.Pointer(returnLength)),
	)
	return int32(r)
}

func IsProcessInJob(process HANDLE, job HANDLE, result *BOOL) bool {
	r, _, _ := procIsProcessInJob.Call(process, job, uintptr(unsafe.Pointer(result)))
	return r != 0
}

func AssignProcessToJobObject(job, process HANDLE) error {
	r, _, err := procAssignProcessToJobObject.Call(job, process)
	if r == 0 {
		return err
	}
	return nil
}

func ResumeThread(thread HANDLE) (DWORD, error) {
	r, _, err := procResumeThread.Call(thread)
	if int32(r) == -1 {
		return 0, err
	}
	return DWORD(r), nil
}

func CreateProcessWithLogon(username, domain, password string, logonFlags DWORD, app, cmdLine string, creationFlags DWORD, env unsafe.Pointer, cwd string, startup *STARTUPINFOW, info *PROCESS_INFORMATION) error {
	userPtr := UTF16Ptr(username)
	var domPtr *uint16
	if domain != "" {
		domPtr = UTF16Ptr(domain)
	}
	passPtr := UTF16Ptr(password)
	var appPtr *uint16
	if app != "" {
		appPtr = UTF16Ptr(app)
	}
	var cmdPtr *uint16
	if cmdLine != "" {
		cmdPtr = UTF16Ptr(cmdLine)
	}
	var cwdPtr *uint16
	if cwd != "" {
		cwdPtr = UTF16Ptr(cwd)
	}
	r, _, err := procCreateProcessWithLogonW.Call(
		uintptr(unsafe.Pointer(userPtr)),
		uintptr(unsafe.Pointer(domPtr)),
		uintptr(unsafe.Pointer(passPtr)),
		uintptr(logonFlags),
		uintptr(unsafe.Pointer(appPtr)),
		uintptr(unsafe.Pointer(cmdPtr)),
		uintptr(creationFlags),
		uintptr(env),
		uintptr(unsafe.Pointer(cwdPtr)),
		uintptr(unsafe.Pointer(startup)),
		uintptr(unsafe.Pointer(info)),
	)
	if r == 0 {
		return err
	}
	return nil
}

func ExitProcess(uExitCode uint32) {
	_, _, _ = procExitProcess.Call(uintptr(uExitCode))
	syscall.Exit(int(uExitCode))
}

func SetEnvironmentVariable(name, value string) error {
	namePtr := UTF16Ptr(name)
	var valPtr *uint16
	if value != "" {
		valPtr = UTF16Ptr(value)
	}
	r, _, err := procSetEnvironmentVariableW.Call(uintptr(unsafe.Pointer(namePtr)), uintptr(unsafe.Pointer(valPtr)))
	if r == 0 {
		return err
	}
	return nil
}

func InitializeProcThreadAttributeList(attrs unsafe.Pointer, count DWORD, flags DWORD, size *uintptr) bool {
	r, _, _ := procInitializeProcThreadAttributeList.Call(uintptr(attrs), uintptr(count), uintptr(flags), uintptr(unsafe.Pointer(size)))
	return r != 0
}

func UpdateProcThreadAttribute(attrs unsafe.Pointer, flags DWORD, attribute uintptr, value unsafe.Pointer, size uintptr, prevValue unsafe.Pointer, returnSize *uintptr) bool {
	r, _, _ := procUpdateProcThreadAttribute.Call(uintptr(attrs), uintptr(flags), attribute, uintptr(value), size, uintptr(prevValue), uintptr(unsafe.Pointer(returnSize)))
	return r != 0
}

func DeleteProcThreadAttributeList(attrs unsafe.Pointer) {
	procDeleteProcThreadAttributeList.Call(uintptr(attrs))
}

func CreateProcess(app, cmd string, procSec, threadSec *SECURITY_ATTRIBUTES, inherit bool, flags DWORD, env unsafe.Pointer, cwd string, startup unsafe.Pointer, info *PROCESS_INFORMATION) error {
	var appPtr, cmdPtr, cwdPtr *uint16
	if app != "" {
		appPtr = UTF16Ptr(app)
	}
	if cmd != "" {
		u := syscall.StringToUTF16(cmd)
		cmdPtr = &u[0]
	}
	if cwd != "" {
		cwdPtr = UTF16Ptr(cwd)
	}
	var inh uintptr
	if inherit {
		inh = 1
	}
	r, _, err := procCreateProcessW.Call(
		uintptr(unsafe.Pointer(appPtr)),
		uintptr(unsafe.Pointer(cmdPtr)),
		uintptr(unsafe.Pointer(procSec)),
		uintptr(unsafe.Pointer(threadSec)),
		inh,
		uintptr(flags),
		uintptr(env),
		uintptr(unsafe.Pointer(cwdPtr)),
		uintptr(startup),
		uintptr(unsafe.Pointer(info)),
	)
	if r == 0 {
		return err
	}
	return nil
}

func CreateProcessAsUser(token HANDLE, app, cmd string, procSec, threadSec *SECURITY_ATTRIBUTES, inherit bool, flags DWORD, env unsafe.Pointer, cwd string, startup unsafe.Pointer, info *PROCESS_INFORMATION) error {
	var appPtr, cmdPtr, cwdPtr *uint16
	if app != "" {
		appPtr = UTF16Ptr(app)
	}
	if cmd != "" {
		u := syscall.StringToUTF16(cmd)
		cmdPtr = &u[0]
	}
	if cwd != "" {
		cwdPtr = UTF16Ptr(cwd)
	}
	var inh uintptr
	if inherit {
		inh = 1
	}
	r, _, err := procCreateProcessAsUserW.Call(
		token,
		uintptr(unsafe.Pointer(appPtr)),
		uintptr(unsafe.Pointer(cmdPtr)),
		uintptr(unsafe.Pointer(procSec)),
		uintptr(unsafe.Pointer(threadSec)),
		inh,
		uintptr(flags),
		uintptr(env),
		uintptr(unsafe.Pointer(cwdPtr)),
		uintptr(startup),
		uintptr(unsafe.Pointer(info)),
	)
	if r == 0 {
		return err
	}
	return nil
}
