package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

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

func newTextBuffer() *TextBuffer {
	return &TextBuffer{
		buf: make([]byte, 64*1024),
	}
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
	pid     uint32
	text    *TextBuffer
	closed  bool
}

func startConsole(commandLine string) (*Console, error) {
	inRead, inWrite, err := win32.CreatePipe(nil, 0)
	if err != nil {
		return nil, err
	}
	outRead, outWrite, err := win32.CreatePipe(nil, 0)
	if err != nil {
		win32.CloseHandle(inRead)
		win32.CloseHandle(inWrite)
		return nil, err
	}

	var pc uintptr
	hr := win32.CreatePseudoConsole(win32.COORD{X: 120, Y: 30}, inRead, outWrite, 0, &pc)
	win32.CloseHandle(inRead)
	win32.CloseHandle(outWrite)
	if hr < 0 {
		win32.CloseHandle(inWrite)
		win32.CloseHandle(outRead)
		return nil, fmt.Errorf("CreatePseudoConsole failed: 0x%x", hr)
	}

	var size uintptr
	win32.InitializeProcThreadAttributeList(nil, 1, 0, &size)
	attrs := make([]byte, size)
	if !win32.InitializeProcThreadAttributeList(unsafe.Pointer(&attrs[0]), 1, 0, &size) {
		win32.ClosePseudoConsole(pc)
		return nil, fmt.Errorf("InitializeProcThreadAttributeList failed")
	}
	defer win32.DeleteProcThreadAttributeList(unsafe.Pointer(&attrs[0]))

	if !win32.UpdateProcThreadAttribute(unsafe.Pointer(&attrs[0]), 0, win32.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, unsafe.Pointer(pc), unsafe.Sizeof(pc), nil, nil) {
		win32.ClosePseudoConsole(pc)
		return nil, fmt.Errorf("UpdateProcThreadAttribute failed")
	}

	var startup win32.STARTUPINFOEXW
	startup.StartupInfo.Cb = win32.DWORD(unsafe.Sizeof(startup))
	startup.StartupInfo.DwFlags = win32.STARTF_USESTDHANDLES
	startup.LpAttributeList = uintptr(unsafe.Pointer(&attrs[0]))

	var info win32.PROCESS_INFORMATION
	err = win32.CreateProcess(
		"",
		commandLine,
		nil,
		nil,
		false,
		win32.EXTENDED_STARTUPINFO_PRESENT,
		nil,
		"",
		unsafe.Pointer(&startup.StartupInfo),
		&info,
	)
	if err != nil {
		win32.ClosePseudoConsole(pc)
		return nil, err
	}
	win32.CloseHandle(info.HThread)

	tb := newTextBuffer()
	go tb.Drain(outRead)

	return &Console{
		pc:      pc,
		input:   inWrite,
		output:  outRead,
		process: info.HProcess,
		pid:     uint32(info.DwProcessId),
		text:    tb,
	}, nil
}

func (c *Console) WaitFor(needle string, timeoutMs time.Duration) (string, bool) {
	deadline := time.Now().Add(timeoutMs)
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

func installedNode(store string) (string, error) {
	refPath := filepath.Join(store, "refs", "org.nodejs.node.json")
	ref, err := os.ReadFile(refPath)
	if err != nil {
		return "", err
	}
	at := bytes.Index(ref, []byte("sha256:"))
	if at == -1 || at+7+64 > len(ref) {
		return "", fmt.Errorf("bad ref")
	}
	manifestDigest := string(ref[at+7 : at+7+64])
	manifestPath := filepath.Join(store, "blobs", "sha256", manifestDigest)
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	layerAt := bytes.LastIndex(manifest, []byte("sha256:"))
	if layerAt == -1 || layerAt+7+64 > len(manifest) {
		return "", fmt.Errorf("bad manifest")
	}
	layerDigest := string(manifest[layerAt+7 : layerAt+7+64])
	return filepath.Join(store, "deploy", layerDigest, "node.exe"), nil
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
