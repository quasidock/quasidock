package win32

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	pipeAccessDuplex          DWORD         = 3
	pipeRejectRemoteClients   DWORD         = 8
	fileFlagFirstPipeInstance DWORD         = 0x00080000
	errorPipeBusy             syscall.Errno = 231
	errorPipeConnected        syscall.Errno = 535
)

var (
	procCreateNamedPipeW                = modKernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe                = modKernel32.NewProc("ConnectNamedPipe")
	procGetOverlappedResult             = modKernel32.NewProc("GetOverlappedResult")
	procConvertStringSecurityDescriptor = modAdvapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
)

// PipeListener serves local, current-user-only, overlapped byte streams. Files
// returned by Accept and DialPipe use Go's poller, so Close cancels blocked I/O.
type PipeListener struct {
	mu        sync.Mutex
	name      string
	sd        uintptr
	pending   HANDLE
	accepting bool
	closed    bool
}

func ListenPipe(name string) (*PipeListener, error) {
	token, err := OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY)
	if err != nil {
		return nil, err
	}
	defer CloseHandle(token)
	sidBytes, userSid, err := GetTokenUserSid(token)
	if err != nil {
		return nil, err
	}
	sid, err := ConvertSidToStringSid(userSid)
	runtime.KeepAlive(sidBytes)
	if err != nil {
		return nil, err
	}
	var sd uintptr
	sddl := UTF16Ptr("D:P(A;;GA;;;" + sid + ")")
	if ok, _, err := procConvertStringSecurityDescriptor.Call(uintptr(unsafe.Pointer(sddl)), 1, uintptr(unsafe.Pointer(&sd)), 0); ok == 0 {
		return nil, err
	}
	l := &PipeListener{name: name, sd: sd}
	l.pending, err = l.create(true)
	if err != nil {
		LocalFree(sd)
		return nil, err
	}
	return l, nil
}

func (l *PipeListener) create(first bool) (HANDLE, error) {
	sec := SECURITY_ATTRIBUTES{NLength: DWORD(unsafe.Sizeof(SECURITY_ATTRIBUTES{})), LpSecurityDescriptor: l.sd}
	flags := pipeAccessDuplex | FILE_FLAG_OVERLAPPED
	if first {
		flags |= fileFlagFirstPipeInstance
	}
	name := UTF16Ptr(l.name)
	h, _, err := procCreateNamedPipeW.Call(uintptr(unsafe.Pointer(name)), uintptr(flags), uintptr(pipeRejectRemoteClients), 255, 64*1024, 64*1024, 0, uintptr(unsafe.Pointer(&sec)))
	if h == INVALID_HANDLE_VALUE {
		return 0, err
	}
	return h, nil
}

// Accept must be called by only one goroutine. Close may run concurrently.
func (l *PipeListener) Accept() (*os.File, error) {
	event, err := CreateEvent("", true, false)
	if err != nil {
		return nil, err
	}
	defer CloseHandle(event)
	ov := OVERLAPPED{HEvent: event}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, os.ErrClosed
	}
	h := l.pending
	l.accepting = true
	ok, _, connectErr := procConnectNamedPipe.Call(h, uintptr(unsafe.Pointer(&ov)))
	l.mu.Unlock()
	if ok == 0 && errors.Is(connectErr, syscall.ERROR_IO_PENDING) {
		WaitForSingleObject(event, INFINITE)
		var transferred DWORD
		ok, _, connectErr = procGetOverlappedResult.Call(h, uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&transferred)), 1)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accepting = false
	l.pending = 0
	if l.closed || (ok == 0 && !errors.Is(connectErr, errorPipeConnected)) {
		CloseHandle(h)
		if l.closed {
			return nil, os.ErrClosed
		}
		return nil, connectErr
	}
	l.pending, err = l.create(false)
	if err != nil {
		CloseHandle(h)
		return nil, err
	}
	return os.NewFile(h, l.name), nil
}

func (l *PipeListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		if l.accepting {
			CancelIoEx(l.pending, nil)
		} else if l.pending != 0 {
			CloseHandle(l.pending)
			l.pending = 0
		}
		LocalFree(l.sd)
	}
	return nil
}

func DialPipe(name string, deadline time.Time) (*os.File, error) {
	return dialPipe(name, deadline, 0)
}

// DialPipeIdentification lets the server identify the client without granting
// it an impersonation token. Elevated clients use this on user-owned pipes.
func DialPipeIdentification(name string, deadline time.Time) (*os.File, error) {
	const securityIdentification = 0x00110000 // SECURITY_SQOS_PRESENT | SECURITY_IDENTIFICATION
	return dialPipe(name, deadline, securityIdentification)
}

func dialPipe(name string, deadline time.Time, securityFlags DWORD) (*os.File, error) {
	for {
		h, err := CreateFile(name, GENERIC_READ|GENERIC_WRITE, 0, nil, OPEN_EXISTING, FILE_FLAG_OVERLAPPED|securityFlags, 0)
		if err == nil {
			return os.NewFile(h, name), nil
		}
		if !errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) && !errors.Is(err, errorPipeBusy) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, os.ErrDeadlineExceeded
		}
		time.Sleep(10 * time.Millisecond)
	}
}
