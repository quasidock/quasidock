package win32

import (
	"errors"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	procSetEnvironmentVariableW   = modKernel32.NewProc("SetEnvironmentVariableW")
	procExitProcess               = modKernel32.NewProc("ExitProcess")
	procCreateProcessWithLogonW   = modAdvapi32.NewProc("CreateProcessWithLogonW")
	procIsProcessInJob            = modKernel32.NewProc("IsProcessInJob")
	procAssignProcessToJobObject  = modKernel32.NewProc("AssignProcessToJobObject")
	procResumeThread              = modKernel32.NewProc("ResumeThread")
	procCreateToolhelp32Snapshot  = modKernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW           = modKernel32.NewProc("Process32FirstW")
	procProcess32NextW            = modKernel32.NewProc("Process32NextW")
	procIsTokenRestricted         = modAdvapi32.NewProc("IsTokenRestricted")
	procNtQueryInformationProcess = modNtdll.NewProc("NtQueryInformationProcess")
)

const (
	LOGON_WITH_PROFILE            DWORD = 1
	CREATE_SUSPENDED              DWORD = 0x00000004
	TH32CS_SNAPPROCESS            DWORD = 0x00000002
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

func QueryProcessCommandLine(process HANDLE) (string, bool) {
	// A maximum-length UTF-16 command needs 64 KiB plus its UNICODE_STRING
	// header. A 64 KiB buffer alone drops otherwise valid long command lines.
	infoBuf := make([]byte, 65536+int(unsafe.Sizeof(UNICODE_STRING{})))
	var retLen uint32
	status := NtQueryInformationProcess(process, ProcessCommandLineInformation, unsafe.Pointer(&infoBuf[0]), uint32(len(infoBuf)), &retLen)
	if status < 0 {
		return "", false
	}
	uStr := (*UNICODE_STRING)(unsafe.Pointer(&infoBuf[0]))
	if uStr.Buffer == nil || uStr.Length == 0 {
		runtime.KeepAlive(infoBuf)
		return "", false
	}
	wSlice := unsafe.Slice(uStr.Buffer, uStr.Length/2)
	line := UTF16ToUTF8(wSlice)
	runtime.KeepAlive(infoBuf)
	return line, true
}

func UCRTArgs() ([]string, bool) {
	ucrt := syscall.NewLazyDLL("ucrtbase.dll")
	if err := ucrt.Load(); err != nil {
		return nil, false
	}
	// _crt_argv_unexpanded_arguments = 1 parses with UCRT rules without loading shell32.dll.
	r, _, _ := ucrt.NewProc("_configure_wide_argv").Call(1)
	if r != 0 {
		return nil, false
	}
	pArgc, _, _ := ucrt.NewProc("__p___argc").Call()
	pArgv, _, _ := ucrt.NewProc("__p___wargv").Call()
	if pArgc == 0 || pArgv == 0 {
		return nil, false
	}
	var argc int32
	_, _, _ = procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&argc)), pArgc, unsafe.Sizeof(argc))
	var argv **uint16
	_, _, _ = procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&argv)), pArgv, unsafe.Sizeof(argv))
	if argc <= 0 || argv == nil {
		return nil, false
	}
	arr := unsafe.Slice(argv, argc)
	out := make([]string, argc)
	for i, p := range arr {
		out[i] = UTF16PtrToString(p)
	}
	return out, true
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

func CreateProcessWithLogon(username, domain, password string, logonFlags DWORD, app, cmdLine string, creationFlags DWORD, env []uint16, cwd string, startup *STARTUPINFOW, info *PROCESS_INFORMATION) error {
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
	var envPtr unsafe.Pointer
	if len(env) > 0 {
		envPtr = unsafe.Pointer(&env[0])
	}
	var si STARTUPINFOW
	if startup != nil {
		si = *startup
	}
	si.Cb = DWORD(unsafe.Sizeof(si))
	r, _, err := procCreateProcessWithLogonW.Call(
		uintptr(unsafe.Pointer(userPtr)),
		uintptr(unsafe.Pointer(domPtr)),
		uintptr(unsafe.Pointer(passPtr)),
		uintptr(logonFlags),
		uintptr(unsafe.Pointer(appPtr)),
		uintptr(unsafe.Pointer(cmdPtr)),
		uintptr(creationFlags),
		uintptr(envPtr),
		uintptr(unsafe.Pointer(cwdPtr)),
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(info)),
	)
	runtime.KeepAlive(env)
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
	_, _, _ = procDeleteProcThreadAttributeList.Call(uintptr(attrs))
}

