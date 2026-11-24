package hkcu

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

const (
	hkcuRoot   = win32.HKEY_CURRENT_USER
	recordsKey = `Software\quasidock\keys`
	asideKey   = `Software\quasidock\aside`
	mutexName  = `Local\quasidock-hkcu`
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
	HiveDir  string
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
	Apps    []string
	HiveDir string
	Locks   []string
	Data    string
	App     string
	Aside   bool
	State   RecordState
	Made    string
}

type Mutex struct {
	handle win32.HANDLE
}

func acquireMutex() (*Mutex, error) {
	runtime.LockOSThread()
	h, err := win32.CreateMutex(mutexName)
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("CreateMutex %s: %w", mutexName, err)
	}
	wait := win32.WaitForSingleObject(h, win32.INFINITE)
	if wait != win32.WAIT_OBJECT_0 && wait != win32.WAIT_ABANDONED {
		win32.CloseHandle(h)
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("WaitForSingleObject mutex: %d", wait)
	}
	return &Mutex{handle: h}, nil
}

func (m *Mutex) Release() {
	if m.handle != 0 {
		win32.ReleaseMutex(m.handle)
		win32.CloseHandle(m.handle)
		m.handle = 0
		runtime.UnlockOSThread()
	}
}

func lockPath(dataDir string) string {
	return strings.TrimRight(dataDir, `\/`) + ".hkcu.lock"
}

