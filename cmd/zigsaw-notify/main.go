package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/zigsaw-io/zigsaw/internal/process"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

const (
	wmTray              = 0x8000 + 1
	ninBalloonUserClick = 0x0400 + 5
	qunsAccepts         = 5
	checkMs             = 5 * 60 * 1000
	listenMs            = 2 * 60 * 1000
)

var clicked = false

func wndProc(hwnd uintptr, msg uintptr, wp uintptr, lp uintptr) uintptr {
	switch uint32(msg) {
	case wmTray:
		if (lp & 0xffff) == ninBalloonUserClick {
			clicked = true
			win32.PostQuitMessage(0)
		}
		return 0
	case uint32(win32.WM_TIMER):
		win32.PostQuitMessage(0)
		return 0
	default:
		return win32.DefWindowProcW(win32.HANDLE(hwnd), win32.DWORD(msg), wp, lp)
	}
}

func copyInto(dest []uint16, text string) {
	u16 := win32.UTF8ToUTF16(text)
	n := len(u16)
	if n > len(dest)-1 {
		n = len(dest) - 1
	}
	copy(dest[:n], u16[:n])
	dest[n] = 0
}

func show(title, text string) (bool, error) {
	instance := win32.GetModuleHandle(nil)
	className := "ZigsawNotify"
	clsNamePtr := win32.UTF16Ptr(className)
	cb := syscall.NewCallback(wndProc)

	cls := win32.WNDCLASSW{
		LpfnWndProc:   cb,
		HInstance:     instance,
		LpszClassName: clsNamePtr,
	}
	if win32.RegisterClassW(&cls) == 0 {
		return false, fmt.Errorf("RegisterClassW failed")
	}

	hwnd := win32.CreateWindowExW(0, clsNamePtr, win32.UTF16Ptr("zigsaw"), 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if hwnd == 0 {
		return false, fmt.Errorf("CreateWindowExW failed")
	}

	var data win32.NOTIFYICONDATAW
	data.CbSize = win32.DWORD(unsafe.Sizeof(data))
	data.HWnd = hwnd
	data.UID = 1
	data.UFlags = win32.NIF_MESSAGE | win32.NIF_ICON | win32.NIF_TIP | win32.NIF_INFO
	data.UCallbackMessage = wmTray
	data.HIcon = win32.LoadIconW(0, 32516) // IDI_INFORMATION
	copyInto(data.SzTip[:], title)
	copyInto(data.SzInfoTitle[:], title)
	copyInto(data.SzInfo[:], text)
	data.DwInfoFlags = win32.NIIF_INFO

	if !win32.Shell_NotifyIconW(win32.NIM_ADD, &data) {
		return false, fmt.Errorf("Shell_NotifyIconW add failed")
	}
	defer win32.Shell_NotifyIconW(win32.NIM_DELETE, &data)

	data.UVersion = win32.NOTIFYICON_VERSION_4
	win32.Shell_NotifyIconW(win32.NIM_SETVERSION, &data)

	win32.SetTimer(hwnd, 1, listenMs, 0)
	var msg win32.MSG
	for win32.GetMessageW(&msg, 0, 0, 0) > 0 {
		win32.DispatchMessageW(&msg)
	}
	return clicked, nil
}

func openList(zigsaw string) error {
	cmd, err := win32.SystemProgram("cmd.exe")
	if err != nil {
		cmd = "cmd.exe"
	}
	line := fmt.Sprintf("\"%s\" /k \"\"%s\" updates\"", cmd, zigsaw)
	var startup win32.STARTUPINFOW
	startup.Cb = win32.DWORD(unsafe.Sizeof(startup))
	var info win32.PROCESS_INFORMATION
	err = win32.CreateProcess(
		cmd,
		line,
		nil,
		nil,
		false,
		win32.CREATE_NEW_CONSOLE,
		nil,
		"",
		unsafe.Pointer(&startup),
		&info,
	)
	if err != nil {
		return err
	}
	win32.CloseHandle(info.HProcess)
	win32.CloseHandle(info.HThread)
	return nil
}

func removeTask(task string) error {
	exe, err := win32.SystemProgram("schtasks.exe")
	if err != nil {
		exe = "schtasks.exe"
	}
	nul, err := win32.CreateFile("NUL", win32.GENERIC_READ|win32.GENERIC_WRITE, win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE, nil, win32.OPEN_EXISTING, 0, 0)
	if err != nil {
		nul = 0
	}
	defer win32.CloseHandle(nul)
	cmdLine := process.BuildCommandLine(exe, []string{"/delete", "/tn", task, "/f"})
	_, err = process.Output(process.SpawnSpec{
		Exe:           exe,
		CommandLine:   cmdLine,
		CreationFlags: win32.CREATE_NO_WINDOW,
		Stdio:         &[3]win32.HANDLE{0, 0, nul},
	}, nil, 60*1000)
	return err
}

func notify() error {
	args := os.Args[1:]
	var storePath, taskName string
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "--store" {
			storePath = args[i+1]
		} else if args[i] == "--task" {
			taskName = args[i+1]
		}
	}
	if storePath == "" {
		return fmt.Errorf("missing --store")
	}

	if _, err := os.Stat(storePath); os.IsNotExist(err) {
		if taskName != "" {
			_ = removeTask(taskName)
		}
		return nil
	}

	_ = win32.SetEnvironmentVariable("ZIGSAW_HOME", storePath)

	self, err := win32.GetModuleFileName()
	if err != nil {
		return err
	}
	zigsaw := filepath.Join(filepath.Dir(self), "zigsaw.exe")
	reportPath := os.Getenv("ZIGSAW_NOTIFY_REPORT")

	state, err := win32.SHQueryUserNotificationState()
	accepting := reportPath != "" || (err == nil && state == qunsAccepts)

	nul, err := win32.CreateFile("NUL", win32.GENERIC_READ|win32.GENERIC_WRITE, win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE, nil, win32.OPEN_EXISTING, 0, 0)
	if err != nil {
		nul = 0
	}
	defer win32.CloseHandle(nul)

	var checkArgs []string
	if accepting {
		checkArgs = []string{"updates", "check", "--notify"}
	} else {
		checkArgs = []string{"updates", "check", "--quiet"}
	}

	cmdLine := process.BuildCommandLine(zigsaw, checkArgs)
	res, err := process.Output(process.SpawnSpec{
		Exe:           zigsaw,
		CommandLine:   cmdLine,
		CreationFlags: win32.CREATE_NO_WINDOW,
		Stdio:         &[3]win32.HANDLE{0, 0, nul},
	}, nil, checkMs)
	if err != nil || res == nil {
		return nil
	}

	lines := strings.Split(strings.ReplaceAll(string(res.Stdout), "\r\n", "\n"), "\n")
	if len(lines) < 2 || lines[0] == "" || lines[1] == "" {
		return nil
	}
	title := lines[0]
	text := lines[1]

	if reportPath != "" {
		return os.WriteFile(reportPath, []byte(fmt.Sprintf("%s\n%s\n", title, text)), 0644)
	}

	wasClicked, err := show(title, text)
	if err != nil {
		return err
	}
	if wasClicked {
		_ = openList(zigsaw)
	}
	return nil
}

func main() {
	if err := notify(); err != nil {
		win32.ExitProcess(1)
	}
	win32.ExitProcess(0)
}
