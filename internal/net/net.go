package net

import (
	"errors"
	"math/bits"
	"net/netip"

	"github.com/quasidock/quasidock/internal/win32"
)

var ErrWinsockUnavailable = errors.New("winsock unavailable")

func Startup() error {
	var data win32.WSADATA
	r, _ := win32.WSAStartup(0x0202, &data)
	if r != 0 {
		return ErrWinsockUnavailable
	}
	return nil
}

type Family int

const (
	FamilyIPv4 Family = 1
	FamilyIPv6 Family = 2
)

type Address struct {
	Family Family
	IPv4   win32.SockaddrIn
	IPv6   win32.SockaddrIn6
}

// IPAddress constructs a Winsock address for an unscoped IP literal.
func IPAddress(ip netip.Addr, port uint16) Address {
	if ip.Is4() {
		a := Loopback(FamilyIPv4, port)
		a.IPv4.Addr = ip.As4()
		return a
	}
	a := Loopback(FamilyIPv6, port)
	a.IPv6.Addr = ip.As16()
	return a
}

func Loopback(family Family, port uint16) Address {
	portBe := bits.ReverseBytes16(port)
	if family == FamilyIPv4 {
		var in win32.SockaddrIn
		in.Family = uint16(win32.AF_INET)
		in.Port = portBe
		in.Addr = [4]byte{127, 0, 0, 1}
		return Address{Family: FamilyIPv4, IPv4: in}
	} else {
		var in6 win32.SockaddrIn6
		in6.Family = uint16(win32.AF_INET6)
		in6.Port = portBe
		in6.Addr = [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
		return Address{Family: FamilyIPv6, IPv6: in6}
	}
}

func (a *Address) Len() int32 {
	if a.Family == FamilyIPv4 {
		return win32.SizeofSockaddrIn
	}
	return win32.SizeofSockaddrIn6
}

func (a *Address) AF() int32 {
	if a.Family == FamilyIPv4 {
		return win32.AF_INET
	}
	return win32.AF_INET6
}

type Result struct {
	Socket win32.SOCKET
	Failed int32
}

func (r Result) Ok() bool {
	return r.Failed == 0
}

func newSocket(family int32) Result {
	s, err := win32.WSASocket(family, win32.SOCK_STREAM, win32.IPPROTO_TCP, nil, 0, win32.WSA_FLAG_OVERLAPPED)
	if s == win32.INVALID_SOCKET {
		return Result{Failed: win32.Errno(err)}
	}
	return Result{Socket: s}
}

func failWith(s win32.SOCKET, err error) Result {
	code := win32.Errno(err)
	if code == 0 {
		code = win32.WSAGetLastError()
	}
	_, _ = win32.Closesocket(s)
	return Result{Failed: code}
}

// Listen creates a socket listening on address with SO_EXCLUSIVEADDRUSE.
func Listen(address Address) Result {
	res := newSocket(address.AF())
	if !res.Ok() {
		return res
	}
	s := res.Socket

	if address.Family == FamilyIPv6 {
		// IPV6_V6ONLY keeps wildcard IPv6 bindings separate from IPv4.
		if ret, err := win32.SetsockoptInt(s, win32.IPPROTO_IPV6, win32.IPV6_V6ONLY, 1); ret != 0 {
			return failWith(s, err)
		}
	}
	if ret, err := win32.SetsockoptInt(s, win32.SOL_SOCKET, win32.SO_EXCLUSIVEADDRUSE, 1); ret != 0 {
		return failWith(s, err)
	}
	var bindRet int32
	var bindErr error
	if address.Family == FamilyIPv4 {
		bindRet, bindErr = win32.BindIn4(s, &address.IPv4)
	} else {
		bindRet, bindErr = win32.BindIn6(s, &address.IPv6)
	}
	if bindRet != 0 {
		return failWith(s, bindErr)
	}
	if ret, err := win32.Listen(s, 64); ret != 0 {
		return failWith(s, err)
	}
	return Result{Socket: s}
}

// Connect creates a socket connected to address with a timeout in milliseconds.
func Connect(address Address, timeoutMs uint32) Result {
	res := newSocket(address.AF())
	if !res.Ok() {
		return res
	}
	s := res.Socket

	nonblocking := uint32(1)
	if ret, err := win32.Ioctlsocket(s, win32.FIONBIO, &nonblocking); ret != 0 {
		return failWith(s, err)
	}

	var connRet int32
	var connErr error
	if address.Family == FamilyIPv4 {
		connRet, connErr = win32.ConnectIn4(s, &address.IPv4)
	} else {
		connRet, connErr = win32.ConnectIn6(s, &address.IPv6)
	}
	if connRet != 0 {
		if win32.Errno(connErr) != win32.WSAEWOULDBLOCK {
			return failWith(s, connErr)
		}
		var writable win32.FdSet
		writable.Count = 1
		writable.Array[0] = s
		failed := writable

		timeout := win32.Timeval{
			Sec:  int32(timeoutMs / 1000),
			Usec: int32((timeoutMs % 1000) * 1000),
		}

		selRes, selErr := win32.Select(0, nil, &writable, &failed, &timeout)
		if selRes == 0 {
			_, _ = win32.Closesocket(s)
			return Result{Failed: win32.WSAETIMEDOUT}
		} else if selRes == win32.SOCKET_ERROR {
			return failWith(s, selErr)
		} else if failed.Count > 0 {
			_, code, _ := win32.GetsockoptInt(s, win32.SOL_SOCKET, win32.SO_ERROR)
			_, _ = win32.Closesocket(s)
			if code != 0 {
				return Result{Failed: code}
			}
			return Result{Failed: win32.WSAECONNREFUSED}
		}
	}

	nonblocking = 0
	if ret, err := win32.Ioctlsocket(s, win32.FIONBIO, &nonblocking); ret != 0 {
		return failWith(s, err)
	}
	return Result{Socket: s}
}

// LocalPort returns the port a socket is bound to.
func LocalPort(s win32.SOCKET) (uint16, bool) {
	var addr win32.SockaddrIn6
	if ret, _ := win32.GetsocknameIn6(s, &addr); ret != 0 {
		return 0, false
	}
	return bits.ReverseBytes16(addr.Port), true
}

// SendAll sends all bytes to socket s; returns false if error or closed early.
func SendAll(s win32.SOCKET, bytes []byte) bool {
	rest := bytes
	for len(rest) > 0 {
		chunkLen := len(rest)
		if chunkLen > 0x7FFFFFFF {
			chunkLen = 0x7FFFFFFF
		}
		n, _ := win32.Send(s, rest[:chunkLen], 0)
		if n <= 0 {
			return false
		}
		rest = rest[n:]
	}
	return true
}
