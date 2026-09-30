package main

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"github.com/quasidock/quasidock/internal/win32"
)

func rest(cmdLine string, skip int) string {
	i := 0
	left := skip
	for left > 0 {
		for i < len(cmdLine) && (cmdLine[i] == ' ' || cmdLine[i] == '\t') {
			i++
		}
		quoted := false
		for i < len(cmdLine) && (quoted || (cmdLine[i] != ' ' && cmdLine[i] != '\t')) {
			if cmdLine[i] == '"' {
				quoted = !quoted
			}
			i++
		}
		left--
	}
	for i < len(cmdLine) && (cmdLine[i] == ' ' || cmdLine[i] == '\t') {
		i++
	}
	if i >= len(cmdLine) {
		return ""
	}
	return cmdLine[i:]
}

func start(line string, flags win32.DWORD) (*win32.PROCESS_INFORMATION, error) {
	var startup win32.STARTUPINFOW
	startup.Cb = win32.DWORD(unsafe.Sizeof(startup))
	var info win32.PROCESS_INFORMATION
	err := win32.CreateProcess(
		"",
		line,
		nil,
		nil,
		true,
		flags,
		nil,
		"",
		unsafe.Pointer(&startup),
		&info,
	)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

func main() {
	args := os.Args[1:]
	if len(args) < 1 {
		win32.ExitProcess(2)
	}

	rawLine := win32.UTF16PtrToString(win32.GetCommandLineW())

	if args[0] == "breakaway-child" {
		var inJob win32.BOOL
		win32.IsProcessInJob(win32.GetCurrentProcess(), 0, &inJob)
		if inJob != 0 {
			fmt.Println("kept")
		} else {
			fmt.Println("left")
		}
		win32.ExitProcess(0)
	}

	if args[0] == "breakaway" {
		// Ourselves again with breakaway-child
		exe := strings.TrimSpace(rawLine[:len(rawLine)-len(rest(rawLine, 1))])
		childLine := exe + " breakaway-child"
		info, err := start(childLine, win32.CREATE_BREAKAWAY_FROM_JOB)
		if err != nil {
			fmt.Printf("refused (error %v)\n", err)
			win32.ExitProcess(0)
		}
		_ = win32.WaitForSingleObject(info.HProcess, win32.INFINITE)
		win32.CloseHandle(info.HThread)
		win32.CloseHandle(info.HProcess)
		win32.ExitProcess(0)
	}

	if args[0] == "injob" {
		job, err := win32.CreateJobObject("")
		if err != nil {
			win32.ExitProcess(125)
		}
		defer win32.CloseHandle(job)

		var limit win32.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		limit.BasicLimitInformation.LimitFlags = win32.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if err := win32.SetInformationJobObject(job, win32.JobObjectExtendedLimitInformation, unsafe.Pointer(&limit), win32.DWORD(unsafe.Sizeof(limit))); err != nil {
			win32.ExitProcess(125)
		}

		childCmd := rest(rawLine, 2)
		info, err := start(childCmd, win32.CREATE_SUSPENDED)
		if err != nil {
			win32.ExitProcess(125)
		}
		if err := win32.AssignProcessToJobObject(job, info.HProcess); err != nil {
			win32.ExitProcess(125)
		}
		_, _ = win32.ResumeThread(info.HThread)
		win32.WaitForSingleObject(info.HProcess, win32.INFINITE)
		code, _ := win32.GetExitCodeProcess(info.HProcess)
		win32.CloseHandle(info.HThread)
		win32.CloseHandle(info.HProcess)
		win32.ExitProcess(uint32(code))
	}

	win32.ExitProcess(2)
}
