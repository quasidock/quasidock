package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func sendBreak(pid uint32) uint32 {
	win32.FreeConsole()
	if !win32.AttachConsole(win32.DWORD(pid)) {
		return 10
	}
	win32.SetConsoleCtrlHandler(0, true)
	if !win32.GenerateConsoleCtrlEvent(win32.CTRL_BREAK_EVENT, 0) {
		return 11
	}
	return 0
}

type TextBuffer struct {
	buf []byte
	len int64
	mu  sync.Mutex
}

func (t *TextBuffer) Get() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := atomic.LoadInt64(&t.len)
	return string(t.buf[:n])
}

func (t *TextBuffer) Drain(pipe win32.HANDLE) {
	spill := make([]byte, 4096)
	for {
		t.mu.Lock()
		curLen := atomic.LoadInt64(&t.len)
		dest := t.buf[curLen:]
		if len(dest) == 0 {
			dest = spill
		}
		t.mu.Unlock()

		n, err := win32.ReadFile(pipe, dest)
		if err != nil || n == 0 {
			return
		}
		t.mu.Lock()
		if curLen < int64(len(t.buf)) {
			atomic.StoreInt64(&t.len, curLen+int64(n))
		}
		t.mu.Unlock()
	}
}

type Console struct {
	pc      uintptr
	input   win32.HANDLE
	output  win32.HANDLE
	process win32.HANDLE
	text    *TextBuffer
	closed  bool
}

func (c *Console) WaitFor(needle string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		txt := c.text.Get()
		if idx := strings.Index(txt, needle); idx != -1 {
			time.Sleep(100 * time.Millisecond)
			now := c.text.Get()
			rest := now[idx+len(needle):]
			end := strings.IndexAny(rest, "\r\n")
			if end == -1 {
				end = len(rest)
			}
			return rest[:end], true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", false
}

func (c *Console) PressCtrlC() error {
	_, err := win32.WriteFile(c.input, []byte{3})
	return err
}

func (c *Console) ExitCode(timeoutMs uint32) (uint32, bool) {
	r := win32.WaitForSingleObject(c.process, timeoutMs)
	if r != win32.WAIT_OBJECT_0 {
		return 0, false
	}
	code, _ := win32.GetExitCodeProcess(c.process)
	return uint32(code), true
}

func (c *Console) Close() {
	if c.closed {
		return
	}
	win32.ClosePseudoConsole(c.pc)
	c.closed = true
}

func (c *Console) Deinit() {
	c.Close()
	win32.CloseHandle(c.input)
	win32.CloseHandle(c.output)
	win32.CloseHandle(c.process)
}

func installedNode(root string) (string, error) {
	st, err := store.Open(root, false, "")
	if err != nil {
		return "", err
	}
	ref, err := st.ResolveLocalImage("", "node")
	if err != nil {
		return "", err
	}
	manifest, err := st.ReadManifest(ref.Manifest)
	if err != nil {
		return "", err
	}
	layer := oci.OwnLayer(manifest)
	return filepath.Join(root, "deploy", oci.DigestHex(layer.Digest), "node.exe"), nil
}

func main() {
	args := os.Args[1:]
	if len(args) == 2 && args[0] == "break" {
		pid, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil {
			win32.ExitProcess(1)
		}
		win32.ExitProcess(sendBreak(uint32(pid)))
	}

	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: quasidock-ctrlc <quasidock.exe> <store> <work-dir>")
		win32.ExitProcess(2)
	}

	win32.SetConsoleCtrlHandler(0, false)
	workDir := args[2]
	if err := win32.SetCurrentDirectory(workDir); err != nil {
		fmt.Fprintf(os.Stderr, "cannot set work dir: %v\n", err)
		win32.ExitProcess(1)
	}

	nodeExe, err := installedNode(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "installedNode: %v\n", err)
		win32.ExitProcess(1)
	}

	path := filepath.Dir(nodeExe) + ";" + os.Getenv("PATH")
	_ = win32.SetEnvironmentVariable("PATH", path)

	fmt.Println("ctrlc test driver initialized successfully")
	win32.ExitProcess(0)
}
