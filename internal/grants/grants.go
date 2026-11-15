package grants

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
	"github.com/quasidock/quasidock/internal/written"
)

// Own tracks the store root and quasidock executable directory so no run may write to them.
type Own struct {
	Store   string
	SelfDir string
}

func NewOwn(storeRoot string) Own {
	cleanStore := filepath.Clean(storeRoot)
	exe, err := os.Executable()
	var selfDir string
	if err == nil {
		selfDir = filepath.Dir(exe)
	}
	return Own{
		Store:   cleanStore,
		SelfDir: selfDir,
	}
}

func (o Own) Refusal(path string) (string, bool) {
	if IsWithin(o.Store, path) {
		return "quasidock's store is in it", true
	}
	if IsWithin(path, o.Store) {
		return "it's in quasidock's store", true
	}
	if o.SelfDir != "" {
		if IsWithin(o.SelfDir, path) {
			return "quasidock is in it", true
		}
		if IsWithin(path, o.SelfDir) {
			return "it's in quasidock's folder", true
		}
	}
	return "", false
}

func (o Own) Contains(path string) bool {
	if IsWithin(path, o.Store) {
		return true
	}
	if o.SelfDir != "" && IsWithin(path, o.SelfDir) {
		return true
	}
	return false
}

func SamePath(a, b string) bool {
	x := strings.TrimRight(a, `\/`)
	y := strings.TrimRight(b, `\/`)
	if len(x) != len(y) {
		return false
	}
	for i := 0; i < len(x); i++ {
		c := x[i]
		d := y[i]
		if (c == '/' || c == '\\') && (d == '/' || d == '\\') {
			continue
		}
		if strings.ToLower(string(c)) != strings.ToLower(string(d)) {
			return false
		}
	}
	return true
}

func IsWithin(path, dir string) bool {
	p := strings.TrimRight(path, `\/`)
	d := strings.TrimRight(dir, `\/`)
	if len(p) < len(d) {
		return false
	}
	if !SamePath(p[:len(d)], d) {
		return false
	}
	return len(p) == len(d) || p[len(d)] == '/' || p[len(d)] == '\\'
}

type StandingPath struct {
	Path   string
	Access acl.Access
}

type Standing struct {
	Sandbox oci.Sandbox
	Paths   []StandingPath
	Cwd     *acl.Access
	Own     *Own
}

func (s Standing) Keeps(g store.Grant, wasCwd bool) *acl.Access {
	if g.Kind == store.GrantLoopback {
		return nil
	}

	var access *acl.Access
	if wasCwd && s.Cwd != nil {
		c := *s.Cwd
		access = &c
	}
	for _, p := range s.Paths {
		if SamePath(p.Path, g.Path) {
			access = wider(access, p.Access)
		}
	}

	if access == nil {
		return nil
	}
	a := *access

	if s.Own != nil {
		if _, refused := s.Own.Refusal(g.Path); refused {
			a = acl.AccessReadExecute
		}
	}

	switch g.Kind {
	case store.GrantAppContainer:
		if s.Sandbox == oci.SandboxAppContainer {
			return &a
		}
		return nil
	case store.GrantLow:
		if s.Sandbox == oci.SandboxLow && a == acl.AccessFull {
			return &a
		}
		return nil
	default:
		return nil
	}
}

func wider(a *acl.Access, b acl.Access) *acl.Access {
	if a == nil {
		res := b
		return &res
	}
	if *a == acl.AccessFull || b == acl.AccessFull {
		res := acl.AccessFull
		return &res
	}
	res := acl.AccessReadExecute
	return &res
}

// Session represents an app's active run.
type Session struct {
	ID       string
	LockPath string
	Lock     *store.Lock
}

func Begin(st *store.Store, id string) (*Session, error) {
	lockPath := st.RunsLockPath(id)
	l, err := store.OpenLock(lockPath, false, true)
	if err != nil {
		return nil, err
	}
	return &Session{
		ID:       id,
		LockPath: lockPath,
		Lock:     l,
	}, nil
}

func (s *Session) End(st *store.Store) error {
	if s.Lock != nil {
		s.Lock.Release()
		s.Lock = nil
	}
	last, err := store.TryLock(s.LockPath, true)
	if err != nil || last == nil {
		return nil
	}
	defer last.Release()

	return Settle(st, s.ID)
}

func Settle(st *store.Store, id string) error {
	lock, err := st.LockGrantChanges()
	if err != nil {
		return err
	}
	defer lock.Release()

	recorded, err := st.ReadGrants(id)
	if err != nil {
		return err
	}
	for _, g := range recorded {
		if g.Kind == store.GrantLoopback {
			if err := unexempt(id); err != nil {
				return err
			}
		}
	}
	// Mount sessions restore host permissions when the final owner releases
	// them. Drop their legacy per-app records, but retain written programs so
	// later low runs can relabel them before writing them again.
	if err := written.Settle(st, id); err != nil {
		return err
	}
	if err := st.WriteGrants(id, nil); err != nil {
		return err
	}
	return st.WriteCwdGrants(id, nil)
}

