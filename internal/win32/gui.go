package win32

import (
	"fmt"
	"slices"
	"syscall"
	"unsafe"
)

var (
	procGetStartupInfoW           = modKernel32.NewProc("GetStartupInfoW")
	procGetModuleHandleW          = modKernel32.NewProc("GetModuleHandleW")
	procCommandLineToArgvW        = modShell32.NewProc("CommandLineToArgvW")
	procRegisterClassW            = modUser32.NewProc("RegisterClassW")
	procCreateWindowExW           = modUser32.NewProc("CreateWindowExW")
	procDefWindowProcW            = modUser32.NewProc("DefWindowProcW")
	procGetMessageW               = modUser32.NewProc("GetMessageW")
	procDispatchMessageW          = modUser32.NewProc("DispatchMessageW")
	procPostQuitMessage           = modUser32.NewProc("PostQuitMessage")
	procSetTimer                  = modUser32.NewProc("SetTimer")
	procLoadIconW                 = modUser32.NewProc("LoadIconW")
	procCreatePseudoConsole       = modKernel32.NewProc("CreatePseudoConsole")
	procClosePseudoConsole        = modKernel32.NewProc("ClosePseudoConsole")
	procAttachConsole             = modKernel32.NewProc("AttachConsole")
	procFreeConsole               = modKernel32.NewProc("FreeConsole")
	procGetConsoleProcessList     = modKernel32.NewProc("GetConsoleProcessList")
	procGenerateConsoleCtrlEvent  = modKernel32.NewProc("GenerateConsoleCtrlEvent")
	procSetCurrentDirectoryW      = modKernel32.NewProc("SetCurrentDirectoryW")
	procOpenClipboard             = modUser32.NewProc("OpenClipboard")
	procCloseClipboard            = modUser32.NewProc("CloseClipboard")
	procEnumWindows               = modUser32.NewProc("EnumWindows")
	procIsWindowVisible           = modUser32.NewProc("IsWindowVisible")
	procGetWindowTextW            = modUser32.NewProc("GetWindowTextW")
	procGetClassNameW             = modUser32.NewProc("GetClassNameW")
	procGetWindowThreadProcessId  = modUser32.NewProc("GetWindowThreadProcessId")
	procPostMessageW              = modUser32.NewProc("PostMessageW")
	procGetFinalPathNameByHandleW = modKernel32.NewProc("GetFinalPathNameByHandleW")
	procGetLongPathNameW          = modKernel32.NewProc("GetLongPathNameW")
	procCreateWindowStationW      = modUser32.NewProc("CreateWindowStationW")
	procCloseWindowStation        = modUser32.NewProc("CloseWindowStation")
	procCreateDesktopW            = modUser32.NewProc("CreateDesktopW")
	procCloseDesktop              = modUser32.NewProc("CloseDesktop")
)

const (
	WM_CLOSE        DWORD = 0x0010
	VOLUME_NAME_DOS DWORD = 0x0
	VOLUME_NAME_NT  DWORD = 0x2
)

func GetFinalPathNameByHandle(h HANDLE, flags DWORD) (string, error) {
	var buf [32 * 1024]uint16
	r, _, err := procGetFinalPathNameByHandleW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(flags))
	if r == 0 {
		return "", err
	}
	return UTF16ToUTF8(buf[:r]), nil
}

func GetLongPathName(path string) (string, error) {
	var buf [32 * 1024]uint16
	r, _, err := procGetLongPathNameW.Call(uintptr(unsafe.Pointer(UTF16Ptr(path))), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return "", err
	}
	return UTF16ToUTF8(buf[:r]), nil
}

func CreateWindowStation(name string, flags, access DWORD) (HANDLE, error) {
	var namePtr *uint16
	if name != "" {
		namePtr = UTF16Ptr(name)
	}
	r, _, err := procCreateWindowStationW.Call(uintptr(unsafe.Pointer(namePtr)), uintptr(flags), uintptr(access), 0)
	if r == 0 {
		return 0, err
	}
	return HANDLE(r), nil
}

func CloseWindowStation(h HANDLE) bool {
	r, _, _ := procCloseWindowStation.Call(h)
	return r != 0
}

func CreateDesktop(name string, flags, access DWORD) (HANDLE, error) {
	r, _, err := procCreateDesktopW.Call(uintptr(unsafe.Pointer(UTF16Ptr(name))), 0, 0, uintptr(flags), uintptr(access), 0)
	if r == 0 {
		return 0, err
	}
	return HANDLE(r), nil
}

func CloseDesktop(h HANDLE) bool {
	r, _, _ := procCloseDesktop.Call(h)
	return r != 0
}

func EnumWindows(callback uintptr, lparam uintptr) bool {
	r, _, _ := procEnumWindows.Call(callback, lparam)
	return r != 0
}

