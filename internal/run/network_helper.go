package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

const (
	networkPipePrefix     = `\\.\pipe\quasidock-hcn-`
	networkRequestTimeout = 30 * time.Second
)

type networkRequest struct {
	Action string
	Job    win32.HANDLE `json:",omitempty"`
	Target string       `json:",omitempty"`
}

type networkReply struct {
	ID       string                  `json:",omitempty"`
	Endpoint store.NetworkAttachment `json:",omitzero"`
	Error    string                  `json:",omitempty"`
	Fatal    bool                    `json:",omitempty"`
}

// The supervisor is the only client. One connection spans its restart attempts.
type networkHelper struct {
	stream  *framedStream
	process win32.HANDLE
}

func startNetworkHelper() (*networkHelper, error) {
	if err := requireUnelevatedNetworkSupervisor(); err != nil {
		return nil, err
	}
	key, err := newRunID()
	if err != nil {
		return nil, err
	}
	listener, err := win32.ListenPipe(networkPipePrefix + key)
	if err != nil {
		return nil, err
	}
	defer listener.Close()
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	system, err := win32.SystemProgram("kernel32.dll")
	if err != nil {
		return nil, err
	}
	args := process.BuildCommandLine("network-helper", []string{strconv.Itoa(os.Getpid()), key})
	child, err := win32.StartElevated(exe, args, filepath.Dir(system))
	if err != nil {
		return nil, fmt.Errorf("authorizing the Windows network helper: %w", err)
	}
	childPID := win32.GetProcessId(child)
	if childPID == 0 {
		win32.CloseHandle(child)
		return nil, errors.New("Windows did not identify the elevated network helper")
	}
	result := make(chan *os.File)
	go func() {
		defer close(result)
		file, err := listener.Accept()
		if err != nil {
			return
		}
		pid, err := win32.PipeClientProcessID(file.Fd())
		if err != nil || pid != childPID {
			_ = file.Close()
			return
		}
		select {
		case result <- file:
		case <-time.After(time.Second):
			_ = file.Close()
		}
	}()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case file := <-result:
			if file != nil {
				return &networkHelper{stream: &framedStream{File: file}, process: child}, nil
			}
		case <-tick.C:
			if win32.WaitForSingleObject(child, 0) == win32.WAIT_TIMEOUT {
				continue
			}
		case <-timeout.C:
		}
		win32.CloseHandle(child)
		return nil, errors.New("Windows network helper could not connect as the container's Windows user")
	}
}

func requireUnelevatedNetworkSupervisor() error {
	token, err := win32.OpenProcessToken(win32.GetCurrentProcess(), win32.TOKEN_QUERY)
	if err != nil {
		return err
	}
	defer win32.CloseHandle(token)
	rid, err := win32.GetTokenIntegrityRid(token)
	if err != nil {
		return err
	}
	if rid >= win32.HighIntegrityRID {
		return errors.New("start network-managed containers from a regular PowerShell session so the application runs unelevated")
	}
	return nil
}

