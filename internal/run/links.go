package run

import (
	"errors"
	"fmt"
	"net/netip"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/linkrelay"
	"github.com/quasidock/quasidock/internal/services"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

type linkRuntime struct {
	store                      *store.Store
	id, runID                  string
	job, relay                 win32.HANDLE
	control                    *rpc.Client
	reverse                    *os.File
	controlChild, reverseChild *os.File
	listener                   *win32.PipeListener
	backends                   map[uint16]uint16
	note                       func(string, ...any)
	routes                     []linkrelay.Endpoint
	ready                      chan struct{}
	done                       chan struct{}
	wg                         sync.WaitGroup
	mu                         sync.Mutex
	incoming                   map[*os.File]struct{}
}

func linkPipeName(id, runID string) string {
	return `\\.\pipe\quasidock-link-` + id + "-" + runID
}

func linkPipePair(name string) (parent, child *os.File, err error) {
	listener, err := win32.ListenPipe(name)
	if err != nil {
		return nil, nil, err
	}
	defer listener.Close()
	child, err = win32.DialPipeIdentification(name, time.Now().Add(linkrelay.Timeout))
	if err != nil {
		return nil, nil, err
	}
	parent, err = listener.Accept()
	if err != nil {
		_ = child.Close()
		return nil, nil, err
	}
	return parent, child, nil
}

func prepareContainerLinks(st *store.Store, rec *store.ContainerRecord, job win32.HANDLE, note func(string, ...any)) (*linkRuntime, error) {
	name := linkPipeName(rec.ID, rec.State.RunID)
	control, controlChild, err := linkPipePair(name + "-control")
	if err != nil {
		return nil, err
	}
	reverse, reverseChild, err := linkPipePair(name + "-requests")
	if err != nil {
		_ = control.Close()
		_ = controlChild.Close()
		return nil, err
	}
	return &linkRuntime{
		store: st, id: rec.ID, runID: rec.State.RunID, job: job, note: note,
		control: jsonrpc.NewClient(control), reverse: reverse, backends: make(map[uint16]uint16),
		controlChild: controlChild, reverseChild: reverseChild,
		ready: make(chan struct{}), done: make(chan struct{}), incoming: make(map[*os.File]struct{}),
	}, nil
}

func (r *linkRuntime) relayOptions() ([]win32.HANDLE, []string) {
	handles := []win32.HANDLE{win32.HANDLE(r.controlChild.Fd()), win32.HANDLE(r.reverseChild.Fd())}
	args := []string{"--link-relay", strconv.FormatUint(uint64(handles[0]), 10), strconv.FormatUint(uint64(handles[1]), 10)}
	return handles, args
}

func (r *linkRuntime) start(relay win32.HANDLE, rec *store.ContainerRecord, ports []services.Ports) error {
	r.relay = relay
	_ = r.controlChild.Close()
	_ = r.reverseChild.Close()
	server := rpc.NewServer()
	if err := server.RegisterName("Broker", (*linkDialer)(r)); err != nil {
		return err
	}
	r.wg.Go(func() { server.ServeCodec(jsonrpc.NewServerCodec(r.reverse)) })
	for _, port := range ports {
		r.backends[port.Service.Port] = port.Listen
	}
	var routes []linkrelay.Route
	for _, link := range rec.HostConfig.Links {
		for _, port := range link.TCPPorts {
			routes = append(routes, linkrelay.Route{Alias: link.Alias, Port: port})
		}
	}
	if err := linkrelay.Call(r.control, "Relay.Listen", routes, &r.routes); err != nil {
		return err
	}
	if len(r.backends) != 0 {
		var err error
		r.listener, err = win32.ListenPipe(linkPipeName(r.id, r.runID))
		if err != nil {
			return err
		}
		r.wg.Go(r.serve)
	}
	return nil
}

func (r *linkRuntime) close() {
	close(r.done)
	if r.listener != nil {
		_ = r.listener.Close()
	}
	_ = r.control.Close()
	_ = r.reverse.Close()
	_ = r.controlChild.Close()
	_ = r.reverseChild.Close()
	r.mu.Lock()
	for pipe := range r.incoming {
		_ = pipe.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *linkRuntime) duplicate(pipe *os.File) (uintptr, error) {
	var out syscall.Handle
	err := syscall.DuplicateHandle(syscall.Handle(win32.GetCurrentProcess()), syscall.Handle(pipe.Fd()),
		syscall.Handle(r.relay), &out, 0, false, syscall.DUPLICATE_SAME_ACCESS)
	return uintptr(out), err
}

// linkDialer speaks only to this run's relay over an inherited pipe.
type linkDialer linkRuntime

func (d *linkDialer) Dial(route linkrelay.Route, handle *uintptr) (dialErr error) {
	r := (*linkRuntime)(d)
	defer func() {
		if r.note != nil && dialErr != nil {
			r.note("link %s TCP port %d: %v", route.Alias, route.Port, dialErr)
		}
	}()
	deadline := time.Now().Add(linkrelay.Timeout)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-r.ready:
	case <-r.done:
		return os.ErrClosed
	case <-timer.C:
		return os.ErrDeadlineExceeded
	}
	_, target, err := r.store.LiveLink(r.id, r.runID, route.Alias, route.Port)
	if err != nil {
		return err
	}
	pipe, err := win32.DialPipeIdentification(linkPipeName(target.ID, target.State.RunID), deadline)
	if err != nil {
		return err
	}
	defer pipe.Close()
	pid, err := win32.PipeServerProcessID(win32.HANDLE(pipe.Fd()))
	if err != nil || pid != target.State.LinkSupervisorPID {
		return errors.New("linked target broker changed")
	}
	if err := pipe.SetDeadline(deadline); err != nil {
		return err
	}
	if err := linkrelay.WriteRequest(pipe, linkrelay.DialRequest{SourceID: r.id, RunID: r.runID, Route: route}); err != nil {
		return err
	}
	var status [linkrelay.MaxFrame]byte
	result, err := linkrelay.ReadFrame(pipe, status[:])
	if err != nil {
		return err
	}
	if len(result) != 0 {
		return errors.New(string(result))
	}
	if _, err := pipe.Write([]byte{linkrelay.StreamStart}); err != nil {
		return err
	}
	if err := pipe.SetDeadline(time.Time{}); err != nil {
		return err
	}
	*handle, err = r.duplicate(pipe)
	return err
}

func (r *linkRuntime) serve() {
	for {
		pipe, err := r.listener.Accept()
		if err != nil {
			return
		}
		r.mu.Lock()
		select {
		case <-r.done:
			r.mu.Unlock()
			_ = pipe.Close()
			return
		default:
		}
		if len(r.incoming) >= 64 {
			r.mu.Unlock()
			_ = pipe.Close()
			continue
		}
		r.incoming[pipe] = struct{}{}
		r.mu.Unlock()
		r.wg.Go(func() {
			defer func() {
				_ = pipe.Close()
				r.mu.Lock()
				delete(r.incoming, pipe)
				r.mu.Unlock()
			}()
			if err := r.serveConnection(pipe); err != nil {
				_ = linkrelay.WriteFrame(pipe, []byte(err.Error()))
			}
		})
	}
}

func (r *linkRuntime) authorize(pipe *os.File, request linkrelay.DialRequest) error {
	source, target, err := r.store.LiveLink(request.SourceID, request.RunID, request.Alias, request.Port)
	if err != nil {
		return err
	}
	pid, err := win32.PipeClientProcessID(win32.HANDLE(pipe.Fd()))
	if err != nil || pid != source.State.LinkSupervisorPID || r.id != target.ID || r.runID != target.State.RunID {
		return errors.New("link broker request does not match the active container runs")
	}
	return nil
}

func (r *linkRuntime) serveConnection(pipe *os.File) error {
	if err := pipe.SetDeadline(time.Now().Add(linkrelay.Timeout)); err != nil {
		return err
	}
	request, err := linkrelay.ReadRequest(pipe)
	if err != nil {
		return err
	}
	if err := r.authorize(pipe, request); err != nil {
		return err
	}
	service := r.backends[request.Port]
	if service == 0 {
		return errors.New("the linked target does not declare this TCP service")
	}
	connection, err := r.openService(service)
	if err != nil {
		return err
	}
	var ignored bool
	attached := false
	defer func() {
		if !attached {
			_ = linkrelay.Call(r.control, "Relay.Cancel", connection.ID, &ignored)
		}
	}()
	if err := r.authorize(pipe, request); err != nil {
		return err
	}
	handle, err := r.duplicate(pipe)
	if err != nil {
		return err
	}
	// The relay owns its duplicate from this point on. A transport failure
	// closes the control channel and the relay process releases its handles.
	if err := linkrelay.Call(r.control, "Relay.Attach", linkrelay.AttachRequest{ID: connection.ID, Handle: handle}, &ignored); err != nil {
		return err
	}
	attached = true
	// The target relay waits for the dialer to consume this acknowledgement
	// before writing stream frames to the same pipe.
	return linkrelay.WriteFrame(pipe, nil)
}

func (r *linkRuntime) openService(port uint16) (linkrelay.Socket, error) {
	for _, ip := range []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1}), netip.IPv6Loopback()} {
		var conn linkrelay.Socket
		dst := netip.AddrPortFrom(ip, port)
		if err := linkrelay.Call(r.control, "Relay.Open", linkrelay.OpenRequest{Remote: dst}, &conn); err != nil {
			continue
		}
		if r.ownsService(conn) {
			return conn, nil
		}
		var ignored bool
		_ = linkrelay.Call(r.control, "Relay.Cancel", conn.ID, &ignored)
	}
	return linkrelay.Socket{}, fmt.Errorf("no TCP listener owned by the linked target on service port %d", port)
}

