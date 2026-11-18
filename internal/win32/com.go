package win32

import (
	"syscall"
	"unsafe"
)

const (
	S_OK                     HRESULT = 0
	S_FALSE                  HRESULT = 1
	COINIT_APARTMENTTHREADED DWORD   = 0x2
	CLSCTX_INPROC_SERVER     DWORD   = 0x1
	STGM_READ                DWORD   = 0x0
	SLGP_RAWPATH             DWORD   = 0x4
)

func CoInitializeEx(reserved uintptr, flags DWORD) HRESULT {
	r, _, _ := procCoInitializeEx.Call(reserved, uintptr(flags))
	return HRESULT(r)
}

func CoUninitialize() {
	procCoUninitialize.Call()
}

func CoCreateInstance(clsid *GUID, outer uintptr, context DWORD, iid *GUID, object *uintptr) HRESULT {
	r, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(clsid)),
		outer,
		uintptr(context),
		uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(object)),
	)
	return HRESULT(r)
}

func callComMethod(obj uintptr, index int, args ...uintptr) HRESULT {
	var vtable uintptr
	_, _, _ = procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&vtable)), obj, unsafe.Sizeof(vtable))
	var method uintptr
	_, _, _ = procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&method)), vtable+uintptr(index)*unsafe.Sizeof(method), unsafe.Sizeof(method))
	allArgs := make([]uintptr, 0, len(args)+1)
	allArgs = append(allArgs, obj)
	allArgs = append(allArgs, args...)
	r, _, _ := syscall.SyscallN(method, allArgs...)
	return HRESULT(r)
}

type IShellLinkW uintptr

func (s IShellLinkW) QueryInterface(iid *GUID, obj *uintptr) HRESULT {
	return callComMethod(uintptr(s), 0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(obj)))
}

func (s IShellLinkW) Release() uint32 {
	return uint32(callComMethod(uintptr(s), 2))
}

func (s IShellLinkW) GetPath(buf *uint16, bufLen int, findData uintptr, flags DWORD) HRESULT {
	return callComMethod(uintptr(s), 3, uintptr(unsafe.Pointer(buf)), uintptr(bufLen), findData, uintptr(flags))
}

func (s IShellLinkW) SetDescription(desc *uint16) HRESULT {
	return callComMethod(uintptr(s), 7, uintptr(unsafe.Pointer(desc)))
}

func (s IShellLinkW) SetWorkingDirectory(dir *uint16) HRESULT {
	return callComMethod(uintptr(s), 9, uintptr(unsafe.Pointer(dir)))
}

func (s IShellLinkW) SetIconLocation(path *uint16, iconIndex int32) HRESULT {
	return callComMethod(uintptr(s), 17, uintptr(unsafe.Pointer(path)), uintptr(iconIndex))
}

func (s IShellLinkW) SetPath(path *uint16) HRESULT {
	return callComMethod(uintptr(s), 20, uintptr(unsafe.Pointer(path)))
}

type IPersistFile uintptr

func (p IPersistFile) Release() uint32 {
	return uint32(callComMethod(uintptr(p), 2))
}

func (p IPersistFile) Load(filename *uint16, mode DWORD) HRESULT {
	return callComMethod(uintptr(p), 5, uintptr(unsafe.Pointer(filename)), uintptr(mode))
}

func (p IPersistFile) Save(filename *uint16, remember bool) HRESULT {
	rem := uintptr(0)
	if remember {
		rem = 1
	}
	return callComMethod(uintptr(p), 6, uintptr(unsafe.Pointer(filename)), rem)
}
