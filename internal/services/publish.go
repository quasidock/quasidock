package services

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/quasidock/quasidock/internal/net"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

type PublishSpec struct {
	Target string
	store.PortBinding
}

func PortKey(port uint16) string { return strconv.Itoa(int(port)) + "/tcp" }

// ParseSelector accepts a port number or service name, optionally suffixed /tcp.
func ParseSelector(value string) (string, error) {
	target, protocol, hasProtocol := strings.Cut(value, "/")
	if hasProtocol && protocol != "tcp" {
		return "", fmt.Errorf("unsupported port protocol %q; only tcp is supported", protocol)
	}
	if target == "" || len(target) > 32 {
		return "", fmt.Errorf("invalid port or service %q", value)
	}
	digits := true
	for _, ch := range target {
		digits = digits && ch >= '0' && ch <= '9'
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-') {
			return "", fmt.Errorf("invalid port or service %q", value)
		}
	}
	if digits {
		n, err := strconv.ParseUint(target, 10, 16)
		if err != nil || n == 0 {
			return "", fmt.Errorf("invalid container port %q: expected 1–65535", target)
		}
		return strconv.Itoa(int(n)), nil
	}
	return strings.ToLower(target), nil
}

func ParsePublish(value string) (PublishSpec, error) {
	var out PublishSpec
	target := value
	if i := strings.LastIndexByte(value, ':'); i >= 0 {
		target = value[i+1:]
		prefix := value[:i]
		out.HostPort = prefix
		if j := strings.LastIndexByte(prefix, ':'); j >= 0 {
			out.HostIP, out.HostPort = prefix[:j], prefix[j+1:]
			if strings.HasPrefix(out.HostIP, "[") && strings.HasSuffix(out.HostIP, "]") {
				out.HostIP = out.HostIP[1 : len(out.HostIP)-1]
			} else if strings.Contains(out.HostIP, ":") {
				return out, fmt.Errorf("invalid --publish %q: bracket IPv6 addresses", value)
			}
			ip, err := netip.ParseAddr(out.HostIP)
			if err != nil || ip.Zone() != "" {
				return out, fmt.Errorf("invalid --publish host address %q: expected an unscoped IP literal", out.HostIP)
			}
			out.HostIP = ip.Unmap().String()
		}
	}
	if out.HostPort != "" {
		n, err := strconv.ParseUint(out.HostPort, 10, 16)
		if err != nil {
			return out, fmt.Errorf("invalid --publish host port %q: expected 0–65535", out.HostPort)
		}
		out.HostPort = ""
		if n != 0 {
			out.HostPort = strconv.Itoa(int(n))
		}
	}
	var err error
	out.Target, err = ParseSelector(target)
	return out, err
}

func NormalizePublishing(cfg oci.AppConfig, specs []PublishSpec, moved []string) (map[string][]store.PortBinding, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := map[string][]store.PortBinding{}
	for _, spec := range specs {
		n, _ := strconv.ParseUint(spec.Target, 10, 16)
		var service *oci.Service
		for i := range cfg.Permissions.Services {
			s := &cfg.Permissions.Services[i]
			if (n != 0 && s.Port == uint16(n)) || (n == 0 && strings.EqualFold(s.Name, spec.Target)) {
				service, n = s, uint64(s.Port)
				break
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("--publish: %s has no service %q", cfg.ID, spec.Target)
		}
		if service != nil {
			for _, m := range moved {
				name, _, ok := ParsePort(m)
				if ok && strings.EqualFold(name, service.Name) {
					return nil, fmt.Errorf("cannot combine --publish and --port for service %q", service.Name)
				}
			}
		} else if spec.HostPort == strconv.Itoa(int(n)) && overlapsBackend(spec.HostIP) {
			return nil, fmt.Errorf("--publish: host endpoint overlaps raw backend port %d; choose a different host port or declare a service using ${listen:SERVICE}", n)
		}
		key := PortKey(uint16(n))
		for _, prior := range out[key] {
			if prior == spec.PortBinding && spec.HostPort != "" {
				return nil, fmt.Errorf("duplicate --publish binding for %s", key)
			}
		}
		out[key] = append(out[key], spec.PortBinding)
	}
	return out, nil
}

func overlapsBackend(host string) bool {
	if host == "" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && (ip.IsUnspecified() || ip.IsLoopback())
}

// PreparePublish binds explicit listeners first, then legacy automatic services.
// Service public ports are updated before command/environment expansion.
func PreparePublish(cfg oci.AppConfig, ports []Ports, requests map[string][]store.PortBinding, command string, sandbox oci.Sandbox, note func(string, ...any), reserved ...uint16) (_ *Published, err error) {
	p := &Published{Ports: map[string][]store.PortBinding{}, Direct: map[string][]store.PortBinding{}}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	backends := map[string]uint16{}
	excluded := append([]uint16(nil), reserved...)
	for _, s := range ports {
		backends[PortKey(s.Service.Port)] = s.Listen
		excluded = append(excluded, s.Listen)
	}
	keys := make([]string, 0, len(requests))
	for key := range requests {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		target, parseErr := ParseSelector(strings.TrimSuffix(key, "/tcp"))
		n, numberErr := strconv.ParseUint(target, 10, 16)
		if parseErr != nil || numberErr != nil || key != PortKey(uint16(n)) || n == 0 {
			return nil, fmt.Errorf("invalid saved port binding %q", key)
		}
		if backends[key] == 0 {
			backends[key] = uint16(n)
			excluded = append(excluded, uint16(n))
		}
	}
	for _, key := range keys {
		for _, request := range requests[key] {
			endpoints, sockets, bindErr := bindPublication(request, excluded)
			if bindErr != nil {
				return nil, fmt.Errorf("publishing %s: %w", key, bindErr)
			}
			p.add(key, backends[key], endpoints, sockets)
		}
		for i := range ports {
			if PortKey(ports[i].Service.Port) == key && len(p.Ports[key]) > 0 {
				n, _ := strconv.ParseUint(p.Ports[key][0].HostPort, 10, 16)
				ports[i].Port = uint16(n)
			}
		}
	}
	for _, s := range ports {
		key := PortKey(s.Service.Port)
		if _, explicit := requests[key]; explicit || !Serves(cfg, s.Service, command) {
			continue
		}
		if sandbox != oci.SandboxAppContainer {
			bindings := []store.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(int(s.Port))}, {HostIP: "::1", HostPort: strconv.Itoa(int(s.Port))}}
			p.Ports[key], p.Direct[key] = bindings, bindings
			continue
		}
		endpoints, sockets, bindErr := bindPublication(store.PortBinding{HostPort: strconv.Itoa(int(s.Port))}, nil)
		if bindErr != nil {
			var listenErr *listenError
			if errors.As(bindErr, &listenErr) && (listenErr.code == int32(win32.WSAEADDRINUSE) || listenErr.code == int32(win32.WSAEACCES)) {
				note("note: port %d is taken, so %s isn't published this run", s.Port, s.Service.Name)
			} else {
				note("note: can't publish %s this run: %v", s.Service.Name, bindErr)
			}
			continue
		}
		p.add(key, s.Listen, endpoints, sockets)
	}
	return p, nil
}