func IsWindowVisible(hwnd HANDLE) bool {
	r, _, _ := procIsWindowVisible.Call(hwnd)
	return r != 0
}

func GetWindowText(hwnd HANDLE) string {
	var buf [512]uint16
	r, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return ""
	}
	return UTF16ToUTF8(buf[:r])
}

func GetClassName(hwnd HANDLE) string {
	var buf [512]uint16
	r, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return ""
	}
	return UTF16ToUTF8(buf[:r])
}

func GetWindowThreadProcessId(hwnd HANDLE, pid *DWORD) DWORD {
	r, _, _ := procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(pid)))
	return DWORD(r)
}

func PostMessage(hwnd HANDLE, msg DWORD, wParam, lParam uintptr) bool {
	r, _, _ := procPostMessageW.Call(hwnd, uintptr(msg), wParam, lParam)
	return r != 0
}

func OpenClipboard(hWnd HANDLE) bool {
	r, _, _ := procOpenClipboard.Call(hWnd)
	return r != 0
}

func CloseClipboard() bool {
	r, _, _ := procCloseClipboard.Call()
	return r != 0
}

type COORD struct {
	X int16
	Y int16
}

const (
	PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE = 0x00020016
)

func CreatePseudoConsole(size COORD, input, output HANDLE, flags DWORD, pc *uintptr) int32 {
	coord := uintptr(uint32(uint16(size.X)) | (uint32(uint16(size.Y)) << 16))
	r, _, _ := procCreatePseudoConsole.Call(
		coord,
		input,
		output,
		uintptr(flags),
		uintptr(unsafe.Pointer(pc)),
	)
	return int32(r)
}

func ClosePseudoConsole(pc uintptr) {
	_, _, _ = procClosePseudoConsole.Call(pc)
}

func AttachConsole(pid DWORD) bool {
	r, _, _ := procAttachConsole.Call(uintptr(pid))
	return r != 0
}

func FreeConsole() bool {
	r, _, _ := procFreeConsole.Call()
	return r != 0
}

func GenerateConsoleCtrlEvent(event, group DWORD) bool {
	r, _, _ := procGenerateConsoleCtrlEvent.Call(uintptr(event), uintptr(group))
	return r != 0
}

// SendConsoleBreak reports whether Windows accepted the targeted signal.
func SendConsoleBreak(group DWORD) error {
	// GenerateConsoleCtrlEvent can succeed even when its target has detached.
	// Verify the group leader shares our console before requesting delivery.
	attached := make([]DWORD, 16)
	for {
		count, _, err := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&attached[0])), uintptr(len(attached)))
		if count == 0 {
			return fmt.Errorf("listing console processes: %w", err)
		}
		if count > uintptr(len(attached)) {
			attached = make([]DWORD, count)
			continue
		}
		if !slices.Contains(attached[:count], group) {
			return fmt.Errorf("process %d is not attached to the supervisor's console", group)
		}
		break
	}
	r, _, err := procGenerateConsoleCtrlEvent.Call(uintptr(CTRL_BREAK_EVENT), uintptr(group))
	if r == 0 {
		return err
	}
	return nil
}

func SetCurrentDirectory(path string) error {
	r, _, err := procSetCurrentDirectoryW.Call(uintptr(unsafe.Pointer(UTF16Ptr(path))))
	if r == 0 {
		return err
	}
	return nil
}

const (
	FILE_TYPE_UNKNOWN DWORD = 0x0000
	FILE_TYPE_DISK    DWORD = 0x0001
	FILE_TYPE_CHAR    DWORD = 0x0002
	FILE_TYPE_PIPE    DWORD = 0x0003
	FILE_TYPE_REMOTE  DWORD = 0x8000

	MB_OK        = 0x00000000
	MB_ICONERROR = 0x00000010

	NIM_ADD        DWORD = 0x00000000
	NIM_MODIFY     DWORD = 0x00000001
	NIM_DELETE     DWORD = 0x00000002
	NIM_SETVERSION DWORD = 0x00000004

	NIF_MESSAGE DWORD = 0x00000001
	NIF_ICON    DWORD = 0x00000002
	NIF_TIP     DWORD = 0x00000004
	NIF_STATE   DWORD = 0x00000008
	NIF_INFO    DWORD = 0x00000010
	NIF_GUID    DWORD = 0x00000020

	NIIF_INFO DWORD = 0x00000001

	NOTIFYICON_VERSION_4 DWORD = 4

	WM_TIMER DWORD = 0x0113
)

type NOTIFYICONDATAW struct {
	CbSize           DWORD
	HWnd             HANDLE
	UID              DWORD
	UFlags           DWORD
	UCallbackMessage DWORD
	HIcon            HANDLE
	SzTip            [128]uint16
	DwState          DWORD
	DwStateMask      DWORD
	SzInfo           [256]uint16
	UVersion         DWORD
	SzInfoTitle      [64]uint16
	DwInfoFlags      DWORD
	GuidItem         GUID
	HBalloonIcon     HANDLE
}

