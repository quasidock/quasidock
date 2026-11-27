package win32

import (
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// InheritedPipe performs event-based I/O on a handle whose file object may
// already be associated with the originating process's I/O completion port.
type InheritedPipe struct {
	handle HANDLE
	mu     sync.Mutex
	active sync.WaitGroup
	closed bool
}

func AdoptPipe(handle HANDLE) *InheritedPipe {
	return &InheritedPipe{handle: handle}
}

func (p *InheritedPipe) Read(buf []byte) (int, error) {
	n, err := p.transfer(buf, syscall.ReadFile)
	if errors.Is(err, syscall.ERROR_BROKEN_PIPE) {
		return n, io.EOF
	}
	return n, err
}

func (p *InheritedPipe) Write(buf []byte) (int, error) {
	n, err := p.transfer(buf, syscall.WriteFile)
	if err == nil && n != len(buf) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (p *InheritedPipe) transfer(buf []byte, call func(syscall.Handle, []byte, *uint32, *syscall.Overlapped) error) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	event, err := CreateEvent("", true, false)
	if err != nil {
		return 0, err
	}
	defer CloseHandle(event)
	// Low bit suppresses completion-port delivery for this request.
	ov := syscall.Overlapped{HEvent: syscall.Handle(event | 1)}
	var pins runtime.Pinner
	pins.Pin(&ov)
	pins.Pin(&buf[0])
	defer pins.Unpin()
	var n uint32
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, os.ErrClosed
	}
	p.active.Add(1)
	defer p.active.Done()
	err = call(syscall.Handle(p.handle), buf, &n, &ov)
	p.mu.Unlock()
	if errors.Is(err, syscall.ERROR_IO_PENDING) {
		ok, _, waitErr := procGetOverlappedResult.Call(p.handle, uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&n)), 1)
		err = nil
		if ok == 0 {
			err = waitErr
		}
	}
	return int(n), err
}

func (p *InheritedPipe) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	CancelIoEx(p.handle, nil)
	p.mu.Unlock()
	p.active.Wait()
	CloseHandle(p.handle)
	return nil
}