func (r *linkRuntime) ownsService(conn linkrelay.Socket) bool {
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		pid, err := win32.TCPConnectionOwner(conn.Remote, conn.Local)
		if err != nil {
			return false
		}
		if pid != 0 {
			h, err := win32.OpenProcess(win32.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
			if err != nil {
				return false
			}
			defer win32.CloseHandle(h)
			var member win32.BOOL
			return win32.IsProcessInJob(h, r.job, &member) && member != 0
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func linkEnvironment(base []environment.Var, explicit []string, sourceName string, routes []linkrelay.Endpoint) []environment.Var {
	env := append([]environment.Var(nil), base...)
	keys := make(map[string]bool)
	for _, v := range base {
		keys[strings.ToUpper(v.Name)] = true
	}
	for _, setting := range explicit {
		key, _, _ := strings.Cut(setting, "=")
		keys[strings.ToUpper(key)] = true
	}
	add := func(key, value string) {
		if !keys[key] {
			env = append(env, environment.Var{Name: key, Value: value})
			keys[key] = true
		}
	}
	for _, route := range routes {
		prefix := store.LinkEnvPrefix(route.Alias)
		localPort := strconv.Itoa(int(route.Listen))
		address := "tcp://127.0.0.1:" + localPort
		add(prefix+"_NAME", sourceName+"/"+route.Alias)
		add(prefix+"_PORT", address)
		key := store.LinkPortEnvKey(route.Alias, route.Port)
		add(key, address)
		add(key+"_ADDR", "127.0.0.1")
		add(key+"_PORT", localPort)
		add(key+"_PROTO", "tcp")
	}
	return env
}

func RemoveContainerLink(ctx *context.Context, target string) error {
	source, alias, err := store.ParseLinkPath(target)
	if err != nil {
		return err
	}
	rec, op, err := lockTarget(ctx, source)
	if err != nil {
		return err
	}
	defer op.Release()
	if store.FindLink(rec.HostConfig.Links, alias) < 0 {
		return fmt.Errorf("container %s has no link %q", rec.BareName(), alias)
	}
	if rec.State.Running && ctx.Store.IsContainerRunning(rec.ID) {
		if rec.State.Paused {
			return fmt.Errorf("container %s is paused; unpause or stop it before removing a link", rec.BareName())
		}
		if rec.State.ControlProtocol < 4 {
			return errors.New("live link removal requires the current supervisor; stop the source and retry")
		}
		err := sendControlRequest(ctx, rec, controlRequest{Action: actionUnlink, LinkAlias: alias})
		if err == nil || ctx.Store.IsContainerRunning(rec.ID) {
			return err
		}
		// The source exited during the request. Apply any remaining change
		// directly while its operation lock still prevents a replacement run.
	}
	return saveRemovedLink(ctx.Store, rec.ID, alias)
}

func saveRemovedLink(st *store.Store, id, alias string) error {
	_, err := st.UpdateContainer(id, func(c *store.ContainerRecord) error {
		i := store.FindLink(c.HostConfig.Links, alias)
		if i >= 0 {
			c.HostConfig.Links = append(c.HostConfig.Links[:i], c.HostConfig.Links[i+1:]...)
		}
		return nil
	})
	return err
}

func (r *execRuntime) unlinkLocked(ctx *context.Context, rec *store.ContainerRecord, alias string) error {
	if r.paused {
		return errors.New("unpause or stop the source before removing a link")
	}
	if r.links == nil {
		return errors.New("source link relay is unavailable")
	}
	if err := saveRemovedLink(ctx.Store, rec.ID, alias); err != nil {
		return err
	}
	r.links.routes = slices.DeleteFunc(r.links.routes, func(route linkrelay.Endpoint) bool {
		return strings.EqualFold(route.Alias, alias)
	})
	r.env = linkEnvironment(r.baseEnv, r.explicitEnv, r.sourceName, r.links.routes)
	var ignored bool
	if err := linkrelay.Call(r.links.control, "Relay.Drop", alias, &ignored); err != nil {
		return fmt.Errorf("link %q was removed from the container record, but the relay did not confirm live cleanup: %w", alias, err)
	}
	return nil
}
