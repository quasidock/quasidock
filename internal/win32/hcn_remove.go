package win32

import (
	"encoding/json"
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

type hcnRemovalAPI struct {
	deleteNetwork, enumerateEndpoints *syscall.LazyProc
}

var getHCNRemoval = sync.OnceValues(func() (*hcnRemovalAPI, error) {
	dll, err := systemNetworkDLL("computenetwork.dll")
	if err != nil {
		return nil, err
	}
	api := &hcnRemovalAPI{
		deleteNetwork:      dll.NewProc("HcnDeleteNetwork"),
		enumerateEndpoints: dll.NewProc("HcnEnumerateEndpoints"),
	}
	return api, findNetworkProcs(api.deleteNetwork, api.enumerateEndpoints)
})

// DeleteHCNNetwork removes a network by GUID. The caller must hold the HCN
// mutation lock and check for references and endpoints before calling it.
func DeleteHCNNetwork(id string) error {
	guid, err := parseHCNID(id)
	if err != nil {
		return err
	}
	api, err := getHCNRemoval()
	if err != nil {
		return err
	}
	var result *uint16
	hr, _, _ := api.deleteNetwork.Call(uintptr(unsafe.Pointer(&guid)), uintptr(unsafe.Pointer(&result)))
	return hcnResult(api.deleteNetwork.Name, hr, result)
}

// HCNNetworkHasEndpoints reports whether Windows has any endpoint on the
// network, including endpoints that are currently detached.
func HCNNetworkHasEndpoints(id string) (bool, error) {
	id, err := CanonicalHCNNetworkID(id)
	if err != nil {
		return false, err
	}
	api, err := getHCNRemoval()
	if err != nil {
		return false, err
	}
	// HCN's endpoint filter uses the HNS field name VirtualNetwork.
	filter := `{"VirtualNetwork":"` + id + `"}`
	query, err := syscall.UTF16PtrFromString(`{"SchemaVersion":` + hcnSchemaVersion + `,"Filter":` + fmt.Sprintf("%q", filter) + `}`)
	if err != nil {
		return false, err
	}
	var output, result *uint16
	hr, _, _ := api.enumerateEndpoints.Call(uintptr(unsafe.Pointer(query)), uintptr(unsafe.Pointer(&output)), uintptr(unsafe.Pointer(&result)))
	raw := takeHCNString(output)
	if err := hcnResult(api.enumerateEndpoints.Name, hr, result); err != nil {
		return false, err
	}
	var endpoints []string
	if err := json.Unmarshal([]byte(raw), &endpoints); err != nil {
		return false, fmt.Errorf("decode HCN endpoint IDs: %w", err)
	}
	return len(endpoints) != 0, nil
}
