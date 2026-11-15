package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotJunctionMustBeSkipped(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "app.txt"), filepath.Join(target, "outside.txt")} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	junction := filepath.Join(dir, "to-src")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("creating junction: %v: %s", err, out)
	}
	t.Cleanup(func() { os.Remove(junction) })
	if _, err := TakeSnapshot(dir, nil); err == nil || !strings.Contains(err.Error(), "to-src is a link") {
		t.Fatalf("junction was not reported as a link: %v", err)
	}
	snapshot, err := TakeSnapshot(dir, []string{"/to-src"})
	if err != nil || snapshot.Files != 1 {
		t.Fatalf("skipping the junction: %v %v", snapshot, err)
	}
}
