// Command mitprobe reports the process mitigation policies Windows applied to
// it and tries, one line per check, what each policy is meant to stop. It is
// started by quasidock with QUASIDOCK_TEST_MITIGATION / QUASIDOCK_TEST_CHILD_POLICY.
//
//	mitprobe        policies, checks, and a child's policies (inheritance)
//	mitprobe self   policies only, each line prefixed "child."
//
// Checks print "<check> ok", "<check> error <n> (...)", or a value. Files the
// image checks load come from the working directory (see tests/mitigation.sh):
// mitprobe-ms.dll (a copied System32 DLL), mitprobe-unsigned.dll (the same
// with its header timestamp changed, so it no longer matches a catalog),
// mitprobe-low.dll (the copy, labeled Low), and mitprobe-font.ttf. MITPROBE_REMOTE
// optionally names a DLL on a UNC path.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	pGetProcessMitigationPolicy = kernel32.NewProc("GetProcessMitigationPolicy")
	pGetCurrentProcess          = kernel32.NewProc("GetCurrentProcess")
	pVirtualAlloc               = kernel32.NewProc("VirtualAlloc")
	pVirtualProtect             = kernel32.NewProc("VirtualProtect")
	pVirtualFree                = kernel32.NewProc("VirtualFree")
	pLoadLibraryExW             = kernel32.NewProc("LoadLibraryExW")
	pFreeLibrary                = kernel32.NewProc("FreeLibrary")
	pGetModuleFileNameW         = kernel32.NewProc("GetModuleFileNameW")
	pCloseHandle                = kernel32.NewProc("CloseHandle")
	pCreateFileW                = kernel32.NewProc("CreateFileW")
	pDeviceIoControl            = kernel32.NewProc("DeviceIoControl")
	pWaitForSingleObject        = kernel32.NewProc("WaitForSingleObject")

	pGetDesktopWindow     = user32.NewProc("GetDesktopWindow")
	pAddFontResourceExW   = gdi32.NewProc("AddFontResourceExW")
	pRemoveFontResourceEx = gdi32.NewProc("RemoveFontResourceExW")
)

type policy struct {
	class int
	name  string
	size  uintptr
	bits  []string
}

// Bit names follow the PROCESS_MITIGATION_*_POLICY structs in winnt.h.
var policies = []policy{
	{0, "dep", 8, []string{"Enable", "DisableAtlThunkEmulation"}},
	{1, "aslr", 4, []string{"BottomUp", "ForceRelocate", "HighEntropy", "DisallowStripped"}},
	{2, "dyncode", 4, []string{"ProhibitDynamicCode", "AllowThreadOptOut", "AllowRemoteDowngrade", "Audit"}},
	{3, "handles", 4, []string{"RaiseOnInvalid", "Permanent"}},
	{4, "syscalls", 4, []string{"NoWin32k", "AuditWin32k", "NoFsctl", "AuditFsctl"}},
	{6, "extpoints", 4, []string{"Disable"}},
	{7, "cfg", 4, []string{"Enable", "ExportSuppression", "Strict", "Xfg", "XfgAudit"}},
	{8, "signature", 4, []string{"MicrosoftOnly", "StoreOnly", "OptIn", "AuditMicrosoft", "AuditStore"}},
	{9, "fonts", 4, []string{"NoNonSystem", "Audit"}},
	{10, "imageload", 4, []string{"NoRemote", "NoLowLabel", "PreferSystem32", "AuditNoRemote", "AuditNoLowLabel"}},
	{13, "children", 4, []string{"NoChildren", "Audit", "AllowSecure"}},
	{14, "sidechannel", 4, []string{"SmtBti", "IsolateDomain", "NoPageCombine", "SsbDisable", "RestrictCoreSharing"}},
	{15, "shadowstack", 4, []string{"Enable", "Audit", "SetContextIp", "AuditSetContextIp", "Strict", "BlockNonCet", "BlockNonCetNonEhcont", "AuditBlockNonCet", "DynamicApisOutOfProc", "SetContextIpRelaxed"}},
	{16, "redirtrust", 4, []string{"Enforce", "Audit"}},
}

