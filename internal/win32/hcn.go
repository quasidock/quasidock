package win32

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const hcnSchemaVersion = `{"Major":2,"Minor":0}`

var (
	ErrHCNUnavailable     = errors.New("host networking is unavailable")
	ErrHCNNetworkExists   = errors.New("a Windows network with this name already exists")
	ErrHCNNetworkNotFound = errors.New("Windows network no longer exists")

	getHCN = sync.OnceValues(loadHCN)
)

// HCNNetwork describes one live network managed by the Windows Host Network Service.
type HCNNetwork struct {
	ID     string
	Name   string
	Driver string
}

// HCNNetworkDetails includes the complete HCN properties of a live network.
type HCNNetworkDetails struct {
	HCNNetwork
	Properties map[string]any
}

type hcnAPI struct {
	create    *syscall.LazyProc
	enumerate *syscall.LazyProc
	open      *syscall.LazyProc
	query     *syscall.LazyProc
	close     *syscall.LazyProc
}

func loadHCN() (*hcnAPI, error) {
	path, err := SystemProgram("computenetwork.dll")
	if err != nil {
		return nil, fmt.Errorf("%w: locating ComputeNetwork.dll: %w", ErrHCNUnavailable, err)
	}
	dll := syscall.NewLazyDLL(path)
	api := &hcnAPI{
		create:    dll.NewProc("HcnCreateNetwork"),
		enumerate: dll.NewProc("HcnEnumerateNetworks"),
		open:      dll.NewProc("HcnOpenNetwork"),
		query:     dll.NewProc("HcnQueryNetworkProperties"),
		close:     dll.NewProc("HcnCloseNetwork"),
	}
	for _, proc := range []*syscall.LazyProc{api.create, api.enumerate, api.open, api.query, api.close} {
		if err := proc.Find(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrHCNUnavailable, err)
		}
	}
	return api, nil
}

// CreatePrivateHCNNetwork asks Windows to provision a private virtual network
// and returns its GUID.
func CreatePrivateHCNNetwork(name string) (string, error) {
	api, err := getHCN()
	if err != nil {
		return "", err
	}
	settings := struct {
		SchemaVersion json.RawMessage
		Type          string
		Name          string
	}{
		SchemaVersion: json.RawMessage(hcnSchemaVersion),
		Type:          "Private",
		Name:          name,
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	config, err := syscall.UTF16PtrFromString(string(raw))
	if err != nil {
		return "", err
	}

	var id GUID
	hr, _, _ := procCoCreateGuid.Call(uintptr(unsafe.Pointer(&id)))
	if err := hcnResult("CoCreateGuid", hr, nil); err != nil {
		return "", err
	}
	var handle HANDLE
	var result *uint16
	hr, _, _ = api.create.Call(
		uintptr(unsafe.Pointer(&id)),
		uintptr(unsafe.Pointer(config)),
		uintptr(unsafe.Pointer(&handle)),
		uintptr(unsafe.Pointer(&result)),
	)
	if handle != 0 {
		_, _, _ = api.close.Call(handle)
	}
	if err := hcnResult("HcnCreateNetwork", hr, result); err != nil {
		return "", err
	}
	return formatHCNID(id), nil
}

// ListHCNNetworks queries the host's current Windows virtual networks.
func ListHCNNetworks() ([]HCNNetwork, error) {
	api, err := getHCN()
	if err != nil {
		return nil, err
	}
	query, err := syscall.UTF16PtrFromString(`{"SchemaVersion":` + hcnSchemaVersion + `}`)
	if err != nil {
		return nil, err
	}
	var output, result *uint16
	hr, _, _ := api.enumerate.Call(
		uintptr(unsafe.Pointer(query)),
		uintptr(unsafe.Pointer(&output)),
		uintptr(unsafe.Pointer(&result)),
	)
	raw := takeHCNString(output)
	if err := hcnResult("HcnEnumerateNetworks", hr, result); err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("decode HCN network IDs: %w", err)
	}
	networks := make([]HCNNetwork, 0, len(ids))
	for _, id := range ids {
		guid, err := parseHCNID(id)
		if err != nil {
			return nil, err
		}
		raw, err := api.networkProperties(guid, query)
		if errors.Is(err, ErrHCNNetworkNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("query Windows network %s: %w", id, err)
		}
		network, err := decodeHCNNetwork(guid, raw)
		if err != nil {
			return nil, fmt.Errorf("query Windows network %s: %w", id, err)
		}
		networks = append(networks, network)
	}
	return networks, nil
}

// InspectHCNNetwork queries the detailed properties of a Windows network by GUID.
func InspectHCNNetwork(id string) (HCNNetworkDetails, error) {
	guid, err := parseHCNID(id)
	if err != nil {
		return HCNNetworkDetails{}, err
	}
	api, err := getHCN()
	if err != nil {
		return HCNNetworkDetails{}, err
	}
	// Flag 1 is HCN's Detailed query flag.
	query, err := syscall.UTF16PtrFromString(`{"SchemaVersion":` + hcnSchemaVersion + `,"Flags":1}`)
	if err != nil {
		return HCNNetworkDetails{}, err
	}
	raw, err := api.networkProperties(guid, query)
	if err != nil {
		return HCNNetworkDetails{}, err
	}
	return decodeHCNNetworkDetails(guid, raw)
}

