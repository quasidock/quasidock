package trust

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/store"
)

func TestUnderPrefix(t *testing.T) {
	cases := []struct {
		prefix string
		repo   string
		want   bool
	}{
		{"ghcr.io/zigsaw-io", "ghcr.io/zigsaw-io/org.nodejs.node", true},
		{"ghcr.io/zigsaw-io/", "ghcr.io/zigsaw-io/org.nodejs.node", true},
		{"GHCR.io/zigsaw-io", "ghcr.io/zigsaw-io/org.nodejs.node", true},
		{"localhost:5000", "localhost:5000/a/b", true},
		{"ghcr.io/me/app", "ghcr.io/me/app", true},
		{"ghcr.io/zigsaw-io", "ghcr.io/zigsaw-iox/app", false},
		{"ghcr.io/zigsaw-io", "ghcr.io/other/app", false},
		{"ghcr.io/me/app", "ghcr.io/me", false},
		{"", "ghcr.io/me", false},
		{"/", "ghcr.io/me", false},
	}

	for _, c := range cases {
		got := UnderPrefix(c.prefix, c.repo)
		if got != c.want {
			t.Errorf("UnderPrefix(%q, %q) = %v, want %v", c.prefix, c.repo, got, c.want)
		}
	}
}

func TestBuiltInKeys(t *testing.T) {
	if len(PublishedKeys) != 2 {
		t.Fatalf("expected 2 published keys, got %d", len(PublishedKeys))
	}
	allKeys, err := All(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(allKeys) != 2 {
		t.Fatalf("expected 2 named keys, got %d", len(allKeys))
	}
	if allKeys[0].Key.Fingerprint == allKeys[1].Key.Fingerprint {
		t.Errorf("expected different fingerprints for built-in keys")
	}
}

func TestTrustAddRemove(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}

	keyFile := filepath.Join(dir, "mykey.pub")
	testPEM := `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEXThf29L0MZDKJ0XmGZOnsa9bTfnm
GwTc6JcpcVqMxsROLSOmJYvylIJpsGBkl2Vq9GfVSO/1BQ3E0/+r5f69cA==
-----END PUBLIC KEY-----`
	if err := os.WriteFile(keyFile, []byte(testPEM), 0600); err != nil {
		t.Fatal(err)
	}

	// Add key
	k, added, err := Add(st, "localhost:5000/myteam", keyFile)
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if !added {
		t.Errorf("expected added=true")
	}

	// Add again (idempotent)
	_, added2, err := Add(st, "localhost:5000/myteam", keyFile)
	if err != nil {
		t.Fatalf("Add duplicate error: %v", err)
	}
	if added2 {
		t.Errorf("expected added2=false")
	}

	// Verify image lookup
	keys, err := ForImage(st, "localhost:5000/myteam/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key for image, got %d", len(keys))
	}
	if keys[0].Fingerprint != k.Fingerprint {
		t.Errorf("got fingerprint %s, want %s", keys[0].Fingerprint, k.Fingerprint)
	}

	// Image not matching prefix gets no custom key
	otherKeys, err := ForImage(st, "localhost:5000/other/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(otherKeys) != 0 {
		t.Errorf("expected 0 keys for other, got %d", len(otherKeys))
	}

	// Remove by prefix
	removed, err := Remove(st, "localhost:5000/myteam", nil)
	if err != nil {
		t.Fatalf("Remove error: %v", err)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}

	// Remove non-existent
	if _, err := Remove(st, "localhost:5000/myteam", nil); err == nil {
		t.Errorf("expected error removing non-existent prefix")
	}
}
