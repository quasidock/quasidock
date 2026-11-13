package main

import (
	"fmt"
	"os"
	"strconv"
	"unsafe"

	"github.com/zigsaw-io/zigsaw/internal/win32"
)

func main() {
	args := os.Args
	if len(args) == 1 {
		var startup win32.STARTUPINFOW
		startup.Cb = win32.DWORD(unsafe.Sizeof(startup))
		win32.GetStartupInfoW(&startup)
		shortcut := "none"
		if startup.DwFlags&win32.STARTF_TITLEISLINKNAME != 0 && startup.LpTitle != nil {
			shortcut = win32.UTF16PtrToString(startup.LpTitle)
		}
		record := fmt.Sprintf("started without arguments\nshortcut: %s\n", shortcut)
		_ = os.WriteFile("gui-record.txt", []byte(record), 0644)
		win32.ExitProcess(0)
	}

	if len(args) == 2 {
		old, _ := os.ReadFile(args[1])
		newContent := fmt.Sprintf("opened %s\n%s", args[1], string(old))
		_ = os.WriteFile(args[1], []byte(newContent), 0644)
		win32.ExitProcess(0)
	}

	// ATTACH_PARENT_PROCESS is DWORD(-1) = 0xFFFFFFFF
	parentConsole := win32.AttachConsole(0xFFFFFFFF)
	var record string
	if parentConsole {
		record += "parent console: yes\n"
	} else {
		record += "parent console: no\n"
	}
	for _, arg := range args[3:] {
		record += fmt.Sprintf("arg: %s\n", arg)
	}
	_ = os.WriteFile(args[1], []byte(record), 0644)

	if len(args) > 3 {
		fmt.Fprintln(os.Stderr, args[3])
	}

	code, err := strconv.Atoi(args[2])
	if err != nil {
		win32.ExitProcess(1)
	}
	win32.ExitProcess(uint32(code))
}