func (h *networkHelper) call(request networkRequest) (networkReply, error) {
	if h == nil || h.stream == nil {
		return networkReply{Fatal: true}, errors.New("Windows network helper is unavailable")
	}
	data, err := json.Marshal(request)
	if err != nil {
		return networkReply{}, err
	}
	if err := h.stream.SetDeadline(time.Now().Add(networkRequestTimeout)); err != nil {
		return h.transportFailure(err)
	}
	if err := h.stream.send(1, data); err != nil {
		return h.transportFailure(err)
	}
	kind, data, err := h.stream.receive()
	if err != nil {
		return h.transportFailure(err)
	}
	var reply networkReply
	if kind != 2 || json.Unmarshal(data, &reply) != nil {
		return h.transportFailure(errors.New("invalid Windows network helper reply"))
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

func (h *networkHelper) transportFailure(err error) (networkReply, error) {
	_ = h.stream.Close()
	h.stream = nil
	return networkReply{Fatal: true}, fmt.Errorf("communicating with the Windows network helper: %w", err)
}

func (h *networkHelper) Close() {
	if h.stream != nil {
		_ = h.stream.Close()
		h.stream = nil
	}
	if h.process != 0 {
		win32.WaitForSingleObject(h.process, 5000)
		win32.CloseHandle(h.process)
		h.process = 0
	}
}

// ServeNetworkHelper is an elevated subprocess entry point. The pipe peer is
// the supervisor that initiated consent. Only its job and this helper's HCN
// endpoints are accepted for mutation.
func ServeNetworkHelper(pid uint32, key string) (result error) {
	if pid == 0 || !validRunID(key) {
		return errors.New("invalid Windows network helper arguments")
	}
	file, err := win32.DialPipeIdentification(networkPipePrefix+key, time.Now().Add(5*time.Second))
	if err != nil {
		return err
	}
	defer file.Close()
	actual, err := win32.PipeServerProcessID(file.Fd())
	if err != nil || actual != pid {
		return errors.New("Windows network helper pipe has an unexpected owner")
	}
	parent, err := win32.OpenProcess(win32.PROCESS_QUERY_LIMITED_INFORMATION|win32.PROCESS_DUP_HANDLE|win32.SYNCHRONIZE, false, pid)
	if err != nil {
		return err
	}
	defer win32.CloseHandle(parent)
	if err := verifyNetworkSupervisor(parent); err != nil {
		return err
	}
	stream := &framedStream{File: file}
	backend := &networkDriver{owner: "quasidock-" + key, parent: parent}
	defer func() { result = errors.Join(result, backend.end()) }()
	for {
		kind, data, err := stream.receive()
		if errors.Is(err, io.EOF) || errors.Is(err, syscall.ERROR_BROKEN_PIPE) {
			return nil
		}
		if err != nil {
			return err
		}
		var request networkRequest
		if kind != 1 || len(data) > 8192 || json.Unmarshal(data, &request) != nil {
			return errors.New("invalid Windows network helper request")
		}
		reply, err := backend.apply(request)
		if err != nil {
			reply.Error = err.Error()
		}
		response, err := json.Marshal(reply)
		if err != nil {
			return err
		}
		if err := stream.send(2, response); err != nil {
			return err
		}
		if reply.Fatal {
			return errors.New(reply.Error)
		}
	}
}

func verifyNetworkSupervisor(parent win32.HANDLE) error {
	user := func(process win32.HANDLE) (string, win32.DWORD, error) {
		token, err := win32.OpenProcessToken(process, win32.TOKEN_QUERY)
		if err != nil {
			return "", 0, err
		}
		defer win32.CloseHandle(token)
		rid, err := win32.GetTokenIntegrityRid(token)
		if err != nil {
			return "", 0, err
		}
		sidBytes, sid, err := win32.GetTokenUserSid(token)
		if err != nil {
			return "", 0, err
		}
		value, err := win32.ConvertSidToStringSid(sid)
		runtime.KeepAlive(sidBytes)
		return value, rid, err
	}
	helperUser, helperIntegrity, err := user(win32.GetCurrentProcess())
	if err != nil {
		return err
	}
	parentUser, parentIntegrity, err := user(parent)
	if err != nil {
		return err
	}
	if helperUser != parentUser || helperIntegrity < win32.HighIntegrityRID || parentIntegrity >= win32.HighIntegrityRID || parentIntegrity <= win32.SECURITY_MANDATORY_LOW_RID {
		return errors.New("the network helper and regular supervisor must use the same Windows account")
	}
	return nil
}

type networkDriver struct {
	owner       string
	parent, job win32.HANDLE
	compartment *win32.NetworkCompartment
	active      map[string]networkDriverEndpoint
}

type networkDriverEndpoint struct {
	settings store.NetworkAttachment
	subnet   netip.Prefix
	detached bool
}

func (d *networkDriver) apply(request networkRequest) (networkReply, error) {
	switch request.Action {
	case "begin":
		return networkReply{}, d.begin(request.Job)
	case "end":
		err := d.end()
		return networkReply{Fatal: err != nil}, err
	}
	if d.compartment == nil || request.Job != 0 || request.Target == "" || len(request.Target) > 512 {
		return networkReply{}, errors.New("invalid network attachment request")
	}
	switch request.Action {
	case "connect":
		return d.connect(request.Target)
	case "disconnect":
		return d.disconnect(request.Target)
	case "restore":
		return networkReply{}, d.restore(request.Target)
	case "discard":
		return networkReply{}, d.discard(request.Target)
	default:
		return networkReply{}, errors.New("unsupported Windows network helper operation")
	}
}

func (d *networkDriver) begin(source win32.HANDLE) error {
	if d.job != 0 || source == 0 {
		return errors.New("invalid network job binding request")
	}
	job, err := win32.DuplicateProcessHandle(d.parent, source)
	if err != nil {
		return fmt.Errorf("opening supervisor's network job: %w", err)
	}
	stats, err := win32.QueryJobBasicAccounting(job)
	if err != nil || stats.ActiveProcesses != 0 {
		win32.CloseHandle(job)
		return errors.New("the network job must be empty before binding")
	}
	compartment, err := win32.CreateNetworkCompartment()
	if err != nil {
		win32.CloseHandle(job)
		return err
	}
	if err := win32.SetJobNetworkCompartment(job, compartment.ID); err != nil {
		win32.CloseHandle(job)
		return errors.Join(err, compartment.Close())
	}
	d.job, d.compartment = job, compartment
	d.active = make(map[string]networkDriverEndpoint)
	return nil
}

func (d *networkDriver) end() error {
	if d.job == 0 {
		return nil
	}
	// The extra elevated handle prevents KILL_ON_JOB_CLOSE until the helper
	// exits. End the job explicitly, including when the supervisor disappears.
	process.EndJob(d.job)
	accounting, resetErr := win32.QueryJobBasicAccounting(d.job)
	if resetErr == nil {
		if accounting.ActiveProcesses == 0 {
			resetErr = win32.SetJobNetworkCompartment(d.job, 1)
		} else {
			resetErr = errors.New("network job still has active processes after termination")
		}
	}
	win32.CloseHandle(d.job)
	d.job = 0
	var cleanupErr error
	for _, attachment := range d.active {
		endpoint := attachment.settings.EndpointID
		if !attachment.detached {
			cleanupErr = errors.Join(cleanupErr, d.detach(endpoint))
		}
		cleanupErr = errors.Join(cleanupErr, d.delete(endpoint))
	}
	compartmentErr := d.compartment.Close()
	d.compartment, d.active = nil, nil
	return errors.Join(resetErr, cleanupErr, compartmentErr)
}

func (d *networkDriver) connect(target string) (networkReply, error) {
	// Keep lock contention below the supervisor's request deadline.
	release, err := win32.LockHCNMutation(5_000)
	if err != nil {
		return networkReply{}, err
	}
	defer release()
	networks, err := win32.ListHCNNetworks()
	if err != nil {
		return networkReply{}, err
	}
	network, err := win32.ResolveHCNNetwork(target, networks)
	if err != nil {
		return networkReply{}, err
	}
	id := network.ID
	if attachment, exists := d.active[id]; exists {
		if !attachment.detached {
			return networkReply{}, fmt.Errorf("network %q is already connected", target)
		}
		if err := d.discard(id); err != nil {
			return networkReply{}, err
		}
	}
	subnet, err := win32.PrivateHCNSubnet(id)
	if err != nil {
		return networkReply{}, err
	}
	for _, attachment := range d.active {
		if !attachment.detached && subnet.Overlaps(attachment.subnet) {
			return networkReply{}, fmt.Errorf("network %q overlaps network %q", target, attachment.settings.Name)
		}
	}
	address := subnet.Addr().Next().Next() // Network address and first host are reserved by HCN.
	var endpoint string
	for subnet.Contains(address.Next()) {
		endpoint, err = win32.CreateHCNEndpoint(id, d.owner, netip.PrefixFrom(address, subnet.Bits()))
		if err == nil {
			break
		}
		if !errors.Is(err, win32.ErrHCNAddressReserved) {
			return networkReply{}, err
		}
		address = address.Next()
	}
	if endpoint == "" {
		return networkReply{}, fmt.Errorf("network %q has no free IPv4 address", target)
	}
	if err := win32.AttachHCNEndpoint(d.compartment.ID, endpoint, true); err != nil {
		cleanupErr := d.delete(endpoint)
		if cleanupErr != nil {
			d.active[id] = networkDriverEndpoint{settings: store.NetworkAttachment{EndpointID: endpoint}, detached: true}
		}
		return networkReply{Fatal: cleanupErr != nil}, errors.Join(err, cleanupErr)
	}
	attachment := store.NetworkAttachment{Name: network.Name, EndpointID: endpoint, IPAddress: address.String(), IPPrefixLen: subnet.Bits()}
	d.active[id] = networkDriverEndpoint{settings: attachment, subnet: subnet}
	return networkReply{ID: id, Endpoint: attachment}, nil
}

func (d *networkDriver) disconnect(id string) (networkReply, error) {
	attachment, exists := d.active[id]
	if !exists || attachment.detached {
		return networkReply{}, fmt.Errorf("network %s is not connected", id)
	}
	endpoint := attachment.settings.EndpointID
	if err := d.detach(endpoint); err != nil {
		return networkReply{}, err
	}
	attachment.detached = true
	d.active[id] = attachment
	return networkReply{ID: id, Endpoint: attachment.settings}, nil
}

func (d *networkDriver) restore(id string) error {
	attachment, exists := d.active[id]
	if !exists || !attachment.detached {
		return errors.New("the detached network endpoint is unavailable")
	}
	if err := win32.AttachHCNEndpoint(d.compartment.ID, attachment.settings.EndpointID, true); err != nil {
		return err
	}
	attachment.detached = false
	d.active[id] = attachment
	return nil
}

func (d *networkDriver) discard(id string) error {
	attachment, exists := d.active[id]
	if !exists || !attachment.detached {
		return errors.New("the detached network endpoint is unavailable")
	}
	if err := d.delete(attachment.settings.EndpointID); err != nil {
		return err
	}
	delete(d.active, id)
	return nil
}

func (d *networkDriver) detach(endpoint string) error {
	err := win32.AttachHCNEndpoint(d.compartment.ID, endpoint, false)
	if errors.Is(err, win32.ErrHCNEndpointDetached) || errors.Is(err, win32.ErrHCNEndpointMissing) || errors.Is(err, win32.ErrHCNNetworkNotFound) {
		return nil
	}
	return err
}

func (d *networkDriver) delete(endpoint string) error {
	err := win32.DeleteHCNEndpoint(endpoint)
	if errors.Is(err, win32.ErrHCNEndpointMissing) || errors.Is(err, win32.ErrHCNNetworkNotFound) {
		return nil
	}
	return err
}
