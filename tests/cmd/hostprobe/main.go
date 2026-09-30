package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/quasidock/quasidock/internal/win32"
)

var folders = []struct {
	Name string
	Guid win32.GUID
}{
	{"RoamingAppData", win32.GUID{Data1: 0x3EB685DB, Data2: 0x65F9, Data3: 0x4CF6, Data4: [8]byte{0xA0, 0x3A, 0xE3, 0xEF, 0x65, 0x72, 0x9F, 0x3D}}},
	{"LocalAppData", win32.GUID{Data1: 0xF1B32785, Data2: 0x6FBA, Data3: 0x4FCF, Data4: [8]byte{0x9D, 0x55, 0x7B, 0x8E, 0x7F, 0x15, 0x70, 0x91}}},
	{"LocalAppDataLow", win32.GUID{Data1: 0xA520A1A4, Data2: 0x1780, Data3: 0x4FF6, Data4: [8]byte{0xBD, 0x18, 0x16, 0x73, 0x43, 0xC5, 0xAF, 0x16}}},
	{"Profile", win32.GUID{Data1: 0x5E6C858F, Data2: 0x0E22, Data3: 0x4760, Data4: [8]byte{0x9A, 0xFE, 0xEA, 0x33, 0x17, 0xB6, 0x71, 0x73}}},
	{"Desktop", win32.GUID{Data1: 0xB4BFCC3A, Data2: 0xDB2C, Data3: 0x424C, Data4: [8]byte{0xB0, 0x29, 0x7F, 0xE9, 0x9A, 0x87, 0xC6, 0x41}}},
	{"Documents", win32.GUID{Data1: 0xFDD39AD0, Data2: 0x238F, Data3: 0x46AF, Data4: [8]byte{0xAD, 0xB4, 0x6C, 0x85, 0x48, 0x03, 0x69, 0xC7}}},
	{"Downloads", win32.GUID{Data1: 0x374DE290, Data2: 0x123F, Data3: 0x4565, Data4: [8]byte{0x91, 0x64, 0x39, 0xC4, 0x92, 0x5E, 0x46, 0x7B}}},
	{"Music", win32.GUID{Data1: 0x4BD8D571, Data2: 0x6D19, Data3: 0x48D3, Data4: [8]byte{0xBE, 0x97, 0x42, 0x22, 0x20, 0x08, 0x0E, 0x43}}},
	{"Pictures", win32.GUID{Data1: 0x33E28130, Data2: 0x4E1E, Data3: 0x4676, Data4: [8]byte{0x83, 0x5A, 0x98, 0x39, 0x5C, 0x3B, 0xC3, 0xBB}}},
	{"Videos", win32.GUID{Data1: 0x18989B1D, Data2: 0x99B5, Data3: 0x455B, Data4: [8]byte{0x84, 0x1C, 0xAB, 0x7C, 0x74, 0xE4, 0xDD, 0xFC}}},
	{"ProgramData", win32.GUID{Data1: 0x62AB5D82, Data2: 0xFDC1, Data3: 0x4DC3, Data4: [8]byte{0xA9, 0xDD, 0x07, 0x0D, 0x1D, 0x49, 0x5D, 0x97}}},
}

var variables = []string{
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP",
	"ProgramData", "ProgramFiles", "ProgramFiles(x86)", "CommonProgramFiles",
	"PUBLIC", "ALLUSERSPROFILE", "COMPUTERNAME", "USERDOMAIN",
	"USERNAME",
}

var (
	clsidFileOpenDialog = win32.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidIFileOpenDialog  = win32.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x11, 0xA9, 0x44, 0xB6}}
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

func reportFolder(name, dir string) {
	stat, err := os.Stat(dir)
	if err != nil || !stat.IsDir() {
		fmt.Printf("folder %s = %s (missing)\n", name, dir)
		return
	}
	pid := os.Getpid()
	tmpFile := filepath.Join(dir, fmt.Sprintf("quasidock-hostprobe-%d.tmp", pid))
	h, err := win32.CreateFile(
		tmpFile,
		win32.GENERIC_WRITE|win32.DELETE,
		0,
		nil,
		win32.CREATE_NEW,
		win32.FILE_FLAG_DELETE_ON_CLOSE,
		0,
	)
	if err != nil {
		fmt.Printf("folder %s = %s (write error 5 (AccessDenied))\n", name, dir)
		return
	}
	win32.CloseHandle(h)
	fmt.Printf("folder %s = %s (write ok)\n", name, dir)
}

func createAndDelete(subKey string) error {
	key, status := win32.RegCreateKeyEx(win32.HKEY_CURRENT_USER, subKey, win32.KEY_WRITE)
	if status != win32.ERROR_SUCCESS {
		return fmt.Errorf("error %d", status)
	}
	win32.RegCloseKey(key)
	deleted := win32.RegDeleteKey(win32.HKEY_CURRENT_USER, subKey)
	if deleted != win32.ERROR_SUCCESS {
		return fmt.Errorf("error %d", deleted)
	}
	return nil
}

func main() {
	printSelf()

	for _, v := range variables {
		val, ok := os.LookupEnv(v)
		if ok {
			fmt.Printf("env %s = %s\n", v, val)
		} else {
			fmt.Printf("env %s = (unset)\n", v)
		}
	}

	for _, f := range folders {
		path, err := win32.SHGetKnownFolderPath(&f.Guid, 0x4000 /* KF_FLAG_DONT_VERIFY */, 0)
		if err != nil {
			fmt.Printf("folder %s error 0x80070002\n", f.Name)
		} else {
			reportFolder(f.Name, path)
		}
	}

	tmpDir := os.TempDir()
	reportFolder("GetTempPath", strings.TrimRight(tmpDir, "\\/"))

	pid := os.Getpid()
	if err := createAndDelete(fmt.Sprintf("Software\\QuasidockHostProbe-%d", pid)); err == nil {
		fmt.Println("registry write HKCU\\Software ok")
	} else {
		fmt.Printf("registry write HKCU\\Software %v\n", err)
	}

	if err := createAndDelete(fmt.Sprintf("Software\\AppDataLow\\QuasidockHostProbe-%d", pid)); err == nil {
		fmt.Println("registry write HKCU\\Software\\AppDataLow ok")
	} else {
		fmt.Printf("registry write HKCU\\Software\\AppDataLow %v\n", err)
	}

	// Read HKLM CurrentBuild
	hklmKey, status := win32.RegOpenKeyEx(win32.HKEY_LOCAL_MACHINE, "SOFTWARE\\Microsoft\\Windows NT\\CurrentVersion", win32.KEY_READ)
	if status == win32.ERROR_SUCCESS {
		fmt.Println("registry read HKLM ok")
		win32.RegCloseKey(hklmKey)
	} else {
		fmt.Println("registry read HKLM error")
	}

	// COM file dialog
	hr := win32.CoInitializeEx(0, win32.COINIT_APARTMENTTHREADED)
	if hr == win32.S_OK || hr == win32.S_FALSE {
		var dialog uintptr
		hr = win32.CoCreateInstance(&clsidFileOpenDialog, 0, win32.CLSCTX_INPROC_SERVER, &iidIFileOpenDialog, &dialog)
	}
	if hr == win32.S_OK {
		fmt.Println("com file-dialog ok")
	} else {
		fmt.Printf("com file-dialog error 0x%08X\n", uint32(hr))
	}

	if win32.OpenClipboard(0) {
		win32.CloseClipboard()
		fmt.Println("clipboard open ok")
	} else {
		fmt.Println("clipboard open error")
	}
}
