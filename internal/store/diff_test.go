package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/win32"
)

func diffFixture(t *testing.T, specs ...mounts.Spec) (*Store, *ContainerRecord) {
	t.Helper()
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.CreateContainer("diff-example", ContainerRecord{HostConfig: ContainerHostConfig{Mounts: specs}})
	if err != nil {
		t.Fatal(err)
	}
	return st, c
}

func TestContainerDiffBaselineAndResolution(t *testing.T) {
	st, c := diffFixture(t)
	if paths, err := st.ContainerDiff(c.ID); err != nil || len(paths) != 0 {
		t.Fatalf("empty data: %v %v", paths, err)
	}
	for _, dir := range []string{"home/nested", "empty", "Mixed Case"} {
		if err := os.MkdirAll(filepath.Join(c.DataDir, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(c.DataDir, "home", "nested", "example.txt")
	for _, path := range []string{file, filepath.Join(c.DataDir, "Mixed Case", "日本語.txt"), c.LogPath} {
		if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"/Mixed Case", "/Mixed Case/日本語.txt", "/empty", "/home", "/home/nested", "/home/nested/example.txt"}
	for _, target := range []string{c.ID, c.ShortID(), c.BareName(), "/DIFF-EXAMPLE"} {
		if paths, err := st.ContainerDiff(target); err != nil || !reflect.DeepEqual(paths, want) {
			t.Fatalf("diff %s: %v %v, want %v", target, paths, err, want)
		}
	}
	if err := os.WriteFile(file, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Attribute enumeration must work even if the application denies read access
	// to its open file through Windows file sharing.
	h, err := win32.CreateFile(file, win32.GENERIC_WRITE, 0, nil, win32.OPEN_EXISTING, win32.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	paths, diffErr := st.ContainerDiff(c.ID)
	win32.CloseHandle(h)
	if diffErr != nil || !reflect.DeepEqual(paths, want) {
		t.Fatalf("edited and locked file: %v %v", paths, diffErr)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if paths, err := st.ContainerDiff(c.ID); err != nil || !reflect.DeepEqual(paths, want[:len(want)-1]) {
		t.Fatalf("deleted addition: %v %v", paths, err)
	}
	if _, err := st.ContainerDiff("missing"); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("unknown container: %v", err)
	}
	// Create a second stored record with a deliberately shared ID prefix.
	other := *c
	other.ID = c.ID[:12] + strings.Repeat("0", 52)
	if other.ID == c.ID {
		other.ID = c.ID[:12] + strings.Repeat("1", 52)
	}
	other.Name = "/second"
	if err := os.MkdirAll(st.ContainerDataDir(other.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := st.writeContainerStateNoLock(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ContainerDiff(c.ShortID()); err == nil || !strings.Contains(err.Error(), "multiple containers") {
		t.Fatalf("ambiguous prefix: %v", err)
	}
}

func TestContainerDiffMounts(t *testing.T) {
	for _, kind := range []string{"volume", "bind"} {
		for _, target := range []string{"${data}", "${data}/parent/Work"} {
			t.Run(kind+"/"+target, func(t *testing.T) {
				source := "diff-volume"
				if kind == "bind" {
					source = t.TempDir()
				}
				st, c := diffFixture(t, mounts.Spec{Type: kind, Source: source, Target: target})
				m := c.Mounts[0]
				if err := os.WriteFile(filepath.Join(m.Source, "outside.txt"), []byte("mounted"), 0o644); err != nil {
					t.Fatal(err)
				}
				want := []string{}
				if target != "${data}" {
					if err := os.MkdirAll(filepath.Join(c.DataDir, "parent", "workspace"), 0o755); err != nil {
						t.Fatal(err)
					}
					want = []string{"/parent", "/parent/workspace"}
					// The configured spelling differs from the actual directory.
					m.Destination = filepath.Join(c.DataDir, "parent", "work")
				}
				for _, attached := range []bool{false, true, false} {
					if attached {
						if err := mounts.Attach(c.DataDir, m); err != nil {
							t.Fatal(err)
						}
					} else if err := mounts.Detach(c.DataDir, m); err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(m.Destination, 0o755); err != nil {
						t.Fatal(err)
					}
					if paths, err := st.ContainerDiff(c.ID); err != nil || !reflect.DeepEqual(paths, want) {
						t.Fatalf("attached=%t: %v %v, want %v", attached, paths, err, want)
					}
				}
				if data, err := os.ReadFile(filepath.Join(m.Source, "outside.txt")); err != nil || string(data) != "mounted" {
					t.Fatalf("diff touched mounted contents: %s %v", data, err)
				}
			})
		}
	}
}

func TestContainerDiffReparseAndMissingRoot(t *testing.T) {
	st, c := diffFixture(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "outside.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := mounts.Mount{Source: source, Destination: filepath.Join(c.DataDir, "link")}
	if err := mounts.Attach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	if paths, err := st.ContainerDiff(c.ID); err != nil || !reflect.DeepEqual(paths, []string{"/link"}) {
		t.Fatalf("junction target enumerated: %v %v", paths, err)
	}
	if err := mounts.Detach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	link.Destination = c.DataDir
	if err := mounts.Attach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	if paths, err := st.ContainerDiff(c.ID); err == nil || !strings.Contains(err.Error(), "reparse point") || paths != nil {
		t.Fatalf("unexpected root junction accepted: %v %v", paths, err)
	}
	if err := mounts.Detach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ContainerDiff(c.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root: %v", err)
	}
	if err := os.WriteFile(c.DataDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ContainerDiff(c.ID); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("regular file data root accepted: %v", err)
	}
}

func TestContainerDiffStates(t *testing.T) {
	for _, status := range []string{"created", "running", "paused", "exited"} {
		t.Run(status, func(t *testing.T) {
			st, c := diffFixture(t)
			if status == "running" || status == "paused" {
				lk, err := OpenLock(st.ContainerRunLockPath(c.ID), true, false)
				if err != nil {
					t.Fatal(err)
				}
				defer lk.Release()
			}
			if _, err := st.UpdateContainer(c.ID, func(c *ContainerRecord) error {
				c.State.Status = status
				c.State.Running = status == "running" || status == "paused"
				c.State.Paused = status == "paused"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(st.ContainerStatePath(c.ID))
			if err != nil {
				t.Fatal(err)
			}
			if paths, err := st.ContainerDiff(c.ID); err != nil || len(paths) != 0 {
				t.Fatalf("%s: %v %v", status, paths, err)
			}
			after, err := os.ReadFile(st.ContainerStatePath(c.ID))
			if err != nil || string(before) != string(after) {
				t.Fatalf("diff changed state: %v", err)
			}
		})
	}
}

func TestContainerDiffConcurrentWrites(t *testing.T) {
	st, c := diffFixture(t)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			path := filepath.Join(c.DataDir, fmt.Sprint(i%8))
			_ = os.Mkdir(path, 0o755)
			_ = os.WriteFile(filepath.Join(path, "file"), nil, 0o644)
			_ = os.Remove(filepath.Join(path, "file"))
			_ = os.Remove(path)
		}
	}()
	defer func() { close(done); wg.Wait() }()
	for i := 0; i < 30; i++ {
		if _, err := st.ContainerDiff(c.ID); err != nil {
			t.Fatalf("disappearing descendant failed diff: %v", err)
		}
	}
}

func TestContainerDiffOperationLock(t *testing.T) {
	st, c := diffFixture(t)
	op, err := st.LockContainer(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Release()
	done := make(chan error, 1)
	go func() { _, err := st.ContainerDiff(c.ID); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("diff bypassed operation lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	// Removal wins while diff waits; diff must reread the record after locking.
	if err := op.Remove(); err != nil {
		t.Fatal(err)
	}
	op.Release()
	select {
	case err := <-done:
		if !errors.Is(err, ErrContainerNotFound) {
			t.Fatalf("diff used stale record after removal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("diff did not release operation lock")
	}
}
