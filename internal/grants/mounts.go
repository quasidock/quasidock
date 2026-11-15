package grants

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/ledger"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

// Mount permissions use the user's grant ledger, not a single store's counters.
// Owners are process-backed and survive crashes until the next repair/launch.
type mountOwner struct {
	AppID      string
	Lock       string
	Sandbox    oci.Sandbox
	Capability string
	ReadOnly   bool
}
type mountSource struct {
	Path        string
	Owners      []mountOwner
	Caps        map[string]win32.DWORD
	LowTracked  bool
	LowBaseline *uint32
}
type MountRequest struct {
	Path     string
	ReadOnly bool
}
type MountSession struct {
	store *store.Store
	lock  string
}

// MountGuard holds the same user-wide mutex as permission acquisition while a
// caller removes storage or grants. This closes the check/delete race.
type MountGuard struct {
	handle win32.HANDLE
	store  *store.Store
}

func LockMountSources(st *store.Store) (*MountGuard, error) {
	h, err := mountMutex()
	if err != nil {
		return nil, err
	}
	return &MountGuard{handle: h, store: st}, nil
}
func (g *MountGuard) Close()                              { releaseMountMutex(g.handle) }
func (g *MountGuard) Protected(path string) (bool, error) { return mountProtectedNoLock(g.store, path) }

func mountMutex() (win32.HANDLE, error) {
	runtime.LockOSThread()
	h, err := win32.CreateMutex(`Local\quasidock-mount-grants`)
	if err != nil {
		runtime.UnlockOSThread()
		return 0, err
	}
	r := win32.WaitForSingleObject(h, win32.INFINITE)
	if r != win32.WAIT_OBJECT_0 && r != win32.WAIT_ABANDONED {
		win32.CloseHandle(h)
		runtime.UnlockOSThread()
		return 0, errors.New("waiting for mount grants mutex")
	}
	return h, nil
}
func releaseMountMutex(h win32.HANDLE) {
	_ = win32.ReleaseMutex(h)
	win32.CloseHandle(h)
	runtime.UnlockOSThread()
}

func readMountSources(st *store.Store) ([]mountSource, error) {
	k, err := regkey.Open(win32.HKEY_CURRENT_USER, st.LedgerKey+`\Mounts`, win32.KEY_READ)
	if err != nil || k == nil {
		return nil, err
	}
	defer k.Close()
	v, err := k.GetString("Sources")
	if err != nil || v == nil {
		return nil, err
	}
	var sources []mountSource
	if err := json.Unmarshal([]byte(*v), &sources); err != nil {
		return nil, fmt.Errorf("reading mount permission ledger: %w", err)
	}
	return sources, nil
}
func writeMountSources(st *store.Store, sources []mountSource) error {
	if len(sources) == 0 {
		k, err := regkey.Open(win32.HKEY_CURRENT_USER, st.LedgerKey+`\Mounts`, win32.KEY_READ|win32.KEY_WRITE)
		if err != nil || k == nil {
			return err
		}
		err = k.DeleteValue("Sources")
		k.Close()
		if err != nil {
			return err
		}
		return ledger.DropIfEmpty(st.LedgerKey + `\Mounts`)
	}
	b, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	k, err := regkey.Create(win32.HKEY_CURRENT_USER, st.LedgerKey+`\Mounts`)
	if err != nil {
		return err
	}
	defer k.Close()
	name := "Sources"
	return k.SetString(&name, string(b))
}

func overlap(a, b string) bool { return mounts.Within(a, b) || mounts.Within(b, a) }
func liveMountOwners(sources []mountSource, drop string) {
	for i := range sources {
		kept := sources[i].Owners[:0]
		for _, o := range sources[i].Owners {
			if !strings.EqualFold(o.Lock, drop) && store.IsLocked(o.Lock) {
				kept = append(kept, o)
			}
		}
		sources[i].Owners = kept
	}
}
func needsPermission(sources []mountSource, path, capability string, low bool) bool {
	for _, s := range sources {
		if overlap(s.Path, path) {
			for _, o := range s.Owners {
				if low && o.Sandbox == oci.SandboxLow && !o.ReadOnly {
					return true
				}
				if !low && capability != "" && o.Capability == capability {
					return true
				}
			}
		}
	}
	return false
}

