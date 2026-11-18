package win32

import (
	"runtime"
	"unsafe"
)

const (
	FILE_NOTIFY_CHANGE_FILE_NAME  DWORD = 0x00000001
	FILE_NOTIFY_CHANGE_DIR_NAME   DWORD = 0x00000002
	FILE_NOTIFY_CHANGE_ATTRIBUTES DWORD = 0x00000004
	FILE_NOTIFY_CHANGE_SIZE       DWORD = 0x00000008
	FILE_NOTIFY_CHANGE_LAST_WRITE DWORD = 0x00000010
	FILE_NOTIFY_CHANGE_SECURITY   DWORD = 0x00000100

	FILE_ACTION_ADDED            DWORD = 1
	FILE_ACTION_REMOVED          DWORD = 2
	FILE_ACTION_MODIFIED         DWORD = 3
	FILE_ACTION_RENAMED_OLD_NAME DWORD = 4
	FILE_ACTION_RENAMED_NEW_NAME DWORD = 5
)

func CreateIoCompletionPort(file HANDLE, existingPort HANDLE, completionKey uintptr, numConcurrentThreads DWORD) (HANDLE, error) {
	r, _, err := procCreateIoCompletionPort.Call(file, existingPort, completionKey, uintptr(numConcurrentThreads))
	if r == 0 {
		return 0, err
	}
	return r, nil
}

func GetQueuedCompletionStatus(port HANDLE, bytesTransferred *DWORD, completionKey *uintptr, overlapped **OVERLAPPED, ms DWORD) bool {
	r, _, _ := procGetQueuedCompletionStatus.Call(
		port,
		uintptr(unsafe.Pointer(bytesTransferred)),
		uintptr(unsafe.Pointer(completionKey)),
		uintptr(unsafe.Pointer(overlapped)),
		uintptr(ms),
	)
	return r != 0
}

func PostQueuedCompletionStatus(port HANDLE, bytesTransferred DWORD, completionKey uintptr, overlapped *OVERLAPPED) bool {
	r, _, _ := procPostQueuedCompletionStatus.Call(
		port,
		uintptr(bytesTransferred),
		completionKey,
		uintptr(unsafe.Pointer(overlapped)),
	)
	return r != 0
}

func ReadDirectoryChanges(h HANDLE, buf []byte, watchSubtree bool, notifyFilter DWORD, bytesReturned *DWORD, overlapped *OVERLAPPED) error {
	var watch uintptr
	if watchSubtree {
		watch = 1
	}
	var bufPtr uintptr
	if len(buf) > 0 {
		bufPtr = uintptr(unsafe.Pointer(&buf[0]))
	}
	r, _, err := procReadDirectoryChangesW.Call(
		h,
		bufPtr,
		uintptr(len(buf)),
		watch,
		uintptr(notifyFilter),
		uintptr(unsafe.Pointer(bytesReturned)),
		uintptr(unsafe.Pointer(overlapped)),
		0,
	)
	runtime.KeepAlive(buf)
	if r == 0 {
		return err
	}
	return nil
}

func CancelIoEx(h HANDLE, overlapped *OVERLAPPED) bool {
	r, _, _ := procCancelIoEx.Call(h, uintptr(unsafe.Pointer(overlapped)))
	return r != 0
}
