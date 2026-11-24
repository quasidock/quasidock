package win32

import (
	"errors"
	"syscall"
)

var (
	procGetCurrentThreadId  = modKernel32.NewProc("GetCurrentThreadId")
	procOpenThread          = modKernel32.NewProc("OpenThread")
	procCancelSynchronousIo = modKernel32.NewProc("CancelSynchronousIo")
)

// OpenCurrentThreadForCancellation must be called on a locked OS thread.
func OpenCurrentThreadForCancellation() (HANDLE, error) {
	id, _, _ := procGetCurrentThreadId.Call()
	h, _, err := procOpenThread.Call(0x0001, 0, id) // THREAD_TERMINATE
	if h == 0 {
		return 0, err
	}
	return HANDLE(h), nil
}

func CancelSynchronousIo(thread HANDLE) bool {
	r, _, err := procCancelSynchronousIo.Call(uintptr(thread))
	return r != 0 || errors.Is(err, syscall.ERROR_NOT_FOUND)
}
