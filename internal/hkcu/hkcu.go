package hkcu

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/acl"
	"github.com/zigsaw-io/zigsaw/internal/appcontainer"
	"github.com/zigsaw-io/zigsaw/internal/regkey"
	"github.com/zigsaw-io/zigsaw/internal/store"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

const (
	hkcuRoot   = win32.HKEY_CURRENT_USER
	recordsKey = `Software\zigsaw\keys`
	asideKey   = `Software\zigsaw\aside`
	mutexName  = `Local\zigsaw-hkcu`
)

type AccessType int

const (
	AccessNone AccessType = iota
	AccessLow
	AccessCapability
)

type Access struct {
	Type AccessType
	SID  win32.PSID
}

type Session struct {
	ID       string
	Keys     []string
	DataDir  string
	LockPath string
	Lock     *store.Lock
	Keep     bool
}

type RecordState int

const (
	StateMoving RecordState = iota
	StateInPlace
	StateTakenBack
)

func (s RecordState) String() string {
	switch s {
	case StateMoving:
		return "moving"
	case StateInPlace:
		return "in place"
	case StateTakenBack:
		return "taken back"
	default:
		return "unknown"
	}
}

func parseRecordState(s string) RecordState {
	switch s {
	case "moving":
		return StateMoving
	case "in place":
		return StateInPlace
	case "taken back":
		return StateTakenBack
	default:
		return StateMoving
	}
}

type Record struct {
	Data  string
	App   string
	Aside bool
	State RecordState
	Made  string
}

type Mutex struct {
	handle win32.HANDLE
}

func acquireMutex() (*Mutex, error) {
	h, err := win32.CreateMutex(mutexName)
	if err != nil {
		return nil, fmt.Errorf("CreateMutex %s: %w", mutexName, err)
	}
	wait := win32.WaitForSingleObject(h, win32.INFINITE)
	if wait != win32.WAIT_OBJECT_0 && wait != win32.WAIT_ABANDONED {
		win32.CloseHandle(h)
		return nil, fmt.Errorf("WaitForSingleObject mutex: %d", wait)
	}
	return &Mutex{handle: h}, nil
}

func (m *Mutex) Release() {
	if m.handle != 0 {
		win32.ReleaseMutex(m.handle)
		win32.CloseHandle(m.handle)
		m.handle = 0
	}
}

func lockPath(dataDir string) string {
	return strings.TrimRight(dataDir, `\/`) + ".hkcu.lock"
}

