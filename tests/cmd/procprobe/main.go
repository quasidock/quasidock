package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/quasidock/quasidock/internal/win32"
)

type Process struct {
	Pid    uint32
	Parent uint32
	Exe    string
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

func integrity(token win32.HANDLE) string {
	rid, err := win32.GetTokenIntegrityRid(token)
	if err != nil {
		return "?"
	}

	if rid < 0x1000 {
		return "untrusted"
	} else if rid < 0x2000 {
		return "low"
	} else if rid < 0x3000 {
		return "medium"
	} else if rid < 0x4000 {
		return "high"
	}
	return "system"
}

func switchValue(line, name string) (string, bool) {
	tokens := strings.Fields(line)
	for _, tok := range tokens {
		tok = strings.Trim(tok, "\"")
		if strings.HasPrefix(tok, "--"+name+"=") {
			return tok[len(name)+3:], true
		}
	}
	return "", false
}

func hasSwitch(line, name string) bool {
	tokens := strings.Fields(line)
	for _, tok := range tokens {
		tok = strings.Trim(tok, "\"")
		if tok == "--"+name {
			return true
		}
	}
	return false
}

var loosening = []string{
	"disable-gpu-sandbox",
	"in-process-gpu",
	"single-process",
	"allow-no-sandbox-job",
	"disable-gpu",
}

func main() {
	args := os.Args[1:]
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: quasidock-procprobe <pid>")
		win32.ExitProcess(2)
	}

	root64, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		win32.ExitProcess(2)
	}
	root := uint32(root64)

	snapshot, err := win32.CreateToolhelp32Snapshot(win32.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		win32.ExitProcess(1)
	}
	defer win32.CloseHandle(snapshot)

	var entry win32.PROCESSENTRY32W
	var all []Process
	if win32.Process32First(snapshot, &entry) {
		for {
			exe := win32.UTF16ToUTF8(entry.SzExeFile[:])
			if idx := strings.IndexByte(exe, 0); idx != -1 {
				exe = exe[:idx]
			}
			all = append(all, Process{
				Pid:    uint32(entry.Th32ProcessID),
				Parent: uint32(entry.Th32ParentProcessID),
				Exe:    exe,
			})
			if !win32.Process32Next(snapshot, &entry) {
				break
			}
		}
	}

	listed := 0
	for _, proc := range all {
		if proc.Pid == root || !descends(all, proc.Pid, root) {
			continue
		}
		listed++
		fmt.Printf("%d %s", proc.Pid, proc.Exe)

		pHandle, err := win32.OpenProcess(win32.PROCESS_QUERY_LIMITED_INFORMATION, false, win32.DWORD(proc.Pid))
		line := ""
		knownLine := false
		if err == nil {
			line, knownLine = win32.QueryProcessCommandLine(pHandle)
		}

		typeVal, ok := switchValue(line, "type")
		if !ok {
			typeVal = "-"
		}
		subVal, ok := switchValue(line, "utility-sub-type")
		if !ok {
			subVal = "-"
		}
		svcVal, ok := switchValue(line, "service-sandbox-type")
		if !ok {
			svcVal = "-"
		}
		fmt.Printf(" type=%s sub=%s svc=%s", typeVal, subVal, svcVal)

		var token win32.HANDLE
		var tErr error
		if pHandle != 0 {
			token, tErr = win32.OpenProcessToken(pHandle, win32.TOKEN_QUERY)
		}
		if pHandle != 0 && tErr == nil {
			isAC, acErr := win32.GetTokenIsAppContainer(token)

			acStr := "?"
			if acErr == nil {
				if isAC {
					acStr = "yes"
				} else {
					acStr = "no"
				}
			}
			restrStr := "no"
			if win32.IsTokenRestricted(token) {
				restrStr = "yes"
			}
			fmt.Printf(" il=%s ac=%s restricted=%s", integrity(token), acStr, restrStr)
			win32.CloseHandle(token)
		} else {
			fmt.Print(" il=? ac=? restricted=?")
		}

		noSbStr := "?"
		if knownLine {
			if hasSwitch(line, "no-sandbox") {
				noSbStr = "yes"
			} else {
				noSbStr = "no"
			}
		}
		fmt.Printf(" no-sandbox=%s sandbox-flags=", noSbStr)

		var foundFlags []string
		for _, flag := range loosening {
			if hasSwitch(line, flag) {
				foundFlags = append(foundFlags, flag)
			} else if _, ok := switchValue(line, flag); ok {
				foundFlags = append(foundFlags, flag)
			}
		}
		if len(foundFlags) > 0 {
			fmt.Println(strings.Join(foundFlags, ","))
		} else {
			fmt.Println("-")
		}

		if pHandle != 0 {
			win32.CloseHandle(pHandle)
		}
	}

	if listed > 0 {
		win32.ExitProcess(0)
	}
	win32.ExitProcess(1)
}