func printPolicies(prefix string) {
	self, _, _ := pGetCurrentProcess.Call()
	for _, p := range policies {
		var buf [8]byte
		r, _, err := pGetProcessMitigationPolicy.Call(self, uintptr(p.class), uintptr(unsafe.Pointer(&buf[0])), p.size)
		if r == 0 {
			fmt.Printf("%spolicy %s error %d\n", prefix, p.name, errno(err))
			continue
		}
		flags := *(*uint32)(unsafe.Pointer(&buf[0]))
		var set []string
		for i, name := range p.bits {
			if flags&(1<<i) != 0 {
				set = append(set, name)
			}
		}
		if rest := flags &^ (1<<len(p.bits) - 1); rest != 0 {
			set = append(set, fmt.Sprintf("0x%x", rest))
		}
		if p.class == 0 && buf[4] != 0 {
			set = append(set, "Permanent")
		}
		if len(set) == 0 {
			set = []string{"-"}
		}
		fmt.Printf("%spolicy %s %s\n", prefix, p.name, strings.Join(set, ","))
	}
}

func errno(err error) uintptr {
	if e, ok := err.(syscall.Errno); ok {
		return uintptr(e)
	}
	return 0
}

func report(name string, r uintptr, err error) {
	if r != 0 {
		fmt.Printf("%s ok\n", name)
		return
	}
	fmt.Printf("%s error %d (%v)\n", name, errno(err), err)
}

const (
	memCommit            = 0x1000
	memReserve           = 0x2000
	memRelease           = 0x8000
	pageReadWrite        = 0x04
	pageExecuteRead      = 0x20
	pageExecuteReadWrite = 0x40
)

func checkDynamicCode() {
	addr, _, err := pVirtualAlloc.Call(0, 4096, memCommit|memReserve, pageExecuteReadWrite)
	report("dyncode-rwx", addr, err)
	if addr != 0 {
		pVirtualFree.Call(addr, 0, memRelease)
	}

	addr, _, err = pVirtualAlloc.Call(0, 4096, memCommit|memReserve, pageReadWrite)
	if addr == 0 {
		report("dyncode-flip", 0, err)
		return
	}
	var old uint32
	r, _, err := pVirtualProtect.Call(addr, 4096, pageExecuteRead, uintptr(unsafe.Pointer(&old)))
	report("dyncode-flip", r, err)
	pVirtualFree.Call(addr, 0, memRelease)
}

// checkFsctl asks for a reparse point on a plain file: error 4390
// (ERROR_NOT_A_REPARSE_POINT) means the FSCTL reached the file system.
func checkFsctl() {
	self, _ := os.Executable()
	name, _ := syscall.UTF16PtrFromString(self)
	const fileReadAttributes = 0x80
	const share = 0x7
	const openExisting = 3
	const backupSemantics = 0x02000000
	const openReparsePoint = 0x00200000
	h, _, err := pCreateFileW.Call(uintptr(unsafe.Pointer(name)), fileReadAttributes, share, 0, openExisting, backupSemantics|openReparsePoint, 0)
	if syscall.Handle(h) == syscall.InvalidHandle {
		report("fsctl", 0, err)
		return
	}
	defer pCloseHandle.Call(h)
	const fsctlGetReparsePoint = 0x000900A8
	var out [16 * 1024]byte
	var got uint32
	r, _, err := pDeviceIoControl.Call(h, fsctlGetReparsePoint, 0, 0, uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)), uintptr(unsafe.Pointer(&got)), 0)
	report("fsctl", r, err)
}

func loadLibrary(path string) (uintptr, error) {
	name, _ := syscall.UTF16PtrFromString(path)
	h, _, err := pLoadLibraryExW.Call(uintptr(unsafe.Pointer(name)), 0, 0)
	if h == 0 {
		return 0, err
	}
	return h, nil
}

