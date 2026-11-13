package net_test

import (
	"bytes"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/net"
	"github.com/zigsaw-io/zigsaw/internal/win32"
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
	if clientSock == win32.INVALID_SOCKET {
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
	if n <= 0 {
		t.Fatalf("Recv failed: ret=%d, err=%v", n, err)
	}
	if !bytes.Equal(buf[:n], msg) {
		t.Fatalf("received %q, want %q", string(buf[:n]), string(msg))
	}
}
