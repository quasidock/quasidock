package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/rpc"
	"net/rpc/jsonrpc"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quasidock/quasidock/internal/linkrelay"
	"github.com/quasidock/quasidock/internal/win32"
)

type containerLinkRelay struct {
	broker   *rpc.Client
	mu       sync.Mutex
	routes   map[string][]*linkListener
	pending  map[uint64]*net.TCPConn
	incoming map[*linkrelay.Stream]struct{}
	nextID   uint64
	slots    chan struct{}
}

type linkListener struct {
	listener net.Listener
	broker   *rpc.Client
	route    linkrelay.Route
	slots    chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	closed   bool
	streams  map[*linkrelay.Stream]struct{}
}

func runLinkRelay(controlHandle, brokerHandle string) error {
	control, err := relayPipe(controlHandle, "link relay control")
	if err != nil {
		return err
	}
	defer control.Close()
	broker, err := relayPipe(brokerHandle, "link relay broker")
	if err != nil {
		return err
	}
	client := jsonrpc.NewClient(broker)
	defer client.Close()
	relay := &containerLinkRelay{
		broker: client, routes: make(map[string][]*linkListener),
		pending: make(map[uint64]*net.TCPConn), slots: make(chan struct{}, 64),
		incoming: make(map[*linkrelay.Stream]struct{}),
	}
	server := rpc.NewServer()
	if err := server.RegisterName("Relay", relay); err != nil {
		return err
	}
	server.ServeCodec(jsonrpc.NewServerCodec(control))
	_ = client.Close()
	relay.close()
	return nil
}

func (r *containerLinkRelay) close() {
	r.mu.Lock()
	for _, socket := range r.pending {
		_ = socket.Close()
	}
	for stream := range r.incoming {
		stream.Shutdown()
	}
	r.mu.Unlock()
	for _, listeners := range r.routes {
		closeLinkListeners(listeners)
	}
}

func relayPipe(value, name string) (*win32.InheritedPipe, error) {
	handle, err := strconv.ParseUint(value, 10, strconv.IntSize)
	if err != nil || handle == 0 {
		return nil, fmt.Errorf("invalid %s handle", name)
	}
	return win32.AdoptPipe(win32.HANDLE(handle)), nil
}

func (r *containerLinkRelay) Listen(routes []linkrelay.Route, reply *[]linkrelay.Endpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.routes) != 0 {
		return errors.New("link listeners are already initialized")
	}
	var listeners []*linkListener
	for _, route := range routes {
		addr := net.ListenConfig{Control: exclusiveLinkSocket}
		bound, err := addr.Listen(context.Background(), "tcp4", "127.0.0.1:0")
		if err != nil {
			closeLinkListeners(listeners)
			return fmt.Errorf("binding link %s port %d: %w", route.Alias, route.Port, err)
		}
		listener := &linkListener{
			listener: bound, broker: r.broker, route: route, slots: r.slots,
			streams: make(map[*linkrelay.Stream]struct{}),
		}
		listeners = append(listeners, listener)
		*reply = append(*reply, linkrelay.Endpoint{Route: route, Listen: uint16(bound.Addr().(*net.TCPAddr).Port)})
	}
	for _, listener := range listeners {
		key := strings.ToLower(listener.route.Alias)
		r.routes[key] = append(r.routes[key], listener)
		listener.wg.Add(1)
		go listener.serve()
	}
	return nil
}

func exclusiveLinkSocket(_, _ string, conn syscall.RawConn) error {
	var optionErr error
	err := conn.Control(func(fd uintptr) {
		optionErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, int(win32.SO_EXCLUSIVEADDRUSE), 1)
	})
	return errors.Join(err, optionErr)
}

func closeLinkListeners(listeners []*linkListener) {
	for _, listener := range listeners {
		listener.mu.Lock()
		listener.closed = true
		_ = listener.listener.Close()
		for stream := range listener.streams {
			stream.Shutdown()
		}
		listener.mu.Unlock()
	}
	for _, listener := range listeners {
		listener.wg.Wait()
	}
}

func (l *linkListener) serve() {
	defer l.wg.Done()
	for {
		client, err := l.listener.Accept()
		if err != nil {
			return
		}
		select {
		case l.slots <- struct{}{}:
			l.wg.Add(1)
			go l.forward(client.(*net.TCPConn))
		default:
			_ = client.Close()
		}
	}
}

func (l *linkListener) forward(socket *net.TCPConn) {
	defer l.wg.Done()
	defer func() { <-l.slots }()
	defer socket.Close()
	var handle uintptr
	if err := linkrelay.Call(l.broker, "Broker.Dial", l.route, &handle); err != nil || handle == 0 {
		return
	}
	pipe := win32.AdoptPipe(win32.HANDLE(handle))
	defer pipe.Close()
	stream := &linkrelay.Stream{TCP: socket, Pipe: pipe}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.streams[stream] = struct{}{}
	l.mu.Unlock()
	stream.Run()
	l.mu.Lock()
	delete(l.streams, stream)
	l.mu.Unlock()
}

func (r *containerLinkRelay) Drop(alias string, _ *bool) error {
	r.mu.Lock()
	key := strings.ToLower(alias)
	listeners := r.routes[key]
	delete(r.routes, key)
	r.mu.Unlock()
	closeLinkListeners(listeners)
	return nil
}

func (r *containerLinkRelay) Open(request linkrelay.OpenRequest, reply *linkrelay.Socket) error {
	if !request.Remote.Addr().IsLoopback() || request.Remote.Port() == 0 {
		return errors.New("invalid declared service endpoint")
	}
	select {
	case r.slots <- struct{}{}:
	default:
		return errors.New("too many active link connections")
	}
	socket, err := net.DialTimeout("tcp", request.Remote.String(), 400*time.Millisecond)
	if err != nil {
		<-r.slots
		return fmt.Errorf("connecting to declared service: %w", err)
	}
	r.mu.Lock()
	r.nextID++
	id := r.nextID
	r.pending[id] = socket.(*net.TCPConn)
	r.mu.Unlock()
	addr := socket.LocalAddr().(*net.TCPAddr).AddrPort()
	*reply = linkrelay.Socket{ID: id, Local: netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port()), Remote: request.Remote}
	return nil
}

func (r *containerLinkRelay) takeSocket(id uint64) (*net.TCPConn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	socket, ok := r.pending[id]
	delete(r.pending, id)
	return socket, ok
}

func (r *containerLinkRelay) Attach(request linkrelay.AttachRequest, _ *bool) error {
	if request.Handle == 0 {
		return errors.New("missing link stream handle")
	}
	pipe := win32.AdoptPipe(win32.HANDLE(request.Handle))
	socket, ok := r.takeSocket(request.ID)
	if !ok {
		_ = pipe.Close()
		return errors.New("link stream expired")
	}
	stream := &linkrelay.Stream{TCP: socket, Pipe: pipe}
	r.mu.Lock()
	r.incoming[stream] = struct{}{}
	r.mu.Unlock()
	go func() {
		defer func() { <-r.slots }()
		defer func() {
			stream.Shutdown()
			r.mu.Lock()
			delete(r.incoming, stream)
			r.mu.Unlock()
		}()
		var start [1]byte
		if _, err := io.ReadFull(pipe, start[:]); err == nil && start[0] == linkrelay.StreamStart {
			stream.Run()
		}
	}()
	return nil
}

func (r *containerLinkRelay) Cancel(id uint64, _ *bool) error {
	if socket, ok := r.takeSocket(id); ok {
		_ = socket.Close()
		<-r.slots
	}
	return nil
}