type WNDCLASSW struct {
	Style         DWORD
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     HANDLE
	HIcon         HANDLE
	HCursor       HANDLE
	HbrBackground HANDLE
	LpszMenuName  *uint16
	LpszClassName *uint16
}

type MSG struct {
	HWnd     HANDLE
	Message  DWORD
	WParam   uintptr
	LParam   uintptr
	Time     DWORD
	Pt       [2]int32
	LPrivate DWORD
}

func GetCommandLineW() *uint16 {
	return syscall.GetCommandLine()
}

func GetStartupInfoW(startup *STARTUPINFOW) {
	_, _, _ = procGetStartupInfoW.Call(uintptr(unsafe.Pointer(startup)))
}

func GetModuleHandle(name *uint16) HANDLE {
	r, _, _ := procGetModuleHandleW.Call(uintptr(unsafe.Pointer(name)))
	return HANDLE(r)
}

func MessageBox(hwnd HANDLE, text, caption string, uType uint32) int {
	textPtr := UTF16Ptr(text)
	capPtr := UTF16Ptr(caption)
	r, _, _ := procMessageBoxW.Call(
		hwnd,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(capPtr)),
		uintptr(uType),
	)
	return int(r)
}

func CommandLineToArgvW(cmdLine string) ([]string, error) {
	var numArgs int32
	r, _, err := procCommandLineToArgvW.Call(
		uintptr(unsafe.Pointer(UTF16Ptr(cmdLine))),
		uintptr(unsafe.Pointer(&numArgs)),
	)
	if r == 0 {
		return nil, err
	}
	defer LocalFree(r)
	var args []string
	var pPtr **uint16
	_, _, _ = procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&pPtr)), uintptr(unsafe.Pointer(&r)), unsafe.Sizeof(pPtr))
	pArray := unsafe.Slice(pPtr, numArgs)
	for _, p := range pArray {
		args = append(args, UTF16PtrToString(p))
	}
	return args, nil
}

func SHQueryUserNotificationState() (int32, error) {
	var state int32
	r, _, _ := procSHQueryUserNotificationState.Call(uintptr(unsafe.Pointer(&state)))
	if r != 0 {
		return 0, fmt.Errorf("SHQueryUserNotificationState failed with 0x%x", r)
	}
	return state, nil
}

func Shell_NotifyIconW(msg DWORD, data *NOTIFYICONDATAW) bool {
	r, _, _ := procShell_NotifyIconW.Call(uintptr(msg), uintptr(unsafe.Pointer(data)))
	return r != 0
}

func RegisterClassW(cls *WNDCLASSW) uint16 {
	r, _, _ := procRegisterClassW.Call(uintptr(unsafe.Pointer(cls)))
	return uint16(r)
}

func CreateWindowExW(dwExStyle DWORD, lpClassName, lpWindowName *uint16, dwStyle DWORD, x, y, nWidth, nHeight int32, hWndParent, hMenu, hInstance HANDLE, lpParam uintptr) HANDLE {
	r, _, _ := procCreateWindowExW.Call(
		uintptr(dwExStyle),
		uintptr(unsafe.Pointer(lpClassName)),
		uintptr(unsafe.Pointer(lpWindowName)),
		uintptr(dwStyle),
		uintptr(x),
		uintptr(y),
		uintptr(nWidth),
		uintptr(nHeight),
		uintptr(hWndParent),
		uintptr(hMenu),
		uintptr(hInstance),
		lpParam,
	)
	return HANDLE(r)
}

func DefWindowProcW(hWnd HANDLE, msg DWORD, wParam, lParam uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(uintptr(hWnd), uintptr(msg), wParam, lParam)
	return r
}

func GetMessageW(msg *MSG, hWnd HANDLE, wMsgFilterMin, wMsgFilterMax DWORD) int32 {
	r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(msg)), uintptr(hWnd), uintptr(wMsgFilterMin), uintptr(wMsgFilterMax))
	return int32(r)
}

func DispatchMessageW(msg *MSG) uintptr {
	r, _, _ := procDispatchMessageW.Call(uintptr(unsafe.Pointer(msg)))
	return r
}

func PostQuitMessage(nExitCode int32) {
	_, _, _ = procPostQuitMessage.Call(uintptr(nExitCode))
}

func SetTimer(hWnd HANDLE, nIDEvent uintptr, uElapse DWORD, lpTimerFunc uintptr) uintptr {
	r, _, _ := procSetTimer.Call(uintptr(hWnd), nIDEvent, uintptr(uElapse), lpTimerFunc)
	return r
}

func LoadIconW(hInstance HANDLE, lpIconName uintptr) HANDLE {
	r, _, _ := procLoadIconW.Call(uintptr(hInstance), lpIconName)
	return HANDLE(r)
}
