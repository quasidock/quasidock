package tree

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func executableFixture(t *testing.T) *Tree {
	t.Helper()
	root := t.TempDir()
	source := New()
	for name, content := range map[string]string{"out/serv1.exe": "one", "out/serv2.exe": "two", "out/data.txt": "data"} {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := source.AddFile(name, Content{Kind: ContentFile, FilePath: file, FileSize: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.AddDir("out/empty"); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestCopyExecutablesUsesLogicalSelectorsAndNativeDestinations(t *testing.T) {
	for _, tc := range []struct {
		name, dest string
		paths      []string
		files      map[string]string
		mappings   map[string]string
	}{
		{"rename", "app/server", []string{"out/serv1"}, map[string]string{"app/server.exe": "one"}, map[string]string{"app/server": "app/server.exe"}},
		{"glob", "app/", []string{"out/serv?"}, map[string]string{"app/serv1.exe": "one", "app/serv2.exe": "two"}, map[string]string{"app/serv1": "app/serv1.exe", "app/serv2": "app/serv2.exe"}},
		{"directory", "app", []string{"out"}, map[string]string{"app/serv1.exe": "one", "app/serv2.exe": "two", "app/data.txt": "data"}, map[string]string{"app/serv1": "app/serv1.exe", "app/serv2": "app/serv2.exe"}},
		{"multiple", "app/", []string{"out/serv1", "out/data.txt"}, map[string]string{"app/serv1.exe": "one", "app/data.txt": "data"}, map[string]string{"app/serv1": "app/serv1.exe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := executableFixture(t)
			copied, mappings, err := source.CopyExecutables(tc.paths, tc.dest, false, []string{"out/serv1", "out/serv2"})
			if err != nil || !reflect.DeepEqual(mappings, tc.mappings) {
				t.Fatalf("mappings %v: %v", mappings, err)
			}
			for name, expected := range tc.files {
				actual, exists := copied.nodes[strings.ToLower(name)]
				if !exists {
					t.Fatalf("missing %s in %v", name, copied.Sorted())
				}
				data, err := os.ReadFile(actual.Content.FilePath)
				if err != nil || string(data) != expected {
					t.Fatalf("incorrect bytes at %s: %q %v", name, data, err)
				}
			}
			if !source.IsFile("out/serv1.exe") || source.IsFile("out/serv1") {
				t.Fatal("source tree was modified")
			}
		})
	}
}

func TestExecutableViewsRejectAmbiguityAndHideStorageNames(t *testing.T) {
	source := executableFixture(t)
	if _, _, err := source.CopyExecutables([]string{"out/serv1.exe"}, ".", true, []string{"out/serv1"}); err == nil {
		t.Fatal("storage filename was exposed as a second logical source")
	}
	if err := source.AddFile("out/serv1", Content{Kind: ContentFile}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ExecutableView([]string{"out/serv1"}); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("literal/executable collision was accepted: %v", err)
	}
	if _, err := executableFixture(t).ExecutableView([]string{"out/serv1", "out/serv1.exe"}); err == nil {
		t.Fatal("two logical outputs mapped onto one storage filename")
	}
}

func TestIncomingExecutableCollisionChecks(t *testing.T) {
	for _, name := range []string{"app/server", "app/server.exe"} {
		incoming := New()
		if err := incoming.AddFile(name, Content{Kind: ContentFile}); err != nil {
			t.Fatal(err)
		}
		if err := incoming.CheckExecutableCollisions([]string{"app/server"}, nil); err == nil {
			t.Fatal("literal copy occupied a translated executable:", name)
		}
		if name == "app/server.exe" {
			if err := incoming.CheckExecutableCollisions([]string{"app/server"}, map[string]string{"app/server": "app/server.exe"}); err != nil {
				t.Fatal(err)
			}
			if err := incoming.CheckExecutableCollisions([]string{"app/server"}, map[string]string{"app/server.exe": "app/server.exe"}); err == nil {
				t.Fatal("a distinct logical output overwrote a mapped executable")
			}
		}
	}
}
