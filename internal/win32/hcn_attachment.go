package win32

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	ErrHCNEndpointMissing  = errors.New("Windows network endpoint no longer exists")
	ErrHCNAddressReserved  = errors.New("Windows network address is reserved or in use")
	ErrHCNEndpointDetached = errors.New("Windows network endpoint is detached")
)

type hcnEndpointAPI struct {
	create, close, delete, modify *syscall.LazyProc
}

var getHCNEndpoints = sync.OnceValues(func() (*hcnEndpointAPI, error) {
	network, err := systemNetworkDLL("computenetwork.dll")
	if err != nil {
		return nil, err
	}
	compute, err := systemNetworkDLL("vmcompute.dll")
	if err != nil {
		return nil, err
	}
	api := &hcnEndpointAPI{
		create: network.NewProc("HcnCreateEndpoint"),
		close:  network.NewProc("HcnCloseEndpoint"),
		delete: network.NewProc("HcnDeleteEndpoint"),
		modify: compute.NewProc("HNSCall"),
	}
	return api, findNetworkProcs(api.create, api.close, api.delete, api.modify)
})

func systemNetworkDLL(name string) (*syscall.LazyDLL, error) {
	path, err := SystemProgram(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHCNUnavailable, err)
	}
	return syscall.NewLazyDLL(path), nil
}

func findNetworkProcs(procs ...*syscall.LazyProc) error {
	for _, proc := range procs {
		if err := proc.Find(); err != nil {
			return fmt.Errorf("%w: %w", ErrHCNUnavailable, err)
		}
	}
	return nil
}

// CreateHCNEndpoint creates an endpoint with one requested IPv4 address.
func CreateHCNEndpoint(networkID, owner string, address netip.Prefix) (string, error) {
	api, err := getHCNEndpoints()
	if err != nil {
		return "", err
	}
	networkAPI, err := getHCN()
	if err != nil {
		return "", err
	}
	networkGUID, err := parseHCNID(networkID)
	if err != nil {
		return "", err
	}
	type ipConfiguration struct {
		IPAddress    string
		PrefixLength int
	}
	raw, err := json.Marshal(struct {
		SchemaVersion      json.RawMessage
		HostComputeNetwork string
		Owner              string
		IPConfigurations   []ipConfiguration
	}{json.RawMessage(hcnSchemaVersion), formatHCNID(networkGUID), owner, []ipConfiguration{{address.Addr().String(), address.Bits()}}})
	if err != nil {
		return "", err
	}
	settings, err := syscall.UTF16PtrFromString(string(raw))
	if err != nil {
		return "", err
	}
	id, err := newHCNGUID()
	if err != nil {
		return "", err
	}
	var network HANDLE
	var result *uint16
	hr, _, _ := networkAPI.open.Call(uintptr(unsafe.Pointer(&networkGUID)), uintptr(unsafe.Pointer(&network)), uintptr(unsafe.Pointer(&result)))
	if network != 0 {
		defer networkAPI.close.Call(network)
	}
	if err := hcnResult(networkAPI.open.Name, hr, result); err != nil {
		return "", err
	}
	var endpoint HANDLE
	result = nil
	hr, _, _ = api.create.Call(network, uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(settings)), uintptr(unsafe.Pointer(&endpoint)), uintptr(unsafe.Pointer(&result)))
	if endpoint != 0 {
		defer api.close.Call(endpoint)
	}
	if err := hcnResult(api.create.Name, hr, result); err != nil {
		return "", err
	}
	return formatHCNID(id), nil
}

func DeleteHCNEndpoint(id string) error {
	api, err := getHCNEndpoints()
	if err != nil {
		return err
	}
	guid, err := parseHCNID(id)
	if err != nil {
		return err
	}
	var result *uint16
	hr, _, _ := api.delete.Call(uintptr(unsafe.Pointer(&guid)), uintptr(unsafe.Pointer(&result)))
	return hcnResult(api.delete.Name, hr, result)
}

