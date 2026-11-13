package ledger

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestEntryParseAndName(t *testing.T) {
	cases := []store.Grant{
		{Kind: store.GrantLow, Path: `C:\Users\a\Documents`},
		{Kind: store.GrantAppContainer, Path: `D:\proj`},
		{Kind: store.GrantLoopback},
	}

	for _, g := range cases {
		e := Entry{Store: `E:\s`, ID: "a.b", Grant: g}
		name := e.Name()
		parsed := Parse(name)
		if parsed == nil {
			t.Fatalf("failed to parse %s", name)
		}
		if parsed.Store != `E:\s` || parsed.ID != "a.b" || !parsed.Grant.Equal(g) {
			t.Fatalf("roundtrip mismatch: %+v vs %+v", parsed, e)
		}
	}

	for _, bad := range []string{"", `E:\s`, `E:\s|a.b`, `|a.b|x`, `E:\s||x`, `E:\s|a.b|`, "a|b|c|d"} {
		if Parse(bad) != nil {
			t.Errorf("expected nil for %s", bad)
		}
	}

	e := Entry{Store: `E:\S\`, ID: "a.b", Grant: store.Grant{Kind: store.GrantLoopback}}
	if !e.OfStore(`e:\s`) {
		t.Error("expected ofStore true")
	}
	if e.OfStore(`e:\s2`) {
		t.Error("expected ofStore false")
	}
}

func TestSyncListStateOfAndRemove(t *testing.T) {
	testKey := fmt.Sprintf(`Software\QuasidockTest\ledger-%d`, os.Getpid())
	defer func() {
		win32.RegDeleteTree(win32.HKEY_CURRENT_USER, testKey)
		win32.RegDeleteKey(win32.HKEY_CURRENT_USER, `Software\QuasidockTest`)
	}()

	entries, err := List(testKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty initially, got %d", len(entries))
	}

	tmpDir := t.TempDir()
	st, err := store.Open(tmpDir, false, testKey)
	if err != nil {
		t.Fatal(err)
	}

	label := store.Grant{Kind: store.GrantLow, Path: `D:\proj`}
	other := store.Grant{Kind: store.GrantAppContainer, Path: `D:\other`}
	_ = st.WriteGrants("a.app", []store.Grant{label})

	root := filepath.Clean(tmpDir)
	if err := Sync(testKey, root, "a.app", []store.Grant{label, other}); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if err := Sync(testKey, `E:\gone`, "b.app", []store.Grant{label}); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	entries, err = List(testKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Idempotent sync
	if err := Sync(testKey, root, "a.app", []store.Grant{label, other}); err != nil {
		t.Fatal(err)
	}
	entries, _ = List(testKey)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries after idempotent sync, got %d", len(entries))
	}

	live := 0
	orphans := 0
	for _, e := range entries {
		switch StateOf(e) {
		case StateLive:
			live++
		case StateOrphan:
			orphans++
		}
	}
	if live != 1 {
		t.Errorf("expected 1 live, got %d", live)
	}
	if orphans < 1 {
		t.Errorf("expected at least 1 orphan, got %d", orphans)
	}

	// Sync only other
	if err := Sync(testKey, root, "a.app", []store.Grant{other}); err != nil {
		t.Fatal(err)
	}
	left, _ := List(testKey)
	if len(left) != 2 {
		t.Fatalf("expected 2 left, got %d", len(left))
	}

	for _, e := range left {
		_ = Remove(testKey, e)
	}
	left, _ = List(testKey)
	if len(left) != 0 {
		t.Fatalf("expected 0 left after remove, got %d", len(left))
	}
}
