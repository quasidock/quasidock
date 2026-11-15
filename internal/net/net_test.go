package net_test

import (
	"bytes"
	"testing"

	"github.com/quasidock/quasidock/internal/net"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestAddressLoopback(t *testing.T) {
	a := net.Loopback(net.FamilyIPv4, 5432)
	if a.IPv4.Port != 0x3815 {
		t.Fatalf("expected 0x3815, got 0x%x", a.IPv4.Port)
	}
	expectedV4 := [4]byte{127, 0, 0, 1}
	if a.IPv4.Addr != expectedV4 {
		t.Fatalf("expected %v, got %v", expectedV4, a.IPv4.Addr)
	}

	b := net.Loopback(net.FamilyIPv6, 8471)
	if b.IPv6.Addr[15] != 1 || b.IPv6.Addr[0] != 0 {
		t.Fatalf("unexpected IPv6 loopback addr: %v", b.IPv6.Addr)
	}
	if b.Len() != 28 {
		t.Fatalf("expected len 28, got %d", b.Len())
	}
}

func TestListenConnect(t *testing.T) {
	if err := net.Startup(); err != nil {
		t.Fatalf("Startup failed: %v", err)
	}

	// Listen on ephemeral loopback port (port 0)
	lRes := net.Listen(net.Loopback(net.FamilyIPv4, 0))
	if !lRes.Ok() {
		t.Fatalf("Listen failed with code: %d", lRes.Failed)
	}
	defer func() { _, _ = win32.Closesocket(lRes.Socket) }()

	port, ok := net.LocalPort(lRes.Socket)
	if !ok || port == 0 {
		t.Fatalf("LocalPort failed, port = %d", port)
	}

	// Connect to listener
	cRes := net.Connect(net.Loopback(net.FamilyIPv4, port), 2000)
	if !cRes.Ok() {
		t.Fatalf("Connect failed with code: %d", cRes.Failed)
	}
	defer func() { _, _ = win32.Closesocket(cRes.Socket) }()

	// Accept connection
	clientSock, err := win32.Accept(lRes.Socket, nil, nil)
	if clientSock == win32.INVALID_SOCKET || err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	defer func() { _, _ = win32.Closesocket(clientSock) }()

	// Test SendAll and Recv
	msg := []byte("ping from client")
	if !net.SendAll(cRes.Socket, msg) {
		t.Fatalf("SendAll failed")
	}

	buf := make([]byte, 64)
	n, err := win32.Recv(clientSock, buf, 0)
	if n <= 0 || err != nil {
		t.Fatalf("Recv failed: ret=%d, err=%v", n, err)
	}
	if !bytes.Equal(buf[:n], msg) {
		t.Fatalf("received %q, want %q", string(buf[:n]), string(msg))
	}
	_, _ = win32.Shutdown(cRes.Socket, win32.SD_SEND)
	if n, err := win32.Recv(clientSock, buf, 0); n != 0 || err != nil {
		t.Fatalf("Recv at EOF = %d, %v; want 0, nil", n, err)
	}
}

func TestSocketErrors(t *testing.T) {
	if err := net.Startup(); err != nil {
		t.Fatal(err)
	}
	s, err := win32.WSASocket(win32.AF_INET, win32.SOCK_STREAM, win32.IPPROTO_TCP, nil, 0, win32.WSA_FLAG_OVERLAPPED)
	if s == win32.INVALID_SOCKET || err != nil {
		t.Fatalf("WSASocket = %d, %v", s, err)
	}
	_, _ = win32.Closesocket(s)
	if s, err := win32.WSASocket(-1, win32.SOCK_STREAM, win32.IPPROTO_TCP, nil, 0, 0); s != win32.INVALID_SOCKET || err == nil {
		t.Fatalf("invalid WSASocket = %d, %v", s, err)
	}
	if s, err := win32.Accept(win32.INVALID_SOCKET, nil, nil); s != win32.INVALID_SOCKET || err == nil {
		t.Fatalf("invalid Accept = %d, %v", s, err)
	}
	if n, err := win32.Recv(win32.INVALID_SOCKET, make([]byte, 1), 0); n != win32.SOCKET_ERROR || err == nil {
		t.Fatalf("invalid Recv = %d, %v", n, err)
	}
}
