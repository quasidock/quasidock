package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/quasidock/quasidock/internal/win32"
)

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "quasidock-asuser: "+format+"\n", args...)
	win32.ExitProcess(125)
}

func main() {
	args := os.Args
	if len(args) != 4 {
		die("usage: quasidock-asuser <account file> <output file> <command line>")
	}

	accountData, err := os.ReadFile(args[1])
	if err != nil {
		die("cannot read %s: %v", args[1], err)
	}

	lines := strings.Split(string(accountData), "\n")
	user := ""
	password := ""
	if len(lines) > 0 {
		user = strings.Trim(lines[0], " \r\t")
	}
	if len(lines) > 1 {
		password = strings.Trim(lines[1], "\r")
	}
	if user == "" || password == "" {
		die("%s needs the user name on line 1, the password on line 2", args[1])
	}

	sa := win32.SECURITY_ATTRIBUTES{
		BInheritHandle: win32.TRUE,
	}

	output, err := win32.CreateFile(
		args[2],
		win32.GENERIC_WRITE,
		win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE,
		&sa,
		win32.CREATE_ALWAYS,
		win32.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		die("can't write %s: %v", args[2], err)
	}
	defer win32.CloseHandle(output)

	var startup win32.STARTUPINFOW
	startup.DwFlags = win32.STARTF_USESTDHANDLES
	startup.HStdOutput = output
	startup.HStdError = output

	var info win32.PROCESS_INFORMATION
	if err := win32.CreateProcessWithLogon(
		user,
		".",
		password,
		win32.LOGON_WITH_PROFILE,
		"",
		args[3],
		win32.CREATE_NO_WINDOW,
		nil,
		"",
		&startup,
		&info,
	); err != nil {
		die("couldn't start it as %s: %v", user, err)
	}
	defer win32.CloseHandle(info.HProcess)
	defer win32.CloseHandle(info.HThread)

	_ = win32.WaitForSingleObject(info.HProcess, win32.INFINITE)
	code, _ := win32.GetExitCodeProcess(info.HProcess)
	win32.ExitProcess(uint32(code))
}