func Escape(key string) string {
	return strings.ReplaceAll(key, `\`, `/`)
}

func Unescape(name string) string {
	return strings.ReplaceAll(name, `/`, `\`)
}

func recordPath(key string) string {
	return recordsKey + `\` + Escape(key)
}

func readRecord(key string) (*Record, error) {
	k, err := regkey.Open(hkcuRoot, recordPath(key), win32.KEY_READ)
	if err != nil || k == nil {
		return nil, err
	}
	defer k.Close()

	stateStr, _ := k.GetString("State")
	if stateStr == nil {
		return nil, nil
	}

	dataStr, _ := k.GetString("Data")
	appStr, _ := k.GetString("App")
	asideStr, _ := k.GetString("Aside")
	madeStr, _ := k.GetString("Made")

	dataVal := ""
	if dataStr != nil {
		dataVal = *dataStr
	}
	appVal := ""
	if appStr != nil {
		appVal = *appStr
	}
	asideVal := false
	if asideStr != nil && *asideStr == "yes" {
		asideVal = true
	}
	madeVal := ""
	if madeStr != nil {
		madeVal = *madeStr
	}

	return &Record{
		Data:  dataVal,
		App:   appVal,
		Aside: asideVal,
		State: parseRecordState(*stateStr),
		Made:  madeVal,
	}, nil
}

func writeRecord(key string, r *Record) error {
	k, err := regkey.Create(hkcuRoot, recordPath(key))
	if err != nil {
		return err
	}
	defer k.Close()

	d := "Data"
	a := "App"
	as := "Aside"
	s := "State"
	m := "Made"

	_ = k.SetString(&d, r.Data)
	_ = k.SetString(&a, r.App)
	asideText := "no"
	if r.Aside {
		asideText = "yes"
	}
	_ = k.SetString(&as, asideText)
	_ = k.SetString(&s, r.State.String())
	if r.Made != "" {
		_ = k.SetString(&m, r.Made)
	}
	return nil
}

func setRecordState(key string, state RecordState) error {
	k, err := regkey.Create(hkcuRoot, recordPath(key))
	if err != nil {
		return err
	}
	defer k.Close()
	s := "State"
	return k.SetString(&s, state.String())
}

func deleteRecord(key string) error {
	records, err := regkey.Open(hkcuRoot, recordsKey, win32.KEY_READ|win32.KEY_WRITE)
	if err != nil || records == nil {
		return err
	}
	defer records.Close()
	return records.DeleteTree(Escape(key))
}

type Hive struct {
	Root win32.HKEY
}

func (h *Hive) Close() {
	if h.Root != 0 {
		win32.RegCloseKey(h.Root)
		h.Root = 0
	}
}

func openHive(dataDir string, create bool) (*Hive, error) {
	dir := filepath.Join(dataDir, "registry")
	file := filepath.Join(dir, "user.dat")
	if !create {
		if _, err := os.Stat(file); err != nil {
			return nil, nil
		}
	} else {
		if _, err := os.Stat(dataDir); err != nil {
			return nil, nil
		}
		_ = os.MkdirAll(dir, 0755)
	}
	root, status := win32.RegLoadAppKey(file, win32.KEY_ALL_ACCESS)
	if status != 0 {
		return nil, fmt.Errorf("loading registry hive %s: error %d", file, status)
	}
	return &Hive{Root: root}, nil
}

func Enter(st *store.Store, id string, keys []string, dataDir string, access Access, keep bool) (*Session, error) {
	if len(keys) == 0 {
		return nil, nil
	}

	mutex, err := acquireMutex()
	if err != nil {
		return nil, err
	}
	defer mutex.Release()

	lPath := lockPath(dataDir)
	first := false
	if tryL, _ := store.TryLock(lPath, true); tryL != nil {
		tryL.Release()
		first = true
	}

	lock, err := store.OpenLock(lPath, false, true)
	if err != nil {
		return nil, err
	}

	for _, key := range keys {
		r, err := readRecord(key)
		if err != nil {
			lock.Release()
			return nil, err
		}

		if r != nil {
			if samePath(r.Data, dataDir) {
				if first {
					_ = settle(st, key, r, true)
					if err := putInPlace(st, id, key, dataDir); err != nil {
						lock.Release()
						return nil, err
					}
				}
			} else {
				otherLock := lockPath(r.Data)
				if store.IsLocked(otherLock) {
					lock.Release()
					return nil, fmt.Errorf("%s keeps HKCU\\%s, which a run of %s is using (its data is in %s); run it once that ends", id, key, r.App, r.Data)
				}
				_ = settle(st, key, r, true)
				if err := putInPlace(st, id, key, dataDir); err != nil {
					lock.Release()
					return nil, err
				}
			}
		} else {
			if err := putInPlace(st, id, key, dataDir); err != nil {
				lock.Release()
				return nil, err
			}
		}

		switch access.Type {
		case AccessNone:
		case AccessLow:
			_ = acl.LabelKeyLow(key)
		case AccessCapability:
			_ = acl.GrantKey(key, access.SID)
			_ = grantAbove(key, access.SID)
		}
	}

	return &Session{
		ID:       id,
		Keys:     keys,
		DataDir:  dataDir,
		LockPath: lPath,
		Lock:     lock,
		Keep:     keep,
	}, nil
}

func Leave(st *store.Store, s *Session) {
	if s == nil {
		return
	}
	if err := leaveOrFail(st, s); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s's registry keys stay in HKCU until its next run, `zigsaw repair` or `zigsaw rm`\n", s.ID)
	}
}

func leaveOrFail(st *store.Store, s *Session) error {
	mutex, err := acquireMutex()
	if err != nil {
		return err
	}
	defer mutex.Release()

	s.Lock.Release()
	last, _ := store.TryLock(s.LockPath, true)
	if last == nil {
		return nil
	}

	for _, key := range s.Keys {
		r, err := readRecord(key)
		if err == nil && r != nil {
			if samePath(r.Data, s.DataDir) {
				if err := settle(st, key, r, s.Keep); err != nil {
					last.Release()
					return err
				}
			}
		}
	}
	_ = Tidy()
	last.Release()
	_ = os.Remove(s.LockPath)
	return nil
}

func Tidy() error {
	for _, name := range []string{recordsKey, asideKey, `Software\zigsaw`} {
		k, err := regkey.Open(hkcuRoot, name, win32.KEY_READ)
		if err == nil && k != nil {
			empty, _ := k.IsEmpty()
			k.Close()
			if empty {
				_ = win32.RegDeleteKey(hkcuRoot, name)
			}
		}
	}
	return nil
}

func SettleLeftovers(st *store.Store, id string) error {
	records, _ := regkey.Open(hkcuRoot, recordsKey, win32.KEY_READ)
	asides, _ := regkey.Open(hkcuRoot, asideKey, win32.KEY_READ|win32.KEY_WRITE)
	if records == nil && asides == nil {
		return nil
	}

	mutex, err := acquireMutex()
	if err != nil {
		if records != nil {
			records.Close()
		}
		if asides != nil {
			asides.Close()
		}
		return err
	}
	defer mutex.Release()

	if records != nil {
		defer records.Close()
		subkeys, _ := records.SubKeys()
		for _, name := range subkeys {
			key := Unescape(name)
			r, err := readRecord(key)
			if err != nil || r == nil {
				continue
			}
			if !IsInStore(r.Data, st.Root) {
				continue
			}
			if id != "" && !strings.EqualFold(id, r.App) {
				continue
			}
			lPath := lockPath(r.Data)
			if store.IsLocked(lPath) {
				continue
			}
			_ = settle(st, key, r, true)
			_ = os.Remove(lPath)
		}
	}

	if asides != nil {
		defer asides.Close()
		subkeys, _ := asides.SubKeys()
		for _, name := range subkeys {
			if r, _ := readRecord(Unescape(name)); r != nil {
				continue
			}
			_ = asides.DeleteTree(name)
		}
	}

	return Tidy()
}

func putInPlace(st *store.Store, id, key, dataDir string) error {
	name := Escape(key)
	native, _ := regkey.Exists(hkcuRoot, key)
	if native {
		asides, err := regkey.CreateWith(hkcuRoot, asideKey, win32.KEY_ALL_ACCESS)
		if err != nil {
			return err
		}
		defer asides.Close()
		_ = asides.DeleteTree(name)

		to, err := regkey.CreateWith(asides.Handle, name, win32.KEY_ALL_ACCESS)
		if err != nil {
			return err
		}
		defer to.Close()

		from, err := regkey.Open(hkcuRoot, key, win32.KEY_READ)
		if err == nil && from != nil {
			_ = to.CopyFrom(*from)
			from.Close()
		}
	}

	var made string
	if !native {
		made = OutermostMissing(key)
	}

	r := &Record{
		Data:  dataDir,
		App:   id,
		Aside: native,
		State: StateMoving,
		Made:  made,
	}
	_ = writeRecord(key, r)

	if native {
		_ = deleteKey(key)
		fmt.Fprintf(os.Stderr, "moved HKCU\\%s aside while %s runs; it comes back when the app ends\n", key, id)
	}

	dest, err := regkey.CreateWith(hkcuRoot, key, win32.KEY_ALL_ACCESS)
	if err != nil {
		return err
	}
	defer dest.Close()

	if h, _ := openHive(dataDir, false); h != nil {
		defer h.Close()
		if src, _ := regkey.Open(h.Root, key, win32.KEY_READ); src != nil {
			_ = dest.CopyFrom(*src)
			src.Close()
		}
	}

	return setRecordState(key, StateInPlace)
}

func settle(st *store.Store, key string, r *Record, keep bool) error {
	if r.State == StateInPlace {
		if keep {
			_ = saveToHive(st, key, r.Data)
		}
		_ = setRecordState(key, StateTakenBack)
	}

	_ = deleteKey(key)
	name := Escape(key)

	if r.Aside {
		asides, err := regkey.Open(hkcuRoot, asideKey, win32.KEY_READ)
		if err == nil && asides != nil {
			if from, err := regkey.Open(asides.Handle, name, win32.KEY_READ); err == nil && from != nil {
				if to, err := regkey.CreateWith(hkcuRoot, key, win32.KEY_ALL_ACCESS); err == nil {
					_ = to.CopyFrom(*from)
					to.Close()
				}
				from.Close()
			}
			asides.Close()
		}
	}

	_ = deleteRecord(key)

	if a, err := regkey.Open(hkcuRoot, asideKey, win32.KEY_READ|win32.KEY_WRITE); err == nil && a != nil {
		_ = a.DeleteTree(name)
		a.Close()
	}

	if !r.Aside && r.Made != "" {
		_ = DeleteEmptyParents(key, r.Made)
	}
	_ = revokeAbove(key, r.App)
	return nil
}

func grantAbove(key string, sid win32.PSID) error {
	parent := filepath.Dir(key)
	for strings.Contains(parent, `\`) {
		if ex, _ := regkey.Exists(hkcuRoot, parent); ex {
			_ = acl.GrantKeyRead(parent, sid)
		}
		parent = filepath.Dir(parent)
	}
	return nil
}

func revokeAbove(key, app string) error {
	if app == "" {
		return nil
	}
	sid, err := appcontainer.AppCapability(app)
	if err != nil {
		return err
	}
	defer win32.FreeSid(sid)

	parent := filepath.Dir(key)
	for strings.Contains(parent, `\`) {
		if ex, _ := regkey.Exists(hkcuRoot, parent); ex {
			_ = acl.RevokeKey(parent, sid)
		}
		parent = filepath.Dir(parent)
	}
	return nil
}

func saveToHive(st *store.Store, key, dataDir string) error {
	current, err := regkey.Open(hkcuRoot, key, win32.KEY_READ)
	if err != nil || current == nil {
		return err
	}
	defer current.Close()

	h, err := openHive(dataDir, true)
	if err != nil || h == nil {
		return err
	}
	defer h.Close()

	rootKey := regkey.Key{Handle: h.Root}
	_ = rootKey.DeleteTree(key)

	to, err := regkey.CreateWith(h.Root, key, win32.KEY_ALL_ACCESS)
	if err != nil {
		return err
	}
	defer to.Close()

	return to.CopyFrom(*current)
}

func deleteKey(key string) error {
	rootKey := regkey.Key{Handle: hkcuRoot}
	return rootKey.DeleteTree(key)
}

func OutermostMissing(key string) string {
	missing := ""
	parent := filepath.Dir(key)
	for strings.Contains(parent, `\`) {
		if ex, _ := regkey.Exists(hkcuRoot, parent); ex {
			break
		}
		missing = parent
		parent = filepath.Dir(parent)
	}
	return missing
}

func DeleteEmptyParents(key, outermost string) error {
	if !IsAbove(outermost, key) {
		return nil
	}
	parent := filepath.Dir(key)
	for len(parent) >= len(outermost) {
		k, err := regkey.Open(hkcuRoot, parent, win32.KEY_READ)
		if err != nil || k == nil {
			return nil
		}
		empty, _ := k.IsEmpty()
		k.Close()
		if !empty {
			return nil
		}
		if win32.RegDeleteKey(hkcuRoot, parent) != 0 {
			return nil
		}
		parent = filepath.Dir(parent)
	}
	return nil
}

func IsAbove(outer, key string) bool {
	return strings.Contains(outer, `\`) &&
		len(key) > len(outer)+1 &&
		strings.EqualFold(key[:len(outer)], outer) &&
		key[len(outer)] == '\\'
}

func samePath(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, `\/`), strings.TrimRight(b, `\/`))
}

func IsInStore(p, root string) bool {
	r := strings.TrimRight(root, `\/`)
	return len(p) > len(r) && strings.EqualFold(p[:len(r)], r) && (p[len(r)] == '\\' || p[len(r)] == '/')
}
