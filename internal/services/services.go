package services

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/quasidock/quasidock/internal/net"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func ParsePort(value string) (string, uint16, bool) {
	eq := strings.IndexByte(value, '=')
	if eq <= 0 {
		return "", 0, false
	}
	portNum, err := strconv.ParseUint(value[eq+1:], 10, 16)
	if err != nil || portNum == 0 {
		return "", 0, false
	}
	return value[:eq], uint16(portNum), true
}

type Ports struct {
	Service oci.Service
	Port    uint16
	Listen  uint16
}

func Resolve(st *store.Store, cfg oci.AppConfig, uses []oci.Use, moved []string, sandbox oci.Sandbox) ([]Ports, error) {
	services := cfg.Permissions.Services

	for _, m := range moved {
		pName, _, ok := ParsePort(m)
		if !ok {
			return nil, fmt.Errorf("--port expects <service>=<port>; got %q", m)
		}
		known := false
		for _, s := range services {
			if strings.EqualFold(s.Name, pName) {
				known = true
				break
			}
		}
		if !known {
			for _, u := range uses {
				if strings.EqualFold(u.Name, pName) {
					known = true
					break
				}
			}
		}
		if !known {
			suffix := ""
			if len(uses) > 0 {
				suffix = " or used port of that name"
			}
			return nil, fmt.Errorf("--port=%s: %s has no service %q%s%s", m, cfg.ID, pName, suffix, namesOf(services, uses))
		}
	}

	out := make([]Ports, len(services))
	for i, s := range services {
		port := s.Port
		for _, m := range moved {
			if pName, pPort, ok := ParsePort(m); ok {
				if strings.EqualFold(pName, s.Name) {
					port = pPort
				}
			}
		}
		out[i] = Ports{Service: s, Port: port, Listen: port}
	}

	if sandbox == oci.SandboxAppContainer && len(out) > 0 {
		internal, err := internalPorts(st, cfg.ID, out)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].Listen = internal[i]
		}
	}
	return out, nil
}

func namesOf(services []oci.Service, uses []oci.Use) string {
	if len(services)+len(uses) == 0 {
		return "; it has none"
	}
	var names []string
	for _, s := range services {
		names = append(names, s.Name)
	}
	for _, u := range uses {
		names = append(names, u.Name)
	}
	return "; it has " + oci.JoinAnd(names)
}

func UsedBy(cfg oci.AppConfig) []oci.Use {
	var list []oci.Use
	addUses(&list, cfg.Permissions.Uses)
	for _, r := range cfg.From {
		addUses(&list, r.Uses)
	}
	return list
}

func addUses(list *[]oci.Use, uses []oci.Use) {
	for _, u := range uses {
		already := false
		for _, ex := range *list {
			if strings.EqualFold(ex.Name, u.Name) {
				already = true
				break
			}
		}
		if !already {
			*list = append(*list, u)
		}
	}
}

type Target struct {
	Use     oci.Use
	Port    uint16
	Connect uint16
}

func Targets(uses []oci.Use, moved []string) []Target {
	out := make([]Target, len(uses))
	for i, u := range uses {
		port := u.Port
		for _, m := range moved {
			if pName, pPort, ok := ParsePort(m); ok {
				if strings.EqualFold(pName, u.Name) {
					port = pPort
				}
			}
		}
		out[i] = Target{Use: u, Port: port, Connect: port}
	}
	return out
}

func UsePlaceholders(list []Target) []oci.UsedPort {
	out := make([]oci.UsedPort, len(list))
	for i, t := range list {
		out[i] = oci.UsedPort{
			Name: t.Use.Name,
			Port: strconv.Itoa(int(t.Connect)),
		}
	}
	return out
}

func Placeholders(ports []Ports) []oci.ServicePorts {
	out := make([]oci.ServicePorts, len(ports))
	for i, p := range ports {
		out[i] = oci.ServicePorts{
			Name:   p.Service.Name,
			Listen: strconv.Itoa(int(p.Listen)),
			Port:   strconv.Itoa(int(p.Port)),
		}
	}
	return out
}

