package forward

import (
	"fmt"
	"unsafe"

	"github.com/zigsaw-io/zigsaw/internal/net"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

type Forwards struct {
	RelayIn  win32.HANDLE
	RelayOut win32.HANDLE
	In       win32.HANDLE
	Out      win32.HANDLE
	Ports    []uint16
	Connect  []uint16
}

func Open(ports []uint16, listen []uint16) (*Forwards, error) {
	relayIn, out, err := win32.CreatePipe(nil, 0)
	if err != nil {
		return nil, fmt.Errorf("CreatePipe: %w", err)
	}
	in, relayOut, err := win32.CreatePipe(nil, 0)
	if err != nil {
		win32.CloseHandle(relayIn)
		win32.CloseHandle(out)
		return nil, fmt.Errorf("CreatePipe: %w", err)
	}

	return &Forwards{
		RelayIn:  relayIn,
		RelayOut: relayOut,
		In:       in,
		Out:      out,
		Ports:    append([]uint16(nil), ports...),
		Connect:  append([]uint16(nil), listen...),
	}, nil
}

func (f *Forwards) Args() []string {
	var out []string
	out = append(out, fmt.Sprintf("forward=%d:%d", f.RelayIn, f.RelayOut))
	for _, port := range f.Connect {
		out = append(out, fmt.Sprintf("use=%d", port))
	}
	return out
}

func (f *Forwards) Serve(relay win32.HANDLE) bool {
	win32.CloseHandle(f.RelayIn)
	win32.CloseHandle(f.RelayOut)
	f.RelayIn = 0
	f.RelayOut = 0

	var ready [1]byte
	if !readAll(f.In, ready[:]) {
		return false
	}

	relayPid := win32.GetProcessId(relay)
	go f.answer(relayPid)
	return true
}

func (f *Forwards) Close() {
	if f.RelayIn != 0 {
		win32.CloseHandle(f.RelayIn)
		f.RelayIn = 0
	}
	if f.RelayOut != 0 {
		win32.CloseHandle(f.RelayOut)
		f.RelayOut = 0
	}
	if f.In != 0 {
		win32.CloseHandle(f.In)
		f.In = 0
	}
	if f.Out != 0 {
		win32.CloseHandle(f.Out)
		f.Out = 0
	}
}

func (f *Forwards) answer(relayPid win32.DWORD) {
	if err := net.Startup(); err != nil {
		return
	}

	for {
		var index [1]byte
		if !readAll(f.In, index[:]) || int(index[0]) >= len(f.Ports) {
			return
		}

		s := connectPort(f.Ports[index[0]])
		if s == 0 {
			if !writeAll(f.Out, []byte{1}) {
				return
			}
			continue
		}

		var info win32.WSAPROTOCOL_INFOW
		r, _ := win32.WSADuplicateSocket(s, relayPid, &info)
		if r != 0 {
			_, _ = win32.Closesocket(s)
			if !writeAll(f.Out, []byte{1}) {
				return
			}
			continue
		}

		infoBytes := unsafe.Slice((*byte)(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		if !writeAll(f.Out, []byte{0}) || !writeAll(f.Out, infoBytes) {
			_, _ = win32.Closesocket(s)
			return
		}

		// Wait for relay ack before closing socket
		var ack [1]byte
		if !readAll(f.In, ack[:]) {
			_, _ = win32.Closesocket(s)
			return
		}
		_, _ = win32.Closesocket(s)
	}
}

func connectPort(port uint16) win32.SOCKET {
	for _, fam := range []net.Family{net.FamilyIPv4, net.FamilyIPv6} {
		res := net.Connect(net.Loopback(fam, port), 2000)
		if res.Ok() {
			return res.Socket
		}
	}
	return 0
}

func readAll(h win32.HANDLE, buf []byte) bool {
	got := 0
	for got < len(buf) {
		n, err := win32.ReadFile(h, buf[got:])
		if err != nil || n == 0 {
			return false
		}
		got += n
	}
	return true
}

func writeAll(h win32.HANDLE, bytes []byte) bool {
	sent := 0
	for sent < len(bytes) {
		n, err := win32.WriteFile(h, bytes[sent:])
		if err != nil || n == 0 {
			return false
		}
		sent += n
	}
	return true
}