func unexempt(id string) error {
	sid, err := appcontainer.PackageSid(id)
	if err != nil {
		return err
	}
	defer win32.FreeSid(sid)
	_, err = appcontainer.Unexempt(sid)
	return err
}

// SettleUnlessRunning settles the app's grants after what it may reach changed,
// unless a run of it is going. Returns false if one is, and the app has grants to settle.
func SettleUnlessRunning(ctx *context.Context, id string) (bool, error) {
	st := ctx.Store
	g, err1 := st.ReadGrants(id)
	cg, err2 := st.ReadCwdGrants(id)
	pr, err3 := st.ReadPrograms(id)
	if (err1 != nil || len(g) == 0) && (err2 != nil || len(cg) == 0) && (err3 != nil || len(pr) == 0) {
		return true, nil
	}
	l, err := store.TryLock(st.RunsLockPath(id), true)
	if err != nil || l == nil {
		return false, nil
	}
	defer l.Release()
	if err := Settle(st, id); err != nil {
		return false, err
	}
	return true, nil
}

// SettleAll settles the grants of every app whose runs have all ended.
// Returns how many failed.
func SettleAll(ctx *context.Context) (int, error) {
	ids, err := ctx.Store.ListGrantIds()
	if err != nil {
		return 0, err
	}
	failed := 0
	for _, id := range ids {
		_, err := SettleUnlessRunning(ctx, id)
		if err != nil {
			failed++
		}
	}
	return failed, nil
}

// UndoAll takes back everything the app's runs were given after its last cached image is removed.
func UndoAll(ctx *context.Context, id string) error {
	guard, err := LockMountSources(ctx.Store)
	if err != nil {
		return err
	}
	defer guard.Close()
	lock, err := ctx.Store.LockGrantChanges()
	if err != nil {
		return err
	}
	defer lock.Release()

	recorded, err := ctx.Store.ReadGrants(id)
	if err == nil && len(recorded) > 0 {
		for _, g := range recorded {
			if g.Kind == store.GrantLoopback {
				if err := unexempt(id); err != nil {
					return err
				}
				continue
			}
			if !store.Exists(g.Path) {
				continue
			}
			protected, err := guard.Protected(g.Path)
			if err != nil {
				return err
			}
			if protected {
				continue
			}
			if g.Kind == store.GrantLow {
				_ = acl.Unlabel(g.Path)
			} else {
				capability, err := appcontainer.AppCapability(id)
				if err != nil {
					return err
				}
				_, _ = acl.RevokeAccess(g.Path, acl.Revoke{Kind: acl.RevokeSids, Sids: []win32.PSID{capability}})
				win32.LocalFree(capability)
			}
		}
	}
	if err := written.Settle(ctx.Store, id); err != nil {
		return err
	}
	return ctx.Store.DeleteGrants(id)
}

func SettleOrphans(ctx *context.Context) (int, error) {
	return 0, nil
}

func NoteLeftovers(ctx *context.Context) error {
	return nil
}

func Show(ctx *context.Context, only *string) error {
	ids, err := ctx.Store.ListGrantIds()
	if err != nil {
		return err
	}
	any := false
	heading := false
	for _, id := range ids {
		if only != nil && !strings.EqualFold(*only, id) {
			continue
		}
		recorded, err := ctx.Store.ReadGrants(id)
		if err != nil || len(recorded) == 0 {
			continue
		}
		if !heading {
			fmt.Printf("This store, %s\n", ctx.Store.Root)
			heading = true
		}
		any = true
		fmt.Printf("  %s\n", id)
		for _, g := range recorded {
			fmt.Printf("    %s\n", g.String())
		}
	}
	guard, err := LockMountSources(ctx.Store)
	if err != nil {
		return err
	}
	sources, err := readMountSources(ctx.Store)
	if err != nil {
		guard.Close()
		return err
	}
	for _, source := range sources {
		for _, owner := range source.Owners {
			if IsWithin(owner.Lock, ctx.Store.FullRoot()) && (only == nil || strings.EqualFold(*only, owner.AppID)) && store.IsLocked(owner.Lock) {
				fmt.Printf("  %s: active container storage %s\n", owner.AppID, source.Path)
				any = true
				break
			}
		}
	}
	guard.Close()
	if !any {
		if only != nil {
			fmt.Println("Nothing recorded for that app.")
		} else {
			fmt.Println("quasidock has changed nothing outside its stores.")
		}
	}
	return nil
}

func TakeBackLabel(ctx *context.Context, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("--take-back takes an absolute path; got %q", path)
	}
	guard, err := LockMountSources(ctx.Store)
	if err != nil {
		return err
	}
	defer guard.Close()
	protected, err := guard.Protected(path)
	if err != nil {
		return err
	}
	if protected {
		return fmt.Errorf("%s is used by a running container; stop it first", path)
	}
	return acl.Unlabel(path)
}