// AttachHCNEndpoint calls HNS's host NIC operation for a dedicated compartment.
// HCN namespace resource updates alone do not materialize a host vNIC.
func AttachHCNEndpoint(compartment uint32, endpoint string, attach bool) error {
	if compartment <= 1 || compartment > 65535 {
		return fmt.Errorf("invalid private network compartment %d", compartment)
	}
	id, err := parseHCNID(endpoint)
	if err != nil {
		return err
	}
	api, err := getHCNEndpoints()
	if err != nil {
		return err
	}
	operation := "detach"
	if attach {
		operation = "attach"
	}
	method := UTF16Ptr("POST")
	path := UTF16Ptr("/endpoints/" + formatHCNID(id) + "/" + operation)
	settings := UTF16Ptr(fmt.Sprintf(`{"SystemType":"Host","CompartmentId":%d}`, compartment))
	var result *uint16
	hr, _, _ := api.modify.Call(uintptr(unsafe.Pointer(method)), uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(settings)), uintptr(unsafe.Pointer(&result)))
	if HRESULT(hr) < 0 {
		return hcnResult("HNS "+operation+" endpoint", hr, result)
	}
	var response struct {
		Success   bool
		Error     string
		ErrorCode uint32
	}
	if err := json.Unmarshal([]byte(takeHCNString(result)), &response); err != nil {
		return fmt.Errorf("decode HNS endpoint response: %w", err)
	}
	if !response.Success {
		return &hcnError{operation: "HNS " + operation + " endpoint", code: response.ErrorCode, detail: response.Error}
	}
	return nil
}

func newHCNGUID() (GUID, error) {
	var id GUID
	hr, _, _ := procCoCreateGuid.Call(uintptr(unsafe.Pointer(&id)))
	return id, hcnResult("CoCreateGuid", hr, nil)
}

type networkCompartmentAPI struct {
	create, open, close, delete, query, bind *syscall.LazyProc
}

var getNetworkCompartments = sync.OnceValues(func() (*networkCompartmentAPI, error) {
	dll, err := systemNetworkDLL("iphlpapi.dll")
	if err != nil {
		return nil, err
	}
	api := &networkCompartmentAPI{
		create: dll.NewProc("CreateCompartment"),
		open:   dll.NewProc("OpenCompartment"),
		close:  dll.NewProc("CloseCompartment"),
		delete: dll.NewProc("DeleteCompartment"),
		query:  dll.NewProc("ConvertCompartmentGuidToId"),
		bind:   dll.NewProc("SetJobCompartmentId"),
	}
	return api, findNetworkProcs(api.create, api.open, api.close, api.delete, api.query, api.bind)
})

// NetworkCompartment retains the OS handle for an isolated routing table.
type NetworkCompartment struct {
	ID     uint32
	guid   GUID
	handle HANDLE
}

func CreateNetworkCompartment() (*NetworkCompartment, error) {
	api, err := getNetworkCompartments()
	if err != nil {
		return nil, err
	}
	guid, err := newHCNGUID()
	if err != nil {
		return nil, err
	}
	// Layout consumed by IP Helper's CreateCompartment. Flag 1 provisions
	// a compartment with its own loopback interfaces.
	entry := struct {
		GUID        GUID
		ID          uint32
		Description [257]uint16
		Flags       uint32
	}{GUID: guid, Flags: 1}
	copy(entry.Description[:], syscall.StringToUTF16("Quasidock private network"))
	code, _, _ := api.create.Call(uintptr(unsafe.Pointer(&entry)))
	if code != 0 {
		return nil, fmt.Errorf("create network compartment: %w", syscall.Errno(code))
	}
	c := &NetworkCompartment{ID: entry.ID, guid: guid}
	if c.ID <= 1 {
		return nil, fmt.Errorf("unexpected private network compartment %d", c.ID)
	}
	status, _, _ := api.open.Call(uintptr(unsafe.Pointer(&guid)), uintptr(unsafe.Pointer(&c.handle)))
	if status != 0 || c.handle == 0 || c.ID > 65535 {
		return nil, errors.Join(fmt.Errorf("open private network compartment %d: status %#x", c.ID, uint32(status)), c.Close())
	}
	return c, nil
}

// Close releases the compartment after the job has been reset to the default
// compartment and its endpoints have been removed.
func (c *NetworkCompartment) Close() error {
	if c == nil || c.ID <= 1 {
		return nil
	}
	api, err := getNetworkCompartments()
	if err != nil {
		return err
	}
	deleteCode, _, _ := api.delete.Call(uintptr(c.ID))
	if c.handle != 0 {
		_, _, _ = api.close.Call(c.handle)
		c.handle = 0
	}
	deadline := time.Now().Add(time.Second)
	for {
		var id uint32
		code, _, _ := api.query.Call(uintptr(unsafe.Pointer(&c.guid)), uintptr(unsafe.Pointer(&id)))
		if code == uintptr(syscall.ERROR_FILE_NOT_FOUND) || code == 1168 {
			c.ID = 0
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("remove network compartment %d: delete status %#x, lookup status %#x", c.ID, deleteCode, code)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func SetJobNetworkCompartment(job HANDLE, compartment uint32) error {
	api, err := getNetworkCompartments()
	if err != nil {
		return err
	}
	r, _, _ := api.bind.Call(job, uintptr(compartment))
	if code := syscall.Errno(uint32(r)); code != 0 {
		return fmt.Errorf("set job network compartment: %w", code)
	}
	return nil
}
