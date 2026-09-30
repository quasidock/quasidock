package win32

import (
	"syscall"
	"unsafe"
)

const (
	MAXIMUM_REPARSE_DATA_BUFFER_SIZE = 16 * 1024
)

func SHGetKnownFolderPath(rfid *GUID, flags DWORD, token HANDLE) (string, error) {
	var pathPtr *uint16
	r, _, _ := procSHGetKnownFolderPath.Call(
		uintptr(unsafe.Pointer(rfid)),
		uintptr(flags),
		token,
		uintptr(unsafe.Pointer(&pathPtr)),
	)
	if r != 0 {
		return "", syscall.Errno(r)
	}
	defer CoTaskMemFree(uintptr(unsafe.Pointer(pathPtr)))
	return UTF16PtrToString(pathPtr), nil
}

func CoTaskMemFree(p uintptr) {
	procCoTaskMemFree.Call(p)
}

func SHChangeNotify(eventId int32, flags uint32, item1, item2 uintptr) {
	procSHChangeNotify.Call(uintptr(eventId), uintptr(flags), item1, item2)
}
