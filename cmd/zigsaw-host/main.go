package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/process"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

func isBatch(path string) bool {
	ext := filepath.Ext(path)
	return strings.EqualFold(ext, ".cmd") || strings.EqualFold(ext, ".bat")
}

func variable(block []uint16, name string) (string, bool) {
	rest := block
	for len(rest) > 0 && rest[0] != 0 {
		end := -1
		for i, c := range rest {
			if c == 0 {
				end = i
				break
			}
		}
		if end == -1 {
			break
		}
		entry := rest[:end]
		rest = rest[end+1:]

		eq := -1
		for i := 1; i < len(entry); i++ {
			if entry[i] == '=' {
				eq = i
				break
			}
		}
		if eq == -1 || eq != len(name) {
			continue
		}

		match := true
		for i := 0; i < eq; i++ {
			c := entry[i]
			if c >= 0x80 || !strings.EqualFold(string(rune(c)), string(name[i])) {
				match = false
				break
			}
		}
		if match {
			val := win32.UTF16ToUTF8(entry[eq+1:])
			return val, true
		}
	}
	return "", false
}

func existing(dir, program, ext string) string {
	var full string
	if dir != "" {
		full = strings.TrimRight(dir, "\\/") + "\\" + program + ext
	} else {
		full = program + ext
	}
	stat, err := os.Stat(full)
	if err == nil && !stat.IsDir() {
		return full
	}
	return ""
}

func tryNames(dir, pathext, program string) string {
	if filepath.Ext(program) != "" {
		if found := existing(dir, program, ""); found != "" {
			return found
		}
	}
	for _, ext := range strings.Split(pathext, ";") {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}
		if found := existing(dir, program, ext); found != "" {
			return found
		}
	}
	return ""
}

func find(path, pathext, program string) string {
	if strings.ContainsAny(program, "\\/:") {
		return tryNames("", pathext, program)
	}
	for _, raw := range strings.Split(path, ";") {
		dir := strings.Trim(raw, "\" ")
		if dir == "" {
			continue
		}
		if found := tryNames(dir, pathext, program); found != "" {
			return found
		}
	}
	return ""
}

func host() uint32 {
	rawLine := win32.UTF16PtrToString(win32.GetCommandLineW())
	argv, err := win32.CommandLineToArgvW(rawLine)
	if err != nil || len(argv) < 2 {
		_, _ = fmt.Fprintln(os.Stderr, "zigsaw-host: usage: zigsaw-host <program> [arguments]: runs one of your programs in your real environment")
		return 2
	}

	program := argv[1]
	args := argv[2:]

	block, err := process.UserEnvironmentBlock()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "zigsaw-host: cannot get user environment block: %v\n", err)
		return 1
	}

	path, _ := variable(block, "PATH")
	pathext, ok := variable(block, "PATHEXT")
	if !ok {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}

	exe := find(path, pathext, program)
	if exe == "" {
		_, _ = fmt.Fprintf(os.Stderr, "zigsaw-host: %s isn't on your PATH\n", program)
		return 127
	}

	if isBatch(exe) {
		root, ok := variable(block, "SystemRoot")
		if !ok {
			root = "C:\\Windows"
		}
		cmd := root + "\\System32\\cmd.exe"
		commandLine, err := process.BuildBatchCommandLine(cmd, exe, args)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "zigsaw-host: %s is a batch file: %v\n", exe, err)
			return 1
		}
		code, err := process.Spawn(process.SpawnSpec{
			Exe:         cmd,
			CommandLine: commandLine,
			EnvBlock:    block,
		})
		if err != nil {
			return 1
		}
		return code
	}

	commandLine := process.BuildCommandLine(exe, args)
	code, err := process.Spawn(process.SpawnSpec{
		Exe:         exe,
		CommandLine: commandLine,
		EnvBlock:    block,
	})
	if err != nil {
		return 1
	}
	return code
}

func main() {
	code := host()
	win32.ExitProcess(code)
}
