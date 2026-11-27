package main

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quasidock/quasidock/internal/net"
	"github.com/quasidock/quasidock/internal/win32"
)

type UpstreamKind int

const (
	UpstreamApp UpstreamKind = iota
	UpstreamForward
)

type Upstream struct {
	Kind    UpstreamKind
	Port    uint16
	Forward uint8
}

type Pipes struct {
	In  win32.HANDLE
	Out win32.HANDLE
}

var (
	parent     *Pipes
	parentLock sync.Mutex
)

type ArgKind int

const (
	ArgService ArgKind = iota
	ArgForward
	ArgUse
)

type Arg struct {
	Kind    ArgKind
	Socket  win32.SOCKET
	Port    uint16
	Forward Pipes
	Use     uint16
}

func parseArg(arg string) (*Arg, bool) {
	eq := strings.IndexByte(arg, '=')
	if eq == -1 {
		return nil, false
	}
	key := arg[:eq]
	value := arg[eq+1:]

	if key == "use" {
		port, err := strconv.ParseUint(value, 10, 16)
		if err != nil || port == 0 {
			return nil, false
		}
		return &Arg{Kind: ArgUse, Use: uint16(port)}, true
	}

	if key == "forward" {
		colon := strings.IndexByte(value, ':')
		if colon == -1 {
			return nil, false
		}
		inHandle, err1 := strconv.ParseUint(value[:colon], 10, 64)
		outHandle, err2 := strconv.ParseUint(value[colon+1:], 10, 64)
		if err1 != nil || err2 != nil || inHandle == 0 || outHandle == 0 {
			return nil, false
		}
		return &Arg{
			Kind: ArgForward,
			Forward: Pipes{
				In:  win32.HANDLE(inHandle),
				Out: win32.HANDLE(outHandle),
			},
		}, true
	}

	sock, err1 := strconv.ParseUint(key, 10, 64)
	port, err2 := strconv.ParseUint(value, 10, 16)
	if err1 != nil || err2 != nil || port == 0 {
		return nil, false
	}
	return &Arg{
		Kind:   ArgService,
		Socket: win32.SOCKET(sock),
		Port:   uint16(port),
	}, true
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

func pump(from win32.SOCKET, to win32.SOCKET) {
	buf := make([]byte, 16*1024)
	for {
		n, err := win32.Recv(from, buf, 0)
		if err == nil && n == 0 {
			_, _ = win32.Shutdown(to, win32.SD_SEND)
			return
		}
		if err != nil || n < 0 || !net.SendAll(to, buf[:n]) {
			_, _ = win32.Shutdown(from, win32.SD_BOTH)
			_, _ = win32.Shutdown(to, win32.SD_BOTH)
			return
		}
	}
}

func connectApp(port uint16) win32.SOCKET {
	families := []net.Family{net.FamilyIPv4, net.FamilyIPv6}
	for i := 0; i < 20; i++ {
		for _, f := range families {
			res := net.Connect(net.Loopback(f, port), 1000)
			if res.Ok() {
				return res.Socket
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return win32.INVALID_SOCKET
}

func askParent(index uint8) win32.SOCKET {
	if parent == nil {
		return win32.INVALID_SOCKET
	}
	parentLock.Lock()
	defer parentLock.Unlock()

	if !writeAll(parent.Out, []byte{index}) {
		return win32.INVALID_SOCKET
	}
	status := make([]byte, 1)
	if !readAll(parent.In, status) || status[0] != 0 {
		return win32.INVALID_SOCKET
	}

	var info win32.WSAPROTOCOL_INFOW
	if !readAll(parent.In, info.Data[:]) {
		return win32.INVALID_SOCKET
	}

	s, err := win32.WSASocket(win32.FROM_PROTOCOL_INFO, win32.FROM_PROTOCOL_INFO, win32.FROM_PROTOCOL_INFO, &info, 0, win32.WSA_FLAG_OVERLAPPED)
	if err != nil || s == win32.INVALID_SOCKET {
		return win32.INVALID_SOCKET
	}
	// quasidock keeps its own until this one is open.
	_ = writeAll(parent.Out, []byte{1})
	return s
}

func relay(client win32.SOCKET, upstream Upstream) {
	defer func() { _, _ = win32.Closesocket(client) }()
	var other win32.SOCKET
	switch upstream.Kind {
	case UpstreamApp:
		other = connectApp(upstream.Port)
	case UpstreamForward:
		other = askParent(upstream.Forward)
	}
	if other == win32.INVALID_SOCKET {
		return
	}
	defer func() { _, _ = win32.Closesocket(other) }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pump(other, client)
	}()
	pump(client, other)
	wg.Wait()
}

func acceptLoop(listener win32.SOCKET, upstream Upstream) {
	for {
		client, err := win32.Accept(listener, nil, nil)
		if err != nil || client == win32.INVALID_SOCKET {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go relay(client, upstream)
	}
}

func main() {
	if err := net.Startup(); err != nil {
		win32.ExitProcess(1)
	}
	listeners := 0
	var uses uint8 = 0
	args := os.Args[1:]
	var linkArgs []string
	if i := len(args) - 3; i >= 0 && args[i] == "--link-relay" {
		linkArgs = args[i+1:]
		args = args[:i]
	}

	for _, raw := range args {
		parsed, ok := parseArg(raw)
		if !ok {
			win32.ExitProcess(2)
		}
		switch parsed.Kind {
		case ArgService:
			go acceptLoop(parsed.Socket, Upstream{Kind: UpstreamApp, Port: parsed.Port})
			listeners++
		case ArgForward:
			z := parsed.Forward
			parent = &z
		case ArgUse:
			if parent == nil || uses == 255 {
				win32.ExitProcess(2)
			}
			families := []net.Family{net.FamilyIPv4, net.FamilyIPv6}
			for _, f := range families {
				res := net.Listen(net.Loopback(f, parsed.Use))
				if res.Ok() {
					go acceptLoop(res.Socket, Upstream{Kind: UpstreamForward, Forward: uses})
					listeners++
				}
			}
			uses++
		}
	}

	if listeners == 0 {
		if len(linkArgs) == 0 || runLinkRelay(linkArgs[0], linkArgs[1]) != nil {
			win32.ExitProcess(2)
		}
		return
	}

	if parent != nil {
		if !writeAll(parent.Out, []byte{0}) {
			win32.ExitProcess(1)
		}
	}
	if len(linkArgs) != 0 {
		go func() { _ = runLinkRelay(linkArgs[0], linkArgs[1]) }()
	}

	// Wait indefinitely
	select {}
}