func (p *Published) add(key string, backend uint16, bindings []store.PortBinding, sockets []win32.SOCKET) {
	p.Ports[key] = append(p.Ports[key], bindings...)
	p.Sockets = append(p.Sockets, sockets...)
	for _, socket := range sockets {
		p.Args = append(p.Args, fmt.Sprintf("%d=%d", socket, backend))
	}
}

type listenError struct {
	address netip.Addr
	port    uint16
	code    int32
}

func (e *listenError) Error() string {
	return fmt.Sprintf("cannot listen on %s: Winsock error %d", netip.AddrPortFrom(e.address, e.port), e.code)
}

func bindPublication(request store.PortBinding, excluded []uint16) ([]store.PortBinding, []win32.SOCKET, error) {
	if err := net.Startup(); err != nil {
		return nil, nil, err
	}
	var addresses []netip.Addr
	if request.HostIP == "" {
		addresses = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()}
	} else {
		ip, err := netip.ParseAddr(request.HostIP)
		if err != nil || ip.Zone() != "" {
			return nil, nil, fmt.Errorf("invalid host address %q", request.HostIP)
		}
		addresses = []netip.Addr{ip.Unmap()}
	}
	var requested uint64
	if request.HostPort != "" {
		var err error
		requested, err = strconv.ParseUint(request.HostPort, 10, 16)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid host port %q", request.HostPort)
		}
	}
	for attempt := 0; attempt < 20; attempt++ {
		port := uint16(requested)
		var sockets []win32.SOCKET
		var bindings []store.PortBinding
		var bindErr error
		for _, address := range addresses {
			res := net.Listen(net.IPAddress(address, port))
			if !res.Ok() {
				if request.HostIP == "" && address.Is6() && (res.Failed == int32(win32.WSAEAFNOSUPPORT) || res.Failed == int32(win32.WSAEADDRNOTAVAIL)) {
					continue
				}
				bindErr = &listenError{address, port, res.Failed}
				break
			}
			sockets = append(sockets, res.Socket)
			if port == 0 {
				var ok bool
				port, ok = net.LocalPort(res.Socket)
				if !ok || port == 0 {
					bindErr = fmt.Errorf("reading automatically allocated host port")
					break
				}
			}
			for _, backend := range excluded {
				if port == backend && overlapsBackend(address.String()) {
					bindErr = fmt.Errorf("host port %d overlaps a backend port; choose a different host port", port)
					break
				}
			}
			if bindErr != nil {
				break
			}
			bindings = append(bindings, store.PortBinding{HostIP: address.String(), HostPort: strconv.Itoa(int(port))})
		}
		if bindErr == nil {
			return bindings, sockets, nil
		}
		for _, socket := range sockets {
			_, _ = win32.Closesocket(socket)
		}
		if requested != 0 || attempt == 19 {
			return nil, nil, bindErr
		}
	}
	panic("unreachable")
}