func recordRunning(r *Record) bool {
	if r == nil {
		return false
	}
	for _, path := range r.Locks {
		if store.IsLocked(path) {
			return true
		}
	}
	return false
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
	hiveStr, _ := k.GetString("HiveDir")
	locksStr, _ := k.GetString("Locks")
	appsStr, _ := k.GetString("Apps")

	dataVal := ""
	if dataStr != nil {
		dataVal = *dataStr
	}
	appVal := ""
	if appStr != nil {
		appVal = *appStr
	}
	asideVal := asideStr != nil && *asideStr == "yes"
	madeVal := ""
	if madeStr != nil {
		madeVal = *madeStr
	}

	hiveDir := filepath.Join(dataVal, "registry")
	if hiveStr != nil {
		hiveDir = *hiveStr
	}
	var locks []string
	if locksStr != nil {
		if err := json.Unmarshal([]byte(*locksStr), &locks); err != nil {
			return nil, err
		}
	} else {
		locks = []string{lockPath(dataVal)}
	}
	apps := []string{appVal}
	if appsStr != nil {
		if err := json.Unmarshal([]byte(*appsStr), &apps); err != nil {
			return nil, err
		}
	}
	return &Record{
		Apps:    apps,
		HiveDir: hiveDir,
		Locks:   locks,
		Data:    dataVal,
		App:     appVal,
		Aside:   asideVal,
		State:   parseRecordState(*stateStr),
		Made:    madeVal,
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
	h := "HiveDir"
	l := "Locks"
	ap := "Apps"
	apps, _ := json.Marshal(r.Apps)
	if err := k.SetString(&ap, string(apps)); err != nil {
		return err
	}
	locks, _ := json.Marshal(r.Locks)
	if err := k.SetString(&h, r.HiveDir); err != nil {
		return err
	}
	if err := k.SetString(&l, string(locks)); err != nil {
		return err
	}

	var errs []error
	errs = append(errs, k.SetString(&d, r.Data), k.SetString(&a, r.App))
	asideText := "no"
	if r.Aside {
		asideText = "yes"
	}
	errs = append(errs, k.SetString(&as, asideText), k.SetString(&s, r.State.String()))
	if r.Made != "" {
		errs = append(errs, k.SetString(&m, r.Made))
	}
	return errors.Join(errs...)
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

func openHive(dir string, create bool) (*Hive, error) {
	file := filepath.Join(dir, "user.dat")
	if !create {
		if _, err := os.Stat(file); err != nil {
			return nil, nil
		}
	} else {
		if _, err := os.Stat(dir); err != nil {
			return nil, err
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

	hiveDir := filepath.Join(dataDir, "registry")
	if err := os.MkdirAll(hiveDir, 0o755); err != nil {
		return nil, err
	}
	resolvedHive, err := mounts.CanonicalPath(hiveDir)
	if err != nil {
		return nil, fmt.Errorf("resolving registry directory %s: %w", hiveDir, err)
	}
	hiveDir = resolvedHive
	identity := sha256.Sum256([]byte(strings.ToLower(hiveDir)))
	lPath := st.Path("registry-locks", fmt.Sprintf("%x.lock", identity))
	// Inspect ownership before taking our lock, which may have the same path
	// as a stale record left by an earlier process in this store.
	active := map[string]bool{}
	for _, key := range keys {
		r, e := readRecord(key)
		if e != nil {
			return nil, e
		}
		active[key] = recordRunning(r)
	}

	lock, err := store.OpenLock(lPath, false, true)
	if err != nil {
		return nil, err
	}
	lPath, err = mounts.CanonicalPath(lPath)
	if err != nil {
		lock.Release()
		return nil, err
	}

	for _, key := range keys {
		r, err := readRecord(key)
		if err != nil {
			lock.Release()
			return nil, err
		}

		if r != nil {
			if samePath(r.HiveDir, hiveDir) {
				if !active[key] {
					if err := settle(st, key, r, true); err != nil {
						lock.Release()
						return nil, err
					}
					if err := putInPlace(st, id, key, dataDir, hiveDir, lPath); err != nil {
						lock.Release()
						return nil, err
					}
				}
			} else {
				if active[key] {
					lock.Release()
					return nil, fmt.Errorf("%s keeps HKCU\\%s, which a run of %s is using (its data is in %s); run it once that ends", id, key, r.App, r.Data)
				}
				if err := settle(st, key, r, true); err != nil {
					lock.Release()
					return nil, err
				}
				if err := putInPlace(st, id, key, dataDir, hiveDir, lPath); err != nil {
					lock.Release()
					return nil, err
				}
			}
		} else {
			if err := putInPlace(st, id, key, dataDir, hiveDir, lPath); err != nil {
				lock.Release()
				return nil, err
			}
		}

		current, err := readRecord(key)
		if err != nil || current == nil {
			lock.Release()
			if err == nil {
				err = fmt.Errorf("missing HKCU ownership record for %s", key)
			}
			return nil, err
		}
		found := false
		for _, p := range current.Locks {
			if samePath(p, lPath) {
				found = true
			}
		}
		if !found {
			current.Locks = append(current.Locks, lPath)
		}
		found = false
		for _, app := range current.Apps {
			if strings.EqualFold(app, id) {
				found = true
			}
		}
		if !found {
			current.Apps = append(current.Apps, id)
		}
		if err := writeRecord(key, current); err != nil {
			lock.Release()
			return nil, err
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
		HiveDir:  hiveDir,
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
		fmt.Fprintf(os.Stderr, "warning: %s's registry keys stay in HKCU until its next run, `quasidock repair` or `quasidock rmi`\n", s.ID)
	}
}

func leaveOrFail(st *store.Store, s *Session) error {
	mutex, err := acquireMutex()
	if err != nil {
		return err
	}
	defer mutex.Release()

	s.Lock.Release()
	for _, key := range s.Keys {
		r, err := readRecord(key)
		if err == nil && r != nil {
			if samePath(r.HiveDir, s.HiveDir) && !recordRunning(r) {
				if err := settle(st, key, r, s.Keep); err != nil {
					return err
				}
			}
		}
	}
	_ = Tidy()
	if !store.IsLocked(s.LockPath) {
		_ = os.Remove(s.LockPath)
	}
	return nil
}

func Tidy() error {
	for _, name := range []string{recordsKey, asideKey, `Software\quasidock`} {
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
	var errs []error
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
			belongs := IsInStore(r.Data, st.Root)
			for _, path := range r.Locks {
				if IsInStore(path, st.FullRoot()) {
					belongs = true
				}
			}
			if !belongs {
				continue
			}
			matches := id == ""
			for _, app := range r.Apps {
				if strings.EqualFold(id, app) {
					matches = true
				}
			}
			if !matches {
				continue
			}
			lPath := lockPath(r.Data)
			if recordRunning(r) {
				continue
			}
			if err := settle(st, key, r, true); err != nil {
				errs = append(errs, err)
				continue
			}
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

	errs = append(errs, Tidy())
	return errors.Join(errs...)
}

func putInPlace(st *store.Store, id, key, dataDir, hiveDir, leasePath string) error {
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
		Apps:    []string{id},
		HiveDir: hiveDir,
		Locks:   []string{leasePath},
		Data:    dataDir,
		App:     id,
		Aside:   native,
		State:   StateMoving,
		Made:    made,
	}
	if err := writeRecord(key, r); err != nil {
		return err
	}

	if native {
		_ = deleteKey(key)
		fmt.Fprintf(os.Stderr, "moved HKCU\\%s aside while %s runs; it comes back when the app ends\n", key, id)
	}

	dest, err := regkey.CreateWith(hkcuRoot, key, win32.KEY_ALL_ACCESS)
	if err != nil {
		return err
	}
	defer dest.Close()

	h, err := openHive(hiveDir, false)
	if err != nil {
		return err
	}
	if h != nil {
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
			if err := saveToHive(st, key, r.HiveDir); err != nil {
				return err
			}
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
	for _, app := range r.Apps {
		_ = revokeAbove(key, app)
	}
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
