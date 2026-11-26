package lazy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/store"
)

func TestPruneLayerLeaseAndScope(t *testing.T) {
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	hex := strings.Repeat("a", 64)
	digest := "sha256:" + hex
	deploy, _ := st.DeployPath(digest)
	dir := DirPath(st, hex)
	for _, path := range []string{filepath.Join(deploy, "file"), filepath.Join(dir, "blob.sparse"), st.Path("lazy", "quasidock-keep.exe")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := store.OpenLock(store.LockPathFor(deploy), false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	for _, dry := range []bool{true, false} {
		if n, err := PruneLayer(st, digest, dry); err != nil || n != 0 || !store.Exists(deploy) || !store.Exists(dir) {
			t.Fatalf("leased dry=%t: %d %v", dry, n, err)
		}
	}
	lock.Release()
	helper, err := store.OpenLock(LockPath(st, hex, "helper"), true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Release()
	if n, err := PruneLayer(st, digest, true); err != nil || n != 10 || !store.Exists(deploy) || !store.Exists(dir) {
		t.Fatalf("preview: %d %v", n, err)
	}
	if !HelperAlive(st, hex) {
		t.Fatal("preview stopped the helper")
	}
	helper.Release()
	if n, err := PruneLayer(st, digest, false); err != nil || n != 10 || store.Exists(deploy) || store.Exists(dir) || !store.Exists(st.Path("lazy", "quasidock-keep.exe")) {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	if _, err := PruneLayer(st, "invalid", false); err == nil {
		t.Fatal("accepted invalid digest")
	}
}
