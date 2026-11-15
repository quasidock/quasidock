// clifixture exercises foreground CLI behavior without downloaded toolchains.
package main

import (
	"encoding/json"
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
		case "console-run":
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
			if err := cmd.Run(); err != nil {
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
		case "signal-hold", "tree-hold":
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
			if args[0] == "tree-hold" {
				exe, err := os.Executable()
				if err != nil {
					panic(err)
				}
				child := exec.Command(exe, "signal-hold", ready+".child", "ignore")
				child.Stdout, child.Stderr = os.Stdout, os.Stderr
				if err := child.Start(); err != nil {
					panic(err)
				}
			}
			if err := os.WriteFile(ready, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
				panic(err)
			}
			fmt.Printf("started %d\n", os.Getpid())
			for {
				if win32.WaitForSingleObject(signal, win32.INFINITE) != win32.WAIT_OBJECT_0 {
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
		case "log":
			if len(args) > 1 {
				fmt.Fprintln(os.Stdout, args[1])
			}
			if len(args) > 2 {
				fmt.Fprintln(os.Stderr, args[2])
			}
			return
		case "lines":
			for _, line := range args[1:] {
				fmt.Fprintln(os.Stdout, line)
			}
			return
		}
	}
	json.NewEncoder(os.Stdout).Encode(args)
}
