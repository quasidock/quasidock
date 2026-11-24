package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestContainerSizePrivateData(t *testing.T) {
	st, c := diffFixture(t)
	if size, err := st.ContainerSize(c.ID); err != nil || size != 0 {
		t.Fatalf("empty data: %d %v", size, err)
	}
	if err := os.MkdirAll(filepath.Join(c.DataDir, "nested", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		filepath.Join(c.DataDir, "one"):           "one",
		filepath.Join(c.DataDir, "nested", "two"): "twotwo",
		c.LogPath: "this log is excluded",
	} {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h, err := win32.CreateFile(filepath.Join(c.DataDir, "one"), win32.GENERIC_WRITE, 0, nil, win32.OPEN_EXISTING, win32.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(h)
	for _, target := range []string{c.ID, c.ShortID(), "/DIFF-EXAMPLE"} {
		if size, err := st.ContainerSize(target); err != nil || size != 9 {
			t.Fatalf("%s: %d %v, want 9", target, size, err)
		}
	}
	if _, err := st.ContainerSize("missing"); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("missing container: %v", err)
	}
}

func TestContainerSizeMounts(t *testing.T) {
	for _, kind := range []string{"volume", "bind"} {
		for _, target := range []string{"${data}", "${data}/parent/Work"} {
			t.Run(kind+"/"+target, func(t *testing.T) {
				source := "size-volume"
				if kind == "bind" {
					source = t.TempDir()
				}
				st, c := diffFixture(t, mounts.Spec{Type: kind, Source: source, Target: target})
				m := c.Mounts[0]
				if err := os.WriteFile(filepath.Join(m.Source, "outside"), []byte("mounted data"), 0o644); err != nil {
					t.Fatal(err)
				}
				var want uint64
				if target != "${data}" {
					if err := os.WriteFile(filepath.Join(c.DataDir, "inside"), []byte("inside"), 0o644); err != nil {
						t.Fatal(err)
					}
					want = 6
					m.Destination = filepath.Join(c.DataDir, "parent", "work")
				}
				if err := mounts.Attach(c.DataDir, m); err != nil {
					t.Fatal(err)
				}
				if size, err := st.ContainerSize(c.ID); err != nil || size != want {
					t.Fatalf("attached: %d %v, want %d", size, err, want)
				}
				if err := mounts.Detach(c.DataDir, m); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(m.Destination, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(m.Destination, "masked"), []byte("excluded even when detached"), 0o644); err != nil {
					t.Fatal(err)
				}
				if size, err := st.ContainerSize(c.ID); err != nil || size != want {
					t.Fatalf("detached: %d %v, want %d", size, err, want)
				}
			})
		}
	}
}

func TestContainerSizeSeededData(t *testing.T) {
	st, image := seededStore(t)
	c, err := st.CreateContainer("seeded-size", ContainerRecord{Image: image}, st.SeedContainerData)
	if err != nil {
		t.Fatal(err)
	}
	if size, err := st.ContainerSize(c.ID); err != nil || size != 17 {
		t.Fatalf("copied seed: %d %v, want 17", size, err)
	}
	if err := os.WriteFile(filepath.Join(c.DataDir, "added"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if size, err := st.ContainerSize(c.ID); err != nil || size != 20 {
		t.Fatalf("seed plus writes: %d %v, want 20", size, err)
	}
}

func TestContainerSizeReparseAndScanErrors(t *testing.T) {
	st, c := diffFixture(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "outside"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := mounts.Mount{Source: source, Destination: filepath.Join(c.DataDir, "link")}
	if err := mounts.Attach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	if size, err := st.ContainerSize(c.ID); err != nil || size != 0 {
		t.Fatalf("junction target counted: %d %v", size, err)
	}
	if err := mounts.Detach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(c.DataDir, "blocked")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := win32.CreateFile(blocked, win32.GENERIC_READ, 0, nil, win32.OPEN_EXISTING, win32.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	size, scanErr := st.ContainerSize(c.ID)
	win32.CloseHandle(h)
	if scanErr == nil || size != 0 {
		t.Fatalf("sharing error ignored: %d %v", size, scanErr)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	link.Destination = c.DataDir
	if err := mounts.Attach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ContainerSize(c.ID); err == nil || !strings.Contains(err.Error(), "reparse point") {
		t.Fatalf("unexpected root junction accepted: %v", err)
	}
	if err := mounts.Detach(c.DataDir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ContainerSize(c.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root: %v", err)
	}
}

func TestContainerSizeStatesAndOperationLock(t *testing.T) {
	for _, status := range []string{"created", "running", "paused", "exited"} {
		t.Run(status, func(t *testing.T) {
			st, c := diffFixture(t)
			if status == "running" || status == "paused" {
				lock, err := OpenLock(st.ContainerRunLockPath(c.ID), true, false)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Release()
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
			op, err := st.LockContainer(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer op.Release()
			done := make(chan error, 1)
			go func() { _, err := st.ContainerSize(c.ID); done <- err }()
			select {
			case err := <-done:
				t.Fatalf("bypassed operation lock: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			op.Release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("size did not resume after operation finished")
			}
			after, err := os.ReadFile(st.ContainerStatePath(c.ID))
			if err != nil || string(before) != string(after) {
				t.Fatalf("size changed state: %v", err)
			}
		})
	}
}

func TestContainerSizeConcurrentWrites(t *testing.T) {
	st, c := diffFixture(t)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			dir := filepath.Join(c.DataDir, "changing")
			_ = os.Mkdir(dir, 0o755)
			_ = os.WriteFile(filepath.Join(dir, "file"), []byte("live"), 0o644)
			_ = os.Remove(filepath.Join(dir, "file"))
			_ = os.Remove(dir)
		}
	}()
	defer func() { close(done); wg.Wait() }()
	for i := 0; i < 30; i++ {
		if _, err := st.ContainerSize(c.ID); err != nil {
			t.Fatalf("disappearing descendant failed size scan: %v", err)
		}
	}
}