type ProcThreadAttributeList struct {
	buf       []byte
	keepAlive []any
}

func NewProcThreadAttributeList(count DWORD) (*ProcThreadAttributeList, error) {
	var size uintptr
	InitializeProcThreadAttributeList(nil, count, 0, &size)
	if size == 0 {
		return nil, errors.New("InitializeProcThreadAttributeList returned zero size")
	}
	buf := make([]byte, size)
	if !InitializeProcThreadAttributeList(unsafe.Pointer(&buf[0]), count, 0, &size) {
		return nil, errors.New("InitializeProcThreadAttributeList failed")
	}
	return &ProcThreadAttributeList{buf: buf}, nil
}

func (l *ProcThreadAttributeList) Delete() {
	if l != nil && len(l.buf) > 0 {
		DeleteProcThreadAttributeList(unsafe.Pointer(&l.buf[0]))
		runtime.KeepAlive(l)
		l.buf = nil
		l.keepAlive = nil
	}
}

func (l *ProcThreadAttributeList) Ptr() uintptr {
	if l == nil || len(l.buf) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&l.buf[0]))
}

func (l *ProcThreadAttributeList) SetJobs(jobs []HANDLE) bool {
	if len(jobs) == 0 {
		return true
	}
	l.keepAlive = append(l.keepAlive, jobs)
	return UpdateProcThreadAttribute(unsafe.Pointer(&l.buf[0]), 0, PROC_THREAD_ATTRIBUTE_JOB_LIST, unsafe.Pointer(&jobs[0]), uintptr(len(jobs))*unsafe.Sizeof(jobs[0]), nil, nil)
}

func (l *ProcThreadAttributeList) SetHandles(handles []HANDLE) bool {
	if len(handles) == 0 {
		return true
	}
	l.keepAlive = append(l.keepAlive, handles)
	return UpdateProcThreadAttribute(unsafe.Pointer(&l.buf[0]), 0, PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0]), nil, nil)
}

func (l *ProcThreadAttributeList) SetSecurityCapabilities(sec *SECURITY_CAPABILITIES) bool {
	if sec == nil {
		return true
	}
	l.keepAlive = append(l.keepAlive, sec)
	return UpdateProcThreadAttribute(unsafe.Pointer(&l.buf[0]), 0, PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES, unsafe.Pointer(sec), unsafe.Sizeof(*sec), nil, nil)
}

func (l *ProcThreadAttributeList) SetPseudoConsole(pc uintptr) bool {
	r, _, _ := procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(&l.buf[0])),
		0,
		PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		pc,
		unsafe.Sizeof(pc),
		0,
		0,
	)
	return r != 0
}

