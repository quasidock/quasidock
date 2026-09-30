package ledger

import (
	"fmt"
	"os"
	"strings"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

const DefaultKey = `Software\Quasidock\Grants`

type State int

const (
	StateLive State = iota
	StateOrphan
	StateAway
)

type Entry struct {
	Store string
	ID    string
	Grant store.Grant
}

func (e Entry) Name() string {
	return fmt.Sprintf("%s|%s|%s", e.Store, e.ID, e.Grant.String())
}

func Parse(valueName string) *Entry {
	parts := strings.Split(valueName, "|")
	if len(parts) != 3 {
		return nil
	}
	s, id, grantStr := parts[0], parts[1], parts[2]
	if len(s) == 0 || len(id) == 0 || len(grantStr) == 0 {
		return nil
	}
	return &Entry{
		Store: s,
		ID:    id,
		Grant: store.ParseGrant(grantStr),
	}
}

func (e Entry) OfStore(st string) bool {
	return strings.EqualFold(strings.TrimRight(e.Store, `\/`), strings.TrimRight(st, `\/`))
}

// List returns all entries recorded under HKCU\key.
func List(key string) ([]Entry, error) {
	k, err := regkey.Open(win32.HKEY_CURRENT_USER, key, win32.KEY_READ)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, nil
	}
	defer k.Close()

	names, err := k.ValueNames()
	if err != nil {
		return nil, err
	}

	var out []Entry
	for _, n := range names {
		if e := Parse(n); e != nil {
			out = append(out, *e)
		}
	}
	return out, nil
}

// Sync synchronizes the registry records under key for (storeRoot, id) to match grants.
func Sync(key, storeRoot, id string, grants []store.Grant) error {
	below, _ := acl.BelowMedium()
	if below {
		return nil
	}

	existing, err := List(key)
	if err != nil {
		return err
	}

	var stale []Entry
	for _, e := range existing {
		if !e.OfStore(storeRoot) || !strings.EqualFold(e.ID, id) {
			continue
		}
		kept := false
		for _, g := range grants {
			if g.Equal(e.Grant) {
				kept = true
				break
			}
		}
		if !kept {
			stale = append(stale, e)
		}
	}

	var missing []Entry
	for _, g := range grants {
		here := false
		for _, e := range existing {
			if e.OfStore(storeRoot) && strings.EqualFold(e.ID, id) && g.Equal(e.Grant) {
				here = true
				break
			}
		}
		if !here {
			missing = append(missing, Entry{Store: storeRoot, ID: id, Grant: g})
		}
	}

	if len(stale) == 0 && len(missing) == 0 {
		return nil
	}

	k, err := regkey.Create(win32.HKEY_CURRENT_USER, key)
	if err != nil {
		return err
	}
	defer k.Close()

	for _, e := range stale {
		_ = k.DeleteValue(e.Name())
	}
	for _, e := range missing {
		nm := e.Name()
		_ = k.SetString(&nm, "")
	}

	if len(missing) == 0 {
		_ = DropIfEmpty(key)
	}
	return nil
}

// Remove forgets one entry from the registry ledger.
func Remove(key string, e Entry) error {
	k, err := regkey.Open(win32.HKEY_CURRENT_USER, key, win32.KEY_READ|win32.KEY_WRITE)
	if err != nil {
		return err
	}
	if k == nil {
		return nil
	}
	_ = k.DeleteValue(e.Name())
	k.Close()
	return DropIfEmpty(key)
}

// DropIfEmpty deletes key if empty, and its parent if left empty (up to Software\...).
func DropIfEmpty(key string) error {
	if !removeIfEmpty(key) {
		return nil
	}
	slash := strings.LastIndexByte(key, '\\')
	if slash == -1 {
		return nil
	}
	parent := key[:slash]
	if strings.ContainsRune(parent, '\\') {
		_ = removeIfEmpty(parent)
	}
	return nil
}

func removeIfEmpty(key string) bool {
	k, err := regkey.Open(win32.HKEY_CURRENT_USER, key, win32.KEY_READ)
	if err != nil || k == nil {
		return false
	}
	empty, _ := k.IsEmpty()
	k.Close()
	if !empty {
		return false
	}
	return win32.RegDeleteKey(win32.HKEY_CURRENT_USER, key) == 0
}

// StateOf evaluates whether an entry's store still stands behind the grant.
func StateOf(e Entry) State {
	st, err := store.Open(e.Store, false, "")
	if err != nil {
		if strings.HasPrefix(e.Store, `\\`) {
			return StateAway
		}
		if len(e.Store) >= 3 && e.Store[1] == ':' {
			if _, err := os.Stat(e.Store[:3]); err != nil {
				return StateAway
			}
		}
		return StateOrphan
	}

	if _, err := os.Stat(st.Path("grants")); err != nil {
		if strings.HasPrefix(e.Store, `\\`) {
			return StateAway
		}
		if len(e.Store) >= 3 && e.Store[1] == ':' {
			if _, err := os.Stat(e.Store[:3]); err != nil {
				return StateAway
			}
		}
		return StateOrphan
	}

	grants, err := st.ReadGrants(e.ID)
	if err != nil {
		return StateLive
	}
	for _, g := range grants {
		if g.Equal(e.Grant) {
			return StateLive
		}
	}
	return StateOrphan
}
