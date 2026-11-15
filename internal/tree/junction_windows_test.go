package tree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddDirFilesRejectsJunction(t *testing.T) {
	dir, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "outside.txt"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(dir, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("creating junction: %v: %s", err, out)
	}
	t.Cleanup(func() { os.Remove(junction) })
	if err := New().AddDirFiles(dir, ""); err == nil || !strings.Contains(err.Error(), "can only be files and directories") {
		t.Fatalf("junction was not rejected: %v", err)
	}
}