// settleMountSources is idempotent, including after a permission change succeeds
// but the process dies before its ledger update. Failed restoration stays recorded.
func settleMountSources(st *store.Store, sources []mountSource) error {
	var errs []error
	kept := []mountSource{}
	for i := range sources {
		s := &sources[i]
		if _, err := os.Stat(s.Path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		for cap, mask := range s.Caps {
			if needsPermission(sources, s.Path, cap, false) {
				continue
			}
			sid, err := win32.ConvertStringSidToSid(cap)
			if err == nil {
				err = acl.RestoreGrant(s.Path, sid, mask)
				win32.LocalFree(sid)
			}
			if err != nil {
				errs = append(errs, err)
			} else {
				delete(s.Caps, cap)
			}
		}
		if s.LowTracked && !needsPermission(sources, s.Path, "", true) {
			if err := acl.RestoreLabel(s.Path, s.LowBaseline); err != nil {
				errs = append(errs, err)
			} else {
				s.LowTracked = false
				s.LowBaseline = nil
			}
		}
		if len(s.Owners) > 0 || len(s.Caps) > 0 || s.LowTracked {
			kept = append(kept, *s)
		}
	}
	if err := writeMountSources(st, kept); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func BeginMounts(st *store.Store, c *store.ContainerRecord, sandbox oci.Sandbox, requests []MountRequest) (*MountSession, error) {
	if len(requests) == 0 {
		return nil, RepairMounts(st)
	}
	h, err := mountMutex()
	if err != nil {
		return nil, err
	}
	defer releaseMountMutex(h)
	sources, err := readMountSources(st)
	if err != nil {
		return nil, err
	}
	liveMountOwners(sources, "")
	lock, err := mounts.CanonicalPath(st.ContainerRunLockPath(c.ID))
	if err != nil {
		return nil, err
	}
	cap := ""
	if sandbox == oci.SandboxAppContainer {
		sid, err := appcontainer.AppCapability(c.AppID)
		if err != nil {
			return nil, err
		}
		defer win32.LocalFree(sid)
		cap, err = win32.ConvertSidToStringSid(sid)
		if err != nil {
			return nil, err
		}
	}
	for _, req := range requests {
		p, err := mounts.CanonicalPath(req.Path)
		if err != nil {
			return nil, err
		}
		idx := -1
		for i := range sources {
			if SamePath(sources[i].Path, p) {
				idx = i
				break
			}
		}
		if idx < 0 {
			sources = append(sources, mountSource{Path: p, Caps: map[string]win32.DWORD{}})
			idx = len(sources) - 1
		}
		s := &sources[idx]
		if s.Caps == nil {
			s.Caps = map[string]win32.DWORD{}
		}
		if cap != "" {
			if _, ok := s.Caps[cap]; !ok {
				sid, _ := win32.ConvertStringSidToSid(cap)
				mask, e := acl.ExplicitGrant(p, sid)
				win32.LocalFree(sid)
				if e != nil {
					return nil, e
				}
				s.Caps[cap] = mask
			}
		}
		if sandbox == oci.SandboxLow && !req.ReadOnly && !s.LowTracked {
			baseline, e := acl.OwnLabel(p)
			if e != nil {
				return nil, e
			}
			s.LowBaseline = baseline
			s.LowTracked = true
		}
		s.Owners = append(s.Owners, mountOwner{AppID: c.AppID, Lock: lock, Sandbox: sandbox, Capability: cap, ReadOnly: req.ReadOnly})
	}
	// Publish restoration information before changing a filesystem ACL.
	if err := writeMountSources(st, sources); err != nil {
		return nil, err
	}
	session := &MountSession{store: st, lock: lock}
	for _, req := range requests {
		if cap != "" {
			sid, _ := win32.ConvertStringSidToSid(cap)
			access := acl.AccessFull
			if req.ReadOnly {
				access = acl.AccessReadExecute
			}
			_, err = acl.Grant(req.Path, sid, access, acl.Revoke{Kind: acl.RevokeNone})
			win32.LocalFree(sid)
		} else if sandbox == oci.SandboxLow && !req.ReadOnly {
			_, err = acl.LabelLow(req.Path)
		}
		if err != nil {
			liveMountOwners(sources, lock)
			_ = settleMountSources(st, sources)
			return nil, fmt.Errorf("granting mount access to %s: %w", req.Path, err)
		}
	}
	if err := settleMountSources(st, sources); err != nil {
		return session, err
	}
	return session, nil
}

func (s *MountSession) Close() error {
	if s == nil {
		return nil
	}
	h, err := mountMutex()
	if err != nil {
		return err
	}
	defer releaseMountMutex(h)
	sources, err := readMountSources(s.store)
	if err != nil {
		return err
	}
	liveMountOwners(sources, s.lock)
	return settleMountSources(s.store, sources)
}

func RepairMounts(st *store.Store) error {
	h, err := mountMutex()
	if err != nil {
		return err
	}
	defer releaseMountMutex(h)
	sources, err := readMountSources(st)
	if err != nil {
		return err
	}
	liveMountOwners(sources, "")
	return settleMountSources(st, sources)
}

func mountProtectedNoLock(st *store.Store, path string) (bool, error) {
	sources, err := readMountSources(st)
	if err != nil {
		return true, err
	}
	p, err := mounts.CanonicalPath(path)
	if err != nil {
		return false, err
	}
	for _, s := range sources {
		if overlap(p, s.Path) {
			for _, o := range s.Owners {
				if store.IsLocked(o.Lock) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
