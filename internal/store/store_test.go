package store

import (
	"path/filepath"
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
)

func TestStoreLocks(t *testing.T) {
	tmpDir := t.TempDir()
	p := filepath.Join(tmpDir, "x.lock")

	if IsLocked(p) {
		t.Fatal("expected not locked initially")
	}

	runA, err := OpenLock(p, false, false)
	if err != nil {
		t.Fatalf("OpenLock runA failed: %v", err)
	}
	defer runA.Release()

	runB, err := OpenLock(p, false, false)
	if err != nil {
		t.Fatalf("OpenLock runB failed: %v", err)
	}
	defer runB.Release()

	if !IsLocked(p) {
		t.Fatal("expected locked by shared locks")
	}

	exclusive, err := TryLock(p, true)
	if err != nil {
		t.Fatalf("TryLock exclusive returned err: %v", err)
	}
	if exclusive != nil {
		exclusive.Release()
		t.Fatal("expected exclusive lock to fail while shared locks exist")
	}

	runA.Release()
	runB.Release()

	exLock, err := TryLock(p, true)
	if err != nil {
		t.Fatalf("TryLock exclusive failed: %v", err)
	}
	if exLock == nil {
		t.Fatal("expected exclusive lock to succeed after shared locks released")
	}
	defer exLock.Release()

	shared, err := TryLock(p, false)
	if err != nil {
		t.Fatalf("TryLock shared returned err: %v", err)
	}
	if shared != nil {
		shared.Release()
		t.Fatal("expected shared lock to fail while exclusive lock held")
	}

	exLock.Release()
	if IsLocked(p) {
		t.Fatal("expected not locked after all locks released")
	}
}

func TestStoreBlobsAndRefs(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := Open(tmpDir, false, "")
	if err != nil {
		t.Fatalf("Open store failed: %v", err)
	}

	// 1. Put and Read Blob
	data := []byte("example-blob-data")
	desc, err := st.PutBlob(data, oci.MediaTypeLayerTar)
	if err != nil {
		t.Fatalf("PutBlob failed: %v", err)
	}
	if desc.Size != int64(len(data)) {
		t.Fatalf("size mismatch: got %d, want %d", desc.Size, len(data))
	}

	readBack, err := st.ReadBlob(desc.Digest)
	if err != nil {
		t.Fatalf("ReadBlob failed: %v", err)
	}
	if string(readBack) != string(data) {
		t.Fatalf("blob content mismatch: %s", string(readBack))
	}

	// 2. Refs
	ref := Ref{
		ID:       "com.example.app",
		Version:  "1.0.0",
		Manifest: desc.Digest,
		Source:   "localhost:5000/app:latest",
	}
	if err := st.WriteRef(ref); err != nil {
		t.Fatalf("WriteRef failed: %v", err)
	}

	gotRef, err := st.ReadRef(ref.ID)
	if err != nil {
		t.Fatalf("ReadRef failed: %v", err)
	}
	if gotRef == nil || gotRef.Version != "1.0.0" {
		t.Fatalf("unexpected ref read: %+v", gotRef)
	}

	refs, err := st.ListRefs()
	if err != nil {
		t.Fatalf("ListRefs failed: %v", err)
	}
	if len(refs) != 1 || refs[0].ID != ref.ID {
		t.Fatalf("unexpected ListRefs: %+v", refs)
	}

	if err := st.DeleteRef(ref.ID); err != nil {
		t.Fatalf("DeleteRef failed: %v", err)
	}
	gotRef, err = st.ReadRef(ref.ID)
	if err != nil || gotRef != nil {
		t.Fatalf("expected nil after delete, got %+v (err %v)", gotRef, err)
	}
}

func TestStoreGrants(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := Open(tmpDir, false, "")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.RecordGrant("a.app", Grant{Kind: GrantLow, Path: "D:\\proj"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordGrant("a.app", Grant{Kind: GrantAppContainer, Path: "D:\\proj"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordGrant("a.app", Grant{Kind: GrantLow, Path: "d:\\PROJ"}); err != nil { // Case insensitive duplicate
		t.Fatal(err)
	}
	if err := st.RecordGrant("a.app", Grant{Kind: GrantAppContainer, Path: "D:\\other"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordGrant("b.app", Grant{Kind: GrantLow, Path: "d:\\Proj"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordGrant("b.app", Grant{Kind: GrantLoopback}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordGrant("b.app", Grant{Kind: GrantLoopback}); err != nil {
		t.Fatal(err)
	}

	a, err := st.ReadGrants("a.app")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 3 {
		t.Fatalf("expected 3 grants for a.app, got %d", len(a))
	}

	others, err := st.GrantedToOthers("a.app", Grant{Kind: GrantLow, Path: "D:\\PROJ"})
	if err != nil || !others {
		t.Fatalf("expected true for grantedToOthers on D:\\PROJ")
	}

	if err := st.RecordCwdGrant("a.app", "D:\\proj"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCwdGrant("a.app", "d:\\PROJ"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCwdGrant("a.app", "D:\\cwd"); err != nil {
		t.Fatal(err)
	}
	cwds, _ := st.ReadCwdGrants("a.app")
	if len(cwds) != 2 {
		t.Fatalf("expected 2 cwd grants, got %d: %v", len(cwds), cwds)
	}

	if err := st.DeleteGrants("a.app"); err != nil {
		t.Fatal(err)
	}
	a, _ = st.ReadGrants("a.app")
	if len(a) != 0 {
		t.Fatalf("expected 0 grants after delete, got %d", len(a))
	}
}