func Serves(cfg oci.AppConfig, s oci.Service, command string) bool {
	if command == "" || strings.EqualFold(command, cfg.ID) {
		if s.Command == "" {
			return len(cfg.Entrypoint) > 0
		}
		return len(cfg.Entrypoint) == 1 && commandMatches(cfg.Entrypoint[0], s.Command)
	}
	if s.Command != "" {
		return commandMatches(command, s.Command)
	}
	return len(cfg.Entrypoint) == 1 && commandMatches(command, cfg.Entrypoint[0])
}

func commandMatches(a, b string) bool {
	na := strings.ReplaceAll(a, "/", `\`)
	nb := strings.ReplaceAll(b, "/", `\`)
	if strings.EqualFold(na, nb) {
		return true
	}
	if !strings.Contains(na, `\`) || !strings.Contains(nb, `\`) {
		return strings.EqualFold(oci.CommandStem(na), oci.CommandStem(nb))
	}
	return false
}

func PortsPath(st *store.Store, id string) string {
	return filepath.Join(st.Root, "ports", id+".txt")
}

const (
	internalFirst = 20000
	internalCount = 29000
)

func internalPorts(st *store.Store, id string, ports []Ports) ([]uint16, error) {
	p := PortsPath(st, id)
	_ = os.MkdirAll(filepath.Dir(p), 0755)

	lock, err := store.OpenLock(p+".lock", true, true)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	textBytes, _ := os.ReadFile(p)
	kept := ParseKept(string(textBytes))

	out := make([]uint16, len(ports))
	picked := false

	for i, s := range ports {
		var foundPort uint16
		for _, k := range kept {
			if strings.EqualFold(k.Name, s.Service.Name) {
				foundPort = k.Port
				break
			}
		}

		if foundPort != 0 {
			out[i] = foundPort
		} else {
			var taken []uint16
			for _, k := range kept {
				taken = append(taken, k.Port)
			}
			for _, other := range ports {
				taken = append(taken, other.Port)
			}
			newPort, err := pickPort(taken)
			if err != nil {
				return nil, err
			}
			kept = append(kept, Kept{Name: s.Service.Name, Port: newPort})
			picked = true
			out[i] = newPort
		}
	}

	if picked {
		var lines strings.Builder
		for _, k := range kept {
			fmt.Fprintf(&lines, "%s %d\n", k.Name, k.Port)
		}
		tmp := st.TmpPath("ports")
		if err := os.WriteFile(tmp, []byte(lines.String()), 0644); err == nil {
			_ = store.RenameRetrying(tmp, p, 5000)
		}
	}
	return out, nil
}

type Kept struct {
	Name string
	Port uint16
}

func ParseKept(text string) []Kept {
	var kept []Kept
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		space := strings.IndexByte(line, ' ')
		if space == -1 {
			continue
		}
		port, err := strconv.ParseUint(line[space+1:], 10, 16)
		if err != nil || port == 0 {
			continue
		}
		kept = append(kept, Kept{Name: line[:space], Port: uint16(port)})
	}
	return kept
}

func pickPort(taken []uint16) (uint16, error) {
	if err := net.Startup(); err != nil {
		return 0, fmt.Errorf("Windows sockets not available: %w", err)
	}
	for tries := 0; tries < 200; tries++ {
		var b [4]byte
		_, _ = rand.Read(b[:])
		cand := uint16(internalFirst + int(binary.LittleEndian.Uint32(b[:])%internalCount))
		already := false
		for _, t := range taken {
			if t == cand {
				already = true
				break
			}
		}
		if already {
			continue
		}
		if !isPortFree(cand) {
			continue
		}
		return cand, nil
	}
	return 0, fmt.Errorf("found no free port for app services")
}

func isPortFree(port uint16) bool {
	for _, fam := range []net.Family{net.FamilyIPv4, net.FamilyIPv6} {
		res := net.Listen(net.Loopback(fam, port))
		if res.Ok() {
			_, _ = win32.Closesocket(res.Socket)
		} else {
			if res.Failed == int32(win32.WSAEADDRINUSE) || res.Failed == int32(win32.WSAEACCES) {
				return false
			}
		}
	}
	return true
}

func ForwardThrough(st *store.Store, list []Target, ports []Ports) error {
	var taken []uint16
	for _, p := range ports {
		taken = append(taken, p.Port, p.Listen)
	}
	for _, t := range list {
		taken = append(taken, t.Port)
	}
	for i := range list {
		port, err := pickPort(taken)
		if err != nil {
			return err
		}
		list[i].Connect = port
		taken = append(taken, port)
	}
	return nil
}

type Published struct {
	Sockets []win32.SOCKET
	Args    []string
}

func (p *Published) Close() {
	if p == nil {
		return
	}
	for _, s := range p.Sockets {
		_, _ = win32.Closesocket(s)
	}
}

func Publish(cfg oci.AppConfig, ports []Ports, command string) (*Published, error) {
	var sockets []win32.SOCKET
	var args []string

	for _, p := range ports {
		if !Serves(cfg, p.Service, command) {
			continue
		}
		if err := net.Startup(); err != nil {
			return nil, fmt.Errorf("Windows sockets not available: %w", err)
		}
		before := len(sockets)
		for _, fam := range []net.Family{net.FamilyIPv4, net.FamilyIPv6} {
			res := net.Listen(net.Loopback(fam, p.Port))
			if res.Ok() {
				sockets = append(sockets, res.Socket)
				args = append(args, fmt.Sprintf("%d=%d", res.Socket, p.Listen))
			} else {
				if fam == net.FamilyIPv6 && (res.Failed == int32(win32.WSAEAFNOSUPPORT) || res.Failed == int32(win32.WSAEADDRNOTAVAIL)) {
					continue
				}
				if res.Failed == int32(win32.WSAEADDRINUSE) || res.Failed == int32(win32.WSAEACCES) {
					fmt.Fprintf(os.Stderr, "note: port %d is taken, so %s isn't published this run\n", p.Port, p.Service.Name)
				} else {
					fmt.Fprintf(os.Stderr, "note: can't listen on port %d, so %s isn't published this run: Winsock error %d\n", p.Port, p.Service.Name, res.Failed)
				}
				for _, s := range sockets[before:] {
					_, _ = win32.Closesocket(s)
				}
				sockets = sockets[:before]
				args = args[:before]
				break
			}
		}
	}

	if len(sockets) == 0 {
		return nil, nil
	}
	return &Published{Sockets: sockets, Args: args}, nil
}

func RelayExe(st *store.Store) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	src := filepath.Join(filepath.Dir(self), "quasidock-relay.exe")
	bytes, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", src, err)
	}

	h := sha256.Sum256(bytes)
	name := fmt.Sprintf("quasidock-relay-%s.exe", hex.EncodeToString(h[:4]))
	dest := filepath.Join(st.Root, "relay", name)

	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	tmp := st.TmpPath("relay")
	_ = os.MkdirAll(filepath.Dir(dest), 0755)
	if err := os.WriteFile(tmp, bytes, 0755); err != nil {
		return "", err
	}
	if err := store.RenameRetrying(tmp, dest, 5000); err != nil {
		_ = os.Remove(tmp)
		if _, statErr := os.Stat(dest); statErr == nil {
			return dest, nil
		}
		return "", err
	}
	return dest, nil
}

func Prune(st *store.Store, installed []string) error {
	self, _ := os.Executable()
	var currentName string
	if self != "" {
		src := filepath.Join(filepath.Dir(self), "quasidock-relay.exe")
		if b, err := os.ReadFile(src); err == nil {
			h := sha256.Sum256(b)
			currentName = fmt.Sprintf("quasidock-relay-%s.exe", hex.EncodeToString(h[:4]))
		}
	}

	relayDir := filepath.Join(st.Root, "relay")
	if entries, err := os.ReadDir(relayDir); err == nil {
		for _, e := range entries {
			if currentName != "" && e.Name() == currentName {
				continue
			}
			_ = os.Remove(filepath.Join(relayDir, e.Name()))
		}
	}

	portsDir := filepath.Join(st.Root, "ports")
	if entries, err := os.ReadDir(portsDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			id := ""
			if strings.HasSuffix(name, ".txt.lock") {
				id = name[:len(name)-len(".txt.lock")]
			} else if strings.HasSuffix(name, ".txt") {
				id = name[:len(name)-len(".txt")]
			} else {
				continue
			}
			found := false
			for _, inst := range installed {
				if strings.EqualFold(inst, id) {
					found = true
					break
				}
			}
			if !found {
				_ = os.Remove(filepath.Join(portsDir, name))
			}
		}
	}
	return nil
}

func Forget(st *store.Store, id string) error {
	err := os.Remove(PortsPath(st, id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
