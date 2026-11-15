package grants

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestMountGrantsSharedAcrossStoresAndCrashRepair(t *testing.T) {
	ledger := `Software\Quasidock\Tests\GrantMounts` + fmt.Sprint(time.Now().UnixNano())
	t.Cleanup(func() { k := regkey.Key{Handle: win32.HKEY_CURRENT_USER}; _ = k.DeleteTree(ledger) })
	source := t.TempDir()
	child := filepath.Join(source, "nested")
	_ = os.Mkdir(child, 0o755)
	baseline, err := acl.OwnLabel(source)
	if err != nil {
		t.Fatal(err)
	}
	stores := make([]*store.Store, 2)
	records := make([]*store.ContainerRecord, 2)
	locks := make([]*store.Lock, 2)
	for i := range stores {
		st, err := store.Open(t.TempDir(), false, ledger)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = st
		c, err := st.CreateContainer("", store.ContainerRecord{AppID: fmt.Sprintf("mount-grant-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		records[i] = c
		l, err := store.OpenLock(st.ContainerRunLockPath(c.ID), true, false)
		if err != nil {
			t.Fatal(err)
		}
		locks[i] = l
		t.Cleanup(l.Release)
	}
	first, err := BeginMounts(stores[0], records[0], oci.SandboxLow, []MountRequest{{Path: source}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := BeginMounts(stores[1], records[1], oci.SandboxLow, []MountRequest{{Path: child}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close(); _ = second.Close() })
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if level, err := acl.EffectiveLabel(child); err != nil || level == nil || *level > win32.SECURITY_MANDATORY_LOW_RID {
		t.Fatalf("removed live nested consumer label: %v %v", level, err)
	}
	// An image's legacy grant cleanup must leave a live mounted consumer alone.
	if err := stores[0].RecordGrant("old-app", store.Grant{Kind: store.GrantLow, Path: source}); err != nil {
		t.Fatal(err)
	}
	ctx := context.New()
	ctx.Store = stores[0]
	if err := UndoAll(ctx, "old-app"); err != nil {
		t.Fatal(err)
	}
	if level, err := acl.EffectiveLabel(child); err != nil || level == nil || *level > win32.SECURITY_MANDATORY_LOW_RID {
		t.Fatal("image removal took active label")
	}
	// Simulate owner death without calling Close, then reconcile from another store.
	locks[1].Release()
	if session, err := BeginMounts(stores[0], records[0], oci.SandboxSoft, nil); err != nil || session != nil {
		t.Fatalf("a run without host grants did not repair dead owners: %v %v", session, err)
	}
	after, err := acl.OwnLabel(source)
	if err != nil {
		t.Fatal(err)
	}
	if (baseline == nil) != (after == nil) || baseline != nil && *baseline != *after {
		t.Fatalf("original integrity label not restored: %v %v", baseline, after)
	}
}

func TestMountGrantsPreserveOtherCapabilitiesAndBaseline(t *testing.T) {
	ledger := `Software\Quasidock\Tests\GrantCaps` + fmt.Sprint(time.Now().UnixNano())
	t.Cleanup(func() { k := regkey.Key{Handle: win32.HKEY_CURRENT_USER}; _ = k.DeleteTree(ledger) })
	st, err := store.Open(t.TempDir(), false, ledger)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	var sessions []*MountSession
	var sids []win32.PSID
	var baseline win32.DWORD
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("mount-cap-%d-%d", time.Now().UnixNano(), i)
		sid, err := appcontainer.AppCapability(id)
		if err != nil {
			t.Fatal(err)
		}
		sids = append(sids, sid)
		t.Cleanup(func() { win32.LocalFree(sid) })
		if i == 0 {
			if _, err := acl.Grant(source, sid, acl.AccessReadExecute, acl.Revoke{Kind: acl.RevokeNone}); err != nil {
				t.Fatal(err)
			}
			baseline, _ = acl.ExplicitGrant(source, sid)
		}
		c, err := st.CreateContainer("", store.ContainerRecord{AppID: id})
		if err != nil {
			t.Fatal(err)
		}
		lock, err := store.OpenLock(st.ContainerRunLockPath(c.ID), true, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(lock.Release)
		s, err := BeginMounts(st, c, oci.SandboxAppContainer, []MountRequest{{Path: source}})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
		t.Cleanup(func() { _ = s.Close() })
	}
	if err := sessions[0].Close(); err != nil {
		t.Fatal(err)
	}
	if yes, err := acl.Grants(source, sids[1], acl.AccessFull); err != nil || !yes {
		t.Fatal("another capability was revoked")
	}
	if after, err := acl.ExplicitGrant(source, sids[0]); err != nil || after != baseline {
		t.Fatalf("original read access not restored: %x %x %v", baseline, after, err)
	}
	if err := sessions[1].Close(); err != nil {
		t.Fatal(err)
	}
	if after, err := acl.ExplicitGrant(source, sids[1]); err != nil || after != 0 {
		t.Fatalf("added capability not removed: %x %v", after, err)
	}
}
