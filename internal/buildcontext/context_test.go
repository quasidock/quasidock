package buildcontext

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func put(t *testing.T, root, name, value string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestIgnorePrecedenceNegationAndGitIndependence(t *testing.T) {
	root := t.TempDir()
	for name, value := range map[string]string{".gitignore": "live.txt", ".dockerignore": "live.txt", "docker/Dockerfile.dockerignore": "ignored/**\n!ignored/keep.txt\n", "live.txt": "live", "ignored/drop.txt": "no", "ignored/keep.txt": "yes", "quasidock.json": "recipe"} {
		put(t, root, name, value)
	}
	s, err := Read(root, "docker/Dockerfile", filepath.Join(root, "quasidock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Tree.IsFile("live.txt") || !s.Tree.IsFile("ignored/keep.txt") || s.Tree.IsFile("ignored/drop.txt") || s.Tree.IsFile("quasidock.json") {
		t.Fatal(s.Tree.Sorted())
	}
	if err := s.Recheck(); err != nil {
		t.Fatal(err)
	}
	put(t, root, "new.txt", "added")
	if err := s.Recheck(); err == nil {
		t.Fatal("did not detect new context file")
	}
}
func TestCopySemantics(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"package.json", "package-lock.json", "src/main.go", "src/nested/data.txt", "archive.zip"} {
		put(t, root, name, name)
	}
	s, err := Read(root, "Dockerfile", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		paths     []string
		dest      string
		directory bool
		files     []string
	}{
		{[]string{"package*.json"}, "app/", false, []string{"app/package.json", "app/package-lock.json"}},
		{[]string{"src"}, "app", false, []string{"app/main.go", "app/nested/data.txt"}},
		{[]string{"archive.zip"}, "renamed.bin", false, []string{"renamed.bin"}},
		{[]string{"package.json"}, "app", true, []string{"app/package.json"}},
		{[]string{"."}, "app/", false, []string{"app/src/main.go", "app/archive.zip"}},
	} {
		out, err := s.Copy(tc.paths, tc.dest, tc.directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range tc.files {
			if !out.IsFile(name) {
				t.Fatalf("missing %s in %+v", name, out.Sorted())
			}
		}
	}
	if _, err := s.Copy([]string{"package*.json"}, "app", false); err == nil {
		t.Fatal("accepted multiple files into non-directory")
	}
	if _, err := s.Copy([]string{"missing"}, "app/", false); err == nil {
		t.Fatal("accepted missing source")
	}
}
func TestContextChangesAndLinks(t *testing.T) {
	root := t.TempDir()
	put(t, root, "data.txt", "one")
	s, err := Read(root, "Dockerfile", "")
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "data.txt", "two")
	if err := s.Recheck(); err == nil {
		t.Fatal("missed same-size edit")
	}
	s, err = Read(root, "Dockerfile", "")
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, ".dockerignore", "data.txt")
	if err := s.Recheck(); err == nil {
		t.Fatal("missed ignore edit")
	}
	for _, name := range []string{"CON", "nul.txt", "COM1", "LPT²", "bad.", "bad ", "a:b", "a\x01"} {
		if ValidName(name) {
			t.Fatalf("accepted %q", name)
		}
	}
	if err := os.Symlink(filepath.Join(root, "data.txt"), filepath.Join(root, "link.txt")); err == nil {
		if _, err := Read(root, "Dockerfile", ""); err == nil || !strings.Contains(err.Error(), "link") {
			t.Fatal("accepted link")
		}
	}
}

func TestContextHashIsIndependentOfLocation(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, root := range []string{a, b} {
		put(t, root, "file.txt", "same")
		put(t, root, ".dockerignore", "*.tmp\n")
	}
	x, err := Read(a, "Dockerfile", "")
	if err != nil {
		t.Fatal(err)
	}
	y, err := Read(b, "Dockerfile", "")
	if err != nil {
		t.Fatal(err)
	}
	if x.Hash != y.Hash {
		t.Fatal("absolute context path changed content identity")
	}
}
