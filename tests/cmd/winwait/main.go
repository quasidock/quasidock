package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/quasidock/quasidock/internal/win32"
)

type Process struct {
	Pid    uint32
	Parent uint32
}

func getProcesses() []Process {
	snapshot, err := win32.CreateToolhelp32Snapshot(win32.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer win32.CloseHandle(snapshot)

	var entry win32.PROCESSENTRY32W
	var all []Process
	if win32.Process32First(snapshot, &entry) {
		for {
			all = append(all, Process{
				Pid:    uint32(entry.Th32ProcessID),
				Parent: uint32(entry.Th32ParentProcessID),
			})
			if !win32.Process32Next(snapshot, &entry) {
				break
			}
		}
	}
	return all
}

func descends(all []Process, pid, root uint32) bool {
	p := pid
	for i := 0; i < 64; i++ {
		if p == root {
			return true
		}
		var parent uint32
		found := false
		for _, proc := range all {
			if proc.Pid == p {
				parent = proc.Parent
				found = true
				break
			}
		}
		if !found || parent == 0 || parent == p {
			return false
		}
		p = parent
	}
	return false
}

type Search struct {
	root      uint32
	processes []Process
	want      string
	found     win32.HANDLE
	title     string
}

func main() {
	args := os.Args[1:]
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: quasidock-winwait <pid> <text> <seconds> [--close|--dismiss]")
		win32.ExitProcess(1)
	}

	root64, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		win32.ExitProcess(1)
	}
	root := uint32(root64)
	want := args[1]
	sec, _ := strconv.Atoi(args[2])
	deadline := time.Now().Add(time.Duration(sec) * time.Second)

	closeWindow := len(args) > 3 && args[3] == "--close"
	dismiss := len(args) > 3 && args[3] == "--dismiss"

	var search Search
	search.root = root
	search.want = want

	visit := func(hwnd uintptr, lparam uintptr) uintptr {
		h := win32.HANDLE(hwnd)
		if !win32.IsWindowVisible(h) {
			return 1
		}
		var pid win32.DWORD
		win32.GetWindowThreadProcessId(h, &pid)
		if !descends(search.processes, uint32(pid), search.root) {
			return 1
		}
		if strings.HasPrefix(search.want, "class:") {
			cls := win32.GetClassName(h)
			if cls != search.want[len("class:"):] {
				return 1
			}
		}
		title := win32.GetWindowText(h)
		if !strings.HasPrefix(search.want, "class:") && !strings.Contains(strings.ToLower(title), strings.ToLower(search.want)) {
			return 1
		}
		search.found = h
		search.title = title
		return 0 // Stop enum
	}

	cb := syscall.NewCallback(visit)

	for {
		search.processes = getProcesses()
		win32.EnumWindows(cb, 0)
		if search.found != 0 {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "no visible window of process %d or its descendants has %q in its title\n", root, want)
			win32.ExitProcess(1)
		}
		time.Sleep(200 * time.Millisecond)
	}

	fmt.Println(search.title)

	if dismiss {
		win32.PostMessage(search.found, win32.WM_CLOSE, 0, 0)
		win32.ExitProcess(0)
	}

	if !closeWindow {
		win32.ExitProcess(0)
	}

	var pid win32.DWORD
	win32.GetWindowThreadProcessId(search.found, &pid)
	pHandle, err := win32.OpenProcess(win32.SYNCHRONIZE, false, pid)
	if err != nil {
		win32.ExitProcess(2)
	}
	defer win32.CloseHandle(pHandle)

	win32.PostMessage(search.found, win32.WM_CLOSE, 0, 0)
	timeLeft := time.Until(deadline)
	if timeLeft < 10*time.Second {
		timeLeft = 10 * time.Second
	}

	r := win32.WaitForSingleObject(pHandle, uint32(timeLeft.Milliseconds()))
	if r == win32.WAIT_OBJECT_0 {
		win32.ExitProcess(0)
	}
	win32.ExitProcess(2)
}