func (api *hcnAPI) networkProperties(id GUID, query *uint16) (string, error) {
	var handle HANDLE
	var result *uint16
	hr, _, _ := api.open.Call(
		uintptr(unsafe.Pointer(&id)),
		uintptr(unsafe.Pointer(&handle)),
		uintptr(unsafe.Pointer(&result)),
	)
	if handle != 0 {
		defer api.close.Call(handle)
	}
	if err := hcnResult("HcnOpenNetwork", hr, result); err != nil {
		return "", err
	}
	var output *uint16
	result = nil
	hr, _, _ = api.query.Call(
		handle,
		uintptr(unsafe.Pointer(query)),
		uintptr(unsafe.Pointer(&output)),
		uintptr(unsafe.Pointer(&result)),
	)
	raw := takeHCNString(output)
	if err := hcnResult("HcnQueryNetworkProperties", hr, result); err != nil {
		return "", err
	}
	return raw, nil
}

func decodeHCNNetwork(id GUID, raw string) (HCNNetwork, error) {
	// Windows can omit Type when the mode is NAT, whose schema value is zero.
	properties := struct{ Name, Type string }{Type: "NAT"}
	if err := json.Unmarshal([]byte(raw), &properties); err != nil {
		return HCNNetwork{}, fmt.Errorf("decode HCN network properties: %w", err)
	}
	return HCNNetwork{ID: formatHCNID(id), Name: properties.Name, Driver: strings.ToLower(properties.Type)}, nil
}

func decodeHCNNetworkDetails(id GUID, raw string) (HCNNetworkDetails, error) {
	network, err := decodeHCNNetwork(id, raw)
	if err != nil {
		return HCNNetworkDetails{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var properties map[string]any
	if err := decoder.Decode(&properties); err != nil {
		return HCNNetworkDetails{}, fmt.Errorf("decode HCN network properties: %w", err)
	}
	if properties == nil {
		return HCNNetworkDetails{}, errors.New("decode HCN network properties: expected a JSON object")
	}
	return HCNNetworkDetails{HCNNetwork: network, Properties: properties}, nil
}

func takeHCNString(value *uint16) string {
	if value == nil {
		return ""
	}
	defer CoTaskMemFree(uintptr(unsafe.Pointer(value)))
	return UTF16PtrToString(value)
}

type hcnError struct {
	operation string
	code      uint32
	detail    string
}

func (e *hcnError) Error() string {
	message := fmt.Sprintf("%s failed (HRESULT 0x%08x)", e.operation, e.code)
	if e.detail != "" {
		message += ": " + e.detail
	}
	return message
}

func (e *hcnError) Unwrap() error {
	switch e.code {
	case 0x80070005: // E_ACCESSDENIED
		return syscall.ERROR_ACCESS_DENIED
	case 0x803b0010, 0x800700b7: // HCN_E_NETWORK_ALREADY_EXISTS, ERROR_ALREADY_EXISTS
		return ErrHCNNetworkExists
	case 0x803b0001, 0x80070490, 0x803b0031: // HCN_E_NETWORK_NOT_FOUND, ERROR_NOT_FOUND, HCN_E_OBJECT_USED_AFTER_UNLOAD
		return ErrHCNNetworkNotFound
	case 0x803b0020, 0x800706ba, 0x80070422, 0x80070426: // HCN manager stopped, RPC unavailable, service disabled or inactive
		return ErrHCNUnavailable
	default:
		return nil
	}
}

func hcnResult(operation string, hr uintptr, result *uint16) error {
	raw := takeHCNString(result)
	if HRESULT(hr) >= 0 {
		return nil
	}
	var record struct{ Error string }
	if err := json.Unmarshal([]byte(raw), &record); err == nil && record.Error != "" {
		raw = record.Error
	}
	return &hcnError{operation: operation, code: uint32(hr), detail: raw}
}

func parseHCNID(id string) (GUID, error) {
	value := id
	if len(value) == 38 && value[0] == '{' && value[37] == '}' {
		value = value[1:37]
	}
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return GUID{}, fmt.Errorf("invalid HCN network ID %q", id)
	}
	var raw [16]byte
	n, err := hex.Decode(raw[:], []byte(strings.ReplaceAll(value, "-", "")))
	if err != nil || n != len(raw) {
		return GUID{}, fmt.Errorf("invalid HCN network ID %q", id)
	}
	return GUID{
		Data1: binary.BigEndian.Uint32(raw[:4]),
		Data2: binary.BigEndian.Uint16(raw[4:6]),
		Data3: binary.BigEndian.Uint16(raw[6:8]),
		Data4: [8]byte(raw[8:]),
	}, nil
}

func formatHCNID(id GUID) string {
	return fmt.Sprintf("%08x-%04x-%04x-%x-%x", id.Data1, id.Data2, id.Data3, id.Data4[:2], id.Data4[2:])
}
