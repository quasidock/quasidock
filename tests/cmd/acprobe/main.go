package main

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"github.com/quasidock/quasidock/internal/win32"
)

func printSelf() {
	token, err := win32.OpenProcessToken(win32.GetCurrentProcess(), win32.TOKEN_QUERY)
	if err != nil {
		fmt.Println("# token unreadable")
		return
	}
	defer win32.CloseHandle(token)

	var isAC win32.DWORD
	var retLen win32.DWORD
	_ = win32.GetTokenInformation(token, win32.TokenIsAppContainer, unsafe.Pointer(&isAC), uint32(unsafe.Sizeof(isAC)), &retLen)

	var label [256]byte
	if err := win32.GetTokenInformation(token, win32.TokenIntegrityLevel, unsafe.Pointer(&label[0]), uint32(len(label)), &retLen); err != nil {
		fmt.Println("# integrity unreadable")
		return
	}
	mandatoryLabel := (*win32.TOKEN_MANDATORY_LABEL)(unsafe.Pointer(&label[0]))
	sid := mandatoryLabel.Label.Sid
	subCount := win32.GetSidSubAuthorityCount(sid)
	if subCount == nil || *subCount == 0 {
		return
	}
	ridPtr := win32.GetSidSubAuthority(sid, uint32(*subCount)-1)
	if ridPtr == nil {
		return
	}
	rid := *ridPtr

	level := "untrusted"
	if rid >= 0x4000 {
		level = "system"
	} else if rid >= 0x3000 {
		level = "high"
	} else if rid >= 0x2000 {
		level = "medium"
	} else if rid >= 0x1000 {
		level = "low"
	}

	acStr := ""
	if isAC != 0 {
		acStr = ", in an AppContainer"
	}
	fmt.Printf("# integrity %s%s\n", level, acStr)
}

func report(name string, err error) {
	if err != nil {
		fmt.Printf("%s error (%v)\n", name, err)
	} else {
		fmt.Printf("%s ok\n", name)
	}
}

func finalPath(path string, volume win32.DWORD) error {
	h, err := win32.CreateFile(
		path,
		win32.FILE_READ_ATTRIBUTES,
		win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE|win32.FILE_SHARE_DELETE,
		nil,
		win32.OPEN_EXISTING,
		win32.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	defer win32.CloseHandle(h)

	_, err = win32.GetFinalPathNameByHandle(h, volume)
	return err
}

func longName(path string) error {
	_, err := win32.GetLongPathName(path)
	return err
}

func openDevice(path string, access, flags win32.DWORD) error {
	h, err := win32.CreateFile(
		path,
		access,
		win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE|win32.FILE_SHARE_DELETE,
		nil,
		win32.OPEN_EXISTING,
		flags,
		0,
	)
	if err != nil {
		return err
	}
	win32.CloseHandle(h)
	return nil
}

func runLow() {
	rawLine := win32.UTF16PtrToString(win32.GetCommandLineW())
	idx := strings.Index(rawLine, "--low")
	if idx == -1 {
		win32.ExitProcess(1)
	}
	cmd := strings.TrimSpace(rawLine[idx+len("--low"):])

	token, err := win32.OpenProcessToken(win32.GetCurrentProcess(), win32.TOKEN_ASSIGN_PRIMARY|win32.TOKEN_DUPLICATE|win32.TOKEN_QUERY|win32.TOKEN_ADJUST_DEFAULT)
	if err != nil {
		win32.ExitProcess(1)
	}
	defer win32.CloseHandle(token)

	low, err := win32.DuplicateTokenEx(token, 0, nil, win32.SecurityImpersonation, win32.TokenPrimary)
	if err != nil {
		win32.ExitProcess(1)
	}
	defer win32.CloseHandle(low)

	sid, err := win32.ConvertStringSidToSid("S-1-16-4096")
	if err != nil {
		win32.ExitProcess(1)
	}
	defer win32.LocalFree(uintptr(sid))

	label := win32.TOKEN_MANDATORY_LABEL{
		Label: win32.SID_AND_ATTRIBUTES{
			Sid:        sid,
			Attributes: win32.SE_GROUP_INTEGRITY,
		},
	}
	_ = win32.SetTokenInformation(low, win32.TokenIntegrityLevel, unsafe.Pointer(&label), uint32(unsafe.Sizeof(label))+uint32(win32.GetLengthSid(sid)))

	var si win32.STARTUPINFOW
	si.Cb = win32.DWORD(unsafe.Sizeof(si))
	si.DwFlags = win32.STARTF_USESTDHANDLES
	si.HStdInput = win32.GetStdHandle(win32.STD_INPUT_HANDLE)
	si.HStdOutput = win32.GetStdHandle(win32.STD_OUTPUT_HANDLE)
	si.HStdError = win32.GetStdHandle(win32.STD_ERROR_HANDLE)

	var pi win32.PROCESS_INFORMATION
	err = win32.CreateProcessAsUser(
		low,
		"",
		cmd,
		nil,
		nil,
		true,
		0,
		nil,
		"",
		unsafe.Pointer(&si),
		&pi,
	)
	if err != nil {
		win32.ExitProcess(1)
	}
	defer win32.CloseHandle(pi.HThread)
	defer win32.CloseHandle(pi.HProcess)

	win32.WaitForSingleObject(pi.HProcess, win32.INFINITE)
	code, _ := win32.GetExitCodeProcess(pi.HProcess)
	win32.ExitProcess(uint32(code))
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--low" {
		runLow()
		return
	}

	printSelf()

	cwd, _ := os.Getwd()
	exe, _ := win32.GetModuleFileName()

	report("final-path-dos", finalPath(cwd, win32.VOLUME_NAME_DOS))
	report("self-path-dos", finalPath(exe, win32.VOLUME_NAME_DOS))
	report("self-long-name", longName(exe))
	report("final-path-nt", finalPath(cwd, win32.VOLUME_NAME_NT))
	report("mount-manager", openDevice(`\\.\MountPointManager`, 0, 0))

	if len(cwd) >= 2 && cwd[1] == ':' {
		root := string(cwd[:2]) + "\\"
		report("drive-root", openDevice(root, win32.FILE_READ_ATTRIBUTES, win32.FILE_FLAG_BACKUP_SEMANTICS))
	} else {
		report("drive-root", nil)
	}

	report("nul", openDevice("NUL", win32.GENERIC_READ|win32.GENERIC_WRITE, 0))

	station, err := win32.CreateWindowStation("", 0, win32.GENERIC_ALL)
	report("window-station", err)
	if err == nil {
		win32.CloseWindowStation(station)
	}

	desk, err := win32.CreateDesktop("Default", 0, win32.GENERIC_ALL)
	report("desktop", err)
	if err == nil {
		win32.CloseDesktop(desk)
	}

	win32.ExitProcess(0)
}