func checkImage(check, path string) {
	if _, err := os.Stat(path); err != nil {
		fmt.Printf("%s skipped (%v)\n", check, err)
		return
	}
	h, err := loadLibrary(path)
	report(check, h, err)
	if h != 0 {
		pFreeLibrary.Call(h)
	}
}

// checkSearch loads version.dll by name; the image ships its own copy beside
// the executable, which normal search order prefers over System32's.
func checkSearch() {
	h, err := loadLibrary("version.dll")
	if h == 0 {
		report("image-search", 0, err)
		return
	}
	defer pFreeLibrary.Call(h)
	var buf [1024]uint16
	n, _, _ := pGetModuleFileNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	path := syscall.UTF16ToString(buf[:n])
	where := "app"
	if strings.Contains(strings.ToLower(path), `\system32\`) {
		where = "system32"
	}
	fmt.Printf("image-search %s (%s)\n", where, path)
}

func checkChild() {
	self, err := os.Executable()
	if err != nil {
		fmt.Printf("child error 0 (%v)\n", err)
		return
	}
	cmd := exec.Command(self, "self")
	out, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Printf("child error 0 (%v)\n", err)
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Printf("child error %d (%v)\n", errno(unwrap(err)), err)
		return
	}
	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		fmt.Println(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		fmt.Printf("child error 0 (reading: %v)\n", err)
	}
	if err := cmd.Wait(); err != nil {
		fmt.Printf("child error 0 (%v)\n", err)
		return
	}
	fmt.Println("child ok")
}

func unwrap(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
}

func checkFont(path string) {
	if _, err := os.Stat(path); err != nil {
		fmt.Printf("font skipped (%v)\n", err)
		return
	}
	if err := gdi32.Load(); err != nil {
		fmt.Printf("font error %d (gdi32: %v)\n", errno(unwrap(err)), err)
		return
	}
	name, _ := syscall.UTF16PtrFromString(path)
	const frPrivate = 0x10
	n, _, err := pAddFontResourceExW.Call(uintptr(unsafe.Pointer(name)), frPrivate, 0)
	report("font", n, err)
	if n != 0 {
		pRemoveFontResourceEx.Call(uintptr(unsafe.Pointer(name)), frPrivate, 0)
	}
}

func checkWin32k() {
	if err := user32.Load(); err != nil {
		fmt.Printf("win32k error %d (user32: %v)\n", errno(unwrap(err)), err)
		return
	}
	r, _, err := pGetDesktopWindow.Call()
	report("win32k", r, err)
}

// checkBadHandle closes, then waits on, a handle that was never opened: error
// 6 normally, a STATUS_INVALID_HANDLE exception (and so no line, and a
// crash) under strict handle checks.
func checkBadHandle() {
	r, _, err := pCloseHandle.Call(0x7ff0)
	report("badhandle-close", r, err)
	r, _, err = pWaitForSingleObject.Call(0x7ff4, 0)
	if r == 0xFFFFFFFF {
		fmt.Printf("badhandle-wait error %d (%v)\n", errno(err), err)
	} else {
		fmt.Printf("badhandle-wait returned 0x%x\n", r)
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "self" {
		printPolicies("child.")
		return
	}
	printPolicies("")
	cwd, _ := os.Getwd()
	checkDynamicCode()
	checkFsctl()
	checkImage("image-ms", filepath.Join(cwd, "mitprobe-ms.dll"))
	checkImage("image-unsigned", filepath.Join(cwd, "mitprobe-unsigned.dll"))
	checkImage("image-low", filepath.Join(cwd, "mitprobe-low.dll"))
	if remote := os.Getenv("MITPROBE_REMOTE"); remote != "" {
		checkImage("image-remote", remote)
	}
	checkSearch()
	checkChild()
	checkFont(filepath.Join(cwd, "mitprobe-font.ttf"))
	checkWin32k()
	checkBadHandle()
	fmt.Println("# done")
}
