// clifixture exercises foreground CLI behavior without downloaded toolchains.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/win32"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "wait-path":
			if err := os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
				panic(err)
			}
			for {
				if _, err := os.Stat(args[2]); err == nil {
					code, _ := strconv.Atoi(args[3])
					os.Exit(code)
				}
				time.Sleep(10 * time.Millisecond)
			}
		case "duplex":
			if _, err := io.Copy(io.MultiWriter(os.Stdout, os.Stderr), os.Stdin); err != nil {
				os.Exit(5)
			}
			return
		case "close-stdin":
			_ = os.Stdin.Close()
			time.Sleep(100 * time.Millisecond)
			fmt.Fprintln(os.Stdout, "closed-input-output")
			fmt.Fprintln(os.Stderr, "closed-input-error")
			os.Exit(27)
		case "console-run", "console-run-check-mode":
			// Test runners may inherit Ctrl+C-disabled state from their launcher.
			// Establish the same state as an ordinary interactive shell, keeping
			// this wrapper alive while the application handles the console event.
			win32.SetConsoleCtrlHandler(0, false)
			handler := syscall.NewCallback(func(event uint32) uintptr {
				if event == win32.CTRL_C_EVENT || event == win32.CTRL_BREAK_EVENT {
					return 1
				}
				return 0
			})
			win32.SetConsoleCtrlHandler(handler, true)
			defer win32.SetConsoleCtrlHandler(handler, false)
			cmd := exec.Command(args[1], args[2:]...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			var originalMode win32.DWORD
			inputHandle := win32.GetStdHandle(win32.STD_INPUT_HANDLE)
			if args[0] == "console-run-check-mode" {
				// CreateProcess with a pseudoconsole can leave the wrapper's
				// standard handles unset. Open its actual console explicitly.
				input, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
				if err != nil {
					panic(err)
				}
				defer func() { _ = input.Close() }()
				output, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
				if err != nil {
					panic(err)
				}
				defer func() { _ = output.Close() }()
				cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, output
				inputHandle = win32.HANDLE(input.Fd())
			}
			win32.GetConsoleMode(inputHandle, &originalMode)
			commandErr := cmd.Run()
			if args[0] == "console-run-check-mode" {
				var finalMode win32.DWORD
				if !win32.GetConsoleMode(inputHandle, &finalMode) || originalMode != finalMode {
					fmt.Fprintf(cmd.Stderr, "console mode changed: %#x -> %#x\n", originalMode, finalMode)
					os.Exit(91)
				}
			}
			if err := commandErr; err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					win32.ExitProcess(uint32(exit.ExitCode()))
				}
				panic(err)
			}
			return
		case "state-hold":
			path := os.ExpandEnv(args[1])
			data, _ := os.ReadFile(path)
			count, _ := strconv.Atoi(string(data))
			count++
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				panic(err)
			}
			key, err := regkey.Create(win32.HKEY_CURRENT_USER, args[2])
			if err != nil {
				panic(err)
			}
			name := "Value"
			value, _ := key.GetString(name)
			registryCount := 0
			if value != nil {
				registryCount, _ = strconv.Atoi(*value)
			}
			registryCount++
			if err := key.SetString(&name, strconv.Itoa(registryCount)); err != nil {
				panic(err)
			}
			key.Close()
			fmt.Printf("state file=%d registry=%d\n", count, registryCount)
			if err := os.WriteFile(path, []byte(strconv.Itoa(count)), 0o644); err != nil {
				panic(err)
			}
			for {
				time.Sleep(time.Second)
			}
		case "signal-hold", "tree-hold", "heartbeat", "heartbeat-tree":
			signal := win32.HANDLE(0)
			var err error
			signal, err = win32.CreateEvent("", false, false)
			if err != nil {
				panic(err)
			}
			defer win32.CloseHandle(signal)
			var lastSignal atomic.Uint32
			handler := syscall.NewCallback(func(event uint32) uintptr {
				if event == win32.CTRL_BREAK_EVENT || event == win32.CTRL_C_EVENT {
					lastSignal.Store(event)
					win32.SetEvent(signal)
					return 1
				}
				return 0
			})
			if !win32.SetConsoleCtrlHandler(handler, true) {
				panic("SetConsoleCtrlHandler failed")
			}
			defer win32.SetConsoleCtrlHandler(handler, false)
			ready := os.ExpandEnv(args[1])
			if args[2] == "no-console" {
				win32.FreeConsole()
			}
			heartbeat := args[0] == "heartbeat" || args[0] == "heartbeat-tree"
			if args[0] == "tree-hold" || args[0] == "heartbeat-tree" {
				exe, err := os.Executable()
				if err != nil {
					panic(err)
				}
				childMode := "signal-hold"
				if heartbeat {
					childMode = "heartbeat"
				}
				child := exec.Command(exe, childMode, ready+".child", "ignore")
				// Server AppContainers cannot open NUL; reuse the stdin handle
				// supplied by the supervisor instead of os/exec's nil default.
				child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
				if err := child.Start(); err != nil {
					panic(err)
				}
			}
			if err := os.WriteFile(ready, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
				panic(err)
			}
			fmt.Printf("started %d\n", os.Getpid())
			var ticks *os.File
			wait := win32.INFINITE
			if heartbeat {
				ticks, err = os.OpenFile(ready+".ticks", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
				if err != nil {
					panic(err)
				}
				defer func() { _ = ticks.Close() }()
				wait = 20
			}
			for {
				if ticks != nil {
					if _, err := ticks.Write([]byte{'x'}); err != nil {
						panic(err)
					}
					fmt.Printf("tick %s\n", filepath.Base(ready))
				}
				result := win32.WaitForSingleObject(signal, wait)
				if result == win32.WAIT_TIMEOUT && heartbeat {
					continue
				}
				if result != win32.WAIT_OBJECT_0 {
					panic("signal wait failed")
				}
				fmt.Println("signal received")
				if err := os.WriteFile(ready+".signal", []byte(strconv.FormatUint(uint64(lastSignal.Load()), 10)), 0o644); err != nil {
					panic(err)
				}
				if args[2] == "exit" {
					return
				}
			}
		case "write-path":
			p := os.ExpandEnv(args[1])
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				panic(err)
			}
			if err := os.WriteFile(p, []byte(args[2]), 0o644); err != nil {
				panic(err)
			}
			return
		case "read-path":
			b, err := os.ReadFile(os.ExpandEnv(args[1]))
			if err != nil {
				panic(err)
			}
			fmt.Print(string(b))
			return
		case "hold-path":
			p := os.ExpandEnv(args[1])
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				panic(err)
			}
			if err := os.WriteFile(p, []byte("ready"), 0o644); err != nil {
				panic(err)
			}
			for {
				time.Sleep(100 * time.Millisecond)
				if err := os.WriteFile(p, []byte("ready"), 0o644); err != nil {
					panic(err)
				}
			}
		case "registry-write":
			key, err := regkey.Create(win32.HKEY_CURRENT_USER, args[1])
			if err != nil {
				panic(err)
			}
			defer key.Close()
			name := "Value"
			if err := key.SetString(&name, args[2]); err != nil {
				panic(err)
			}
			return
		case "registry-read":
			key, err := regkey.Open(win32.HKEY_CURRENT_USER, args[1], win32.KEY_READ)
			if err != nil || key == nil {
				os.Exit(3)
			}
			defer key.Close()
			v, err := key.GetString("Value")
			if err != nil || v == nil {
				os.Exit(3)
			}
			fmt.Print(*v)
			return
		case "env":
			for _, name := range args[1:] {
				value, ok := os.LookupEnv(name)
				fmt.Printf("%s=%s:%t\n", name, value, ok)
			}
			return
		case "exit":
			code, _ := strconv.Atoi(args[1])
			os.Exit(code)
		case "hold":
			if err := os.WriteFile(args[1], []byte("ready"), 0o644); err != nil {
				panic(err)
			}
			for {
				time.Sleep(time.Second)
			}
		case "write":
			if err := os.WriteFile(os.Getenv("HOME")+"\\saved.txt", []byte(args[1]), 0o644); err != nil {
				panic(err)
			}
			return
		case "read":
			data, err := os.ReadFile(os.Getenv("HOME") + "\\saved.txt")
			if err != nil {
				os.Exit(3)
			}
			fmt.Print(string(data))
			return
		case "stdin":
			data, err := io.ReadAll(os.Stdin)
			if err != nil || len(data) == 0 {
				os.Exit(4)
			}
			_, _ = os.Stdout.Write(data)
			return
		case "log", "log-exit":
			if len(args) > 1 {
				fmt.Fprintln(os.Stdout, args[1])
			}
			if len(args) > 2 {
				fmt.Fprintln(os.Stderr, args[2])
			}
			if args[0] == "log-exit" {
				code, _ := strconv.Atoi(args[3])
				os.Exit(code)
			}
			return
		case "lines":
			for _, line := range args[1:] {
				fmt.Fprintln(os.Stdout, line)
			}
			return
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(args)
}
