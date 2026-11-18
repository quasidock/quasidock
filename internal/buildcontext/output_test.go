package buildcontext

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadOutputIgnoresIgnoreFilesAndKeepsEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	put(t, root, ".dockerignore", "*.txt\nempty/\n")
	put(t, root, ".gitignore", "*.txt\n")
	put(t, root, "data.txt", "keep")
	if err := os.Mkdir(filepath.Join(root, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	output, err := ReadOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	if !output.IsFile("data.txt") || !output.IsFile(".dockerignore") {
		t.Fatal("output was filtered by an ignore file")
	}
	found := false
	for _, node := range output.Sorted() {
		found = found || node.Path == "empty"
	}
	if !found {
		t.Fatal("empty output directory was lost")
	}
}

func TestReadOutputRejectsJunctionEvenWhenIgnored(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	put(t, root, ".dockerignore", "link\n")
	put(t, target, "outside.txt", "outside")
	junction := filepath.Join(root, "link")
	if output, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("creating junction: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = os.Remove(junction) })
	if _, err := ReadOutput(root); err == nil || !strings.Contains(err.Error(), "link or junction") {
		t.Fatalf("invalid output junction was accepted: %v", err)
	}
}