func createProcessRaw(token HANDLE, hasToken bool, app, cmd string, procSec, threadSec *SECURITY_ATTRIBUTES, inherit bool, flags DWORD, env []uint16, cwd string, startup unsafe.Pointer, info *PROCESS_INFORMATION) error {
	var appPtr, cmdPtr, cwdPtr *uint16
	if app != "" {
		appPtr = UTF16Ptr(app)
	}
	if cmd != "" {
		u, err := syscall.UTF16FromString(cmd)
		if err != nil {
			return err
		}
		cmdPtr = &u[0]
	}
	if cwd != "" {
		cwdPtr = UTF16Ptr(cwd)
	}
	var inh uintptr
	if inherit {
		inh = 1
	}
	var envPtr unsafe.Pointer
	if len(env) > 0 {
		envPtr = unsafe.Pointer(&env[0])
	}
	var r uintptr
	var err error
	if hasToken {
		r, _, err = procCreateProcessAsUserW.Call(
			token,
			uintptr(unsafe.Pointer(appPtr)),
			uintptr(unsafe.Pointer(cmdPtr)),
			uintptr(unsafe.Pointer(procSec)),
			uintptr(unsafe.Pointer(threadSec)),
			inh,
			uintptr(flags),
			uintptr(envPtr),
			uintptr(unsafe.Pointer(cwdPtr)),
			uintptr(startup),
			uintptr(unsafe.Pointer(info)),
		)
	} else {
		r, _, err = procCreateProcessW.Call(
			uintptr(unsafe.Pointer(appPtr)),
			uintptr(unsafe.Pointer(cmdPtr)),
			uintptr(unsafe.Pointer(procSec)),
			uintptr(unsafe.Pointer(threadSec)),
			inh,
			uintptr(flags),
			uintptr(envPtr),
			uintptr(unsafe.Pointer(cwdPtr)),
			uintptr(startup),
			uintptr(unsafe.Pointer(info)),
		)
	}
	runtime.KeepAlive(env)
	runtime.KeepAlive(appPtr)
	runtime.KeepAlive(cmdPtr)
	runtime.KeepAlive(cwdPtr)
	if r == 0 {
		return err
	}
	return nil
}

func CreateProcess(app, cmd string, procSec, threadSec *SECURITY_ATTRIBUTES, inherit bool, flags DWORD, env []uint16, cwd string, startup *STARTUPINFOW, info *PROCESS_INFORMATION) error {
	var si STARTUPINFOW
	if startup != nil {
		si = *startup
	}
	si.Cb = DWORD(unsafe.Sizeof(si))
	return createProcessRaw(0, false, app, cmd, procSec, threadSec, inherit, flags, env, cwd, unsafe.Pointer(&si), info)
}

func CreateProcessEx(app, cmd string, procSec, threadSec *SECURITY_ATTRIBUTES, inherit bool, flags DWORD, env []uint16, cwd string, startup *STARTUPINFOEXW, attrs *ProcThreadAttributeList, info *PROCESS_INFORMATION) error {
	var si STARTUPINFOEXW
	if startup != nil {
		si = *startup
	}
	si.StartupInfo.Cb = DWORD(unsafe.Sizeof(si))
	if attrs != nil {
		si.LpAttributeList = attrs.Ptr()
	}
	err := createProcessRaw(0, false, app, cmd, procSec, threadSec, inherit, flags|EXTENDED_STARTUPINFO_PRESENT, env, cwd, unsafe.Pointer(&si), info)
	runtime.KeepAlive(attrs)
	return err
}

func CreateProcessAsUser(token HANDLE, app, cmd string, procSec, threadSec *SECURITY_ATTRIBUTES, inherit bool, flags DWORD, env []uint16, cwd string, startup *STARTUPINFOW, info *PROCESS_INFORMATION) error {
	var si STARTUPINFOW
	if startup != nil {
		si = *startup
	}
	si.Cb = DWORD(unsafe.Sizeof(si))
	return createProcessRaw(token, true, app, cmd, procSec, threadSec, inherit, flags, env, cwd, unsafe.Pointer(&si), info)
}

func CreateProcessAsUserEx(token HANDLE, app, cmd string, procSec, threadSec *SECURITY_ATTRIBUTES, inherit bool, flags DWORD, env []uint16, cwd string, startup *STARTUPINFOEXW, attrs *ProcThreadAttributeList, info *PROCESS_INFORMATION) error {
	var si STARTUPINFOEXW
	if startup != nil {
		si = *startup
	}
	si.StartupInfo.Cb = DWORD(unsafe.Sizeof(si))
	if attrs != nil {
		si.LpAttributeList = attrs.Ptr()
	}
	err := createProcessRaw(token, true, app, cmd, procSec, threadSec, inherit, flags|EXTENDED_STARTUPINFO_PRESENT, env, cwd, unsafe.Pointer(&si), info)
	runtime.KeepAlive(attrs)
	return err
}
