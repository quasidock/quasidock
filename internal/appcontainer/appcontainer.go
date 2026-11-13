package appcontainer

import (
	"fmt"

	"github.com/quasidock/quasidock/internal/win32"
)

const (
	mutexProfiles   = "Local\\quasidock-appcontainer"
	mutexExemptions = "Local\\quasidock-loopback"

	CapabilityInternetClient            = "S-1-15-3-1"
	CapabilityPrivateNetworkClientServer = "S-1-15-3-3"
)

type Profile struct {
	Name       string
	Sid        win32.PSID
	Capability win32.PSID
}

func acquireMutex(name string) (win32.HANDLE, error) {
	h, err := win32.CreateMutex(name)
	if err != nil {
		return 0, fmt.Errorf("CreateMutex %s: %w", name, err)
	}
	wait := win32.WaitForSingleObject(h, win32.INFINITE)
	switch wait {
	case win32.WAIT_OBJECT_0, win32.WAIT_ABANDONED:
		return h, nil
	default:
		win32.CloseHandle(h)
		return 0, fmt.Errorf("WaitForSingleObject on %s failed: %d", name, wait)
	}
}

func releaseMutex(h win32.HANDLE) {
	win32.ReleaseMutex(h)
	win32.CloseHandle(h)
}

func ProfileName(id string) string {
	return "quasidock." + id
}

// Ensure creates the app's profile, or opens it if it already exists.
func Ensure(id string) (Profile, error) {
	name := ProfileName(id)
	m, err := acquireMutex(mutexProfiles)
	if err != nil {
		return Profile{}, err
	}
	defer releaseMutex(m)

	sid, hr := win32.CreateAppContainerProfile(name, name, name, nil)
	if hr < 0 && hr != win32.E_ALREADY_EXISTS {
		win32.Sleep(100)
		sid, hr = win32.CreateAppContainerProfile(name, name, name, nil)
	}
	if hr == win32.E_ALREADY_EXISTS {
		sid, hr = win32.DeriveAppContainerSidFromAppContainerName(name)
	}
	if hr < 0 {
		return Profile{}, fmt.Errorf("AppContainer profile %s: HRESULT 0x%08x", name, uint32(hr))
	}

	capSid, err := capabilitySid(name)
	if err != nil {
		return Profile{}, err
	}

	return Profile{
		Name:       name,
		Sid:        sid,
		Capability: capSid,
	}, nil
}

// PackageSid returns the profile's package SID from its name.
func PackageSid(id string) (win32.PSID, error) {
	name := ProfileName(id)
	sid, hr := win32.DeriveAppContainerSidFromAppContainerName(name)
	if hr < 0 {
		return 0, fmt.Errorf("AppContainer SID of %s: HRESULT 0x%08x", name, uint32(hr))
	}
	return sid, nil
}

// DeleteProfile deletes the AppContainer profile.
func DeleteProfile(id string) error {
	name := ProfileName(id)
	hr := win32.DeleteAppContainerProfile(name)
	if hr < 0 {
		return fmt.Errorf("deleting AppContainer profile for %s: HRESULT 0x%08x", id, uint32(hr))
	}
	return nil
}

// AppCapability returns the capability SID derived from the app's name.
func AppCapability(id string) (win32.PSID, error) {
	return capabilitySid(ProfileName(id))
}

func capabilitySid(name string) (win32.PSID, error) {
	caps, _, err := win32.DeriveCapabilitySidsFromName(name)
	if err != nil {
		return 0, fmt.Errorf("DeriveCapabilitySidsFromName: %w", err)
	}
	if len(caps) == 0 {
		return 0, fmt.Errorf("Windows derived no capability SID from %s", name)
	}
	return caps[0], nil
}

// SidFromString converts a string SID into a PSID.
func SidFromString(s string) (win32.PSID, error) {
	sid, err := win32.ConvertStringSidToSid(s)
	if err != nil {
		return 0, fmt.Errorf("ConvertStringSidToSid %s: %w", s, err)
	}
	return sid, nil
}

// Loopback isolation management
type ExemptStatus int

const (
	ExemptAdded ExemptStatus = iota
	ExemptAlready
	ExemptRefused
)

type ExemptResult struct {
	Status  ExemptStatus
	Refused win32.DWORD
}

// Exempt puts the container sid on the loopback isolation exemption list.
func Exempt(sid win32.PSID) (ExemptResult, error) {
	m, err := acquireMutex(mutexExemptions)
	if err != nil {
		return ExemptResult{}, err
	}
	defer releaseMutex(m)

	list, err := win32.NetworkIsolationGetAppContainerConfig()
	if err != nil {
		return ExemptResult{Status: ExemptRefused, Refused: win32.DWORD(win32.Errno(err))}, nil
	}

	for _, item := range list {
		if win32.EqualSid(item.Sid, sid) {
			return ExemptResult{Status: ExemptAlready}, nil
		}
	}

	next := append(list, win32.SID_AND_ATTRIBUTES{Sid: sid, Attributes: 0})
	if err := win32.NetworkIsolationSetAppContainerConfig(next); err != nil {
		return ExemptResult{Status: ExemptRefused, Refused: win32.DWORD(win32.Errno(err))}, nil
	}
	return ExemptResult{Status: ExemptAdded}, nil
}

// Unexempt removes the container sid from the loopback exemption list.
func Unexempt(sid win32.PSID) (bool, error) {
	m, err := acquireMutex(mutexExemptions)
	if err != nil {
		return false, err
	}
	defer releaseMutex(m)

	list, err := win32.NetworkIsolationGetAppContainerConfig()
	if err != nil {
		return false, fmt.Errorf("reading containers exempt from loopback isolation: %w", err)
	}

	at := -1
	for i, item := range list {
		if win32.EqualSid(item.Sid, sid) {
			at = i
			break
		}
	}
	if at == -1 {
		return false, nil
	}

	next := append(list[:at], list[at+1:]...)
	if err := win32.NetworkIsolationSetAppContainerConfig(next); err != nil {
		return false, fmt.Errorf("taking back loopback exemption of container: %w", err)
	}
	return true, nil
}

// IsExempt checks if sid is on the loopback exemption list.
func IsExempt(sid win32.PSID) *bool {
	list, err := win32.NetworkIsolationGetAppContainerConfig()
	if err != nil {
		return nil
	}
	for _, item := range list {
		if win32.EqualSid(item.Sid, sid) {
			res := true
			return &res
		}
	}
	res := false
	return &res
}
