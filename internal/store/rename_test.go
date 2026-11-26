package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestContainerRename(t *testing.T) {
	s, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateContainer("original", ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.DataDir, "keep"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateContainer("occupied", ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.LockContainer(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Release()
	for _, name := range []string{"", "bad/name", "_bad"} {
		if _, err := op.Rename(name); !errors.Is(err, ErrInvalidContainerName) {
			t.Fatalf("%q: %v", name, err)
		}
	}
	if _, err := op.Rename("OCCUPIED"); !errors.Is(err, ErrContainerNameInUse) {
		t.Fatal(err)
	}
	for _, name := range []string{"renamed", "Renamed", "/Renamed"} {
		got, err := op.Rename(name)
		if err != nil || got.ID != a.ID || got.DataDir != a.DataDir || got.LogPath != a.LogPath {
			t.Fatalf("rename: %+v %v", got, err)
		}
	}
	if _, err := s.ResolveContainer("original"); !errors.Is(err, ErrContainerNotFound) {
		t.Fatal(err)
	}
	if got, err := s.ResolveContainer("renamed"); err != nil || got.ID != a.ID {
		t.Fatalf("resolve: %+v %v", got, err)
	}
	if data, err := os.ReadFile(filepath.Join(a.DataDir, "keep")); err != nil || string(data) != "data" {
		t.Fatalf("data lost: %q %v", data, err)
	}
	if _, err := s.CreateContainer("original", ContainerRecord{}); err != nil {
		t.Fatal(err)
	}
	events, err := s.ReadEventSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range events.Events {
		e := entry.Event
		if e.Action == "rename" {
			count++
			if e.Actor.ID != a.ID || e.Actor.Attributes["oldName"] == "" {
				t.Fatalf("bad rename event: %+v", e)
			}
		}
	}
	if count != 2 {
		t.Fatalf("rename events: %d", count)
	}
	if got, err := s.ReadContainer(b.ID); err != nil || got.BareName() != "occupied" {
		t.Fatalf("collision changed other container: %+v %v", got, err)
	}
}

func TestContainerRenameConcurrentCreation(t *testing.T) {
	s, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateContainer("before", ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.LockContainer(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Release()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Go(func() { _, err := op.Rename("contended"); errs <- err })
	wg.Go(func() { _, err := s.CreateContainer("CONTENDED", ContainerRecord{}); errs <- err })
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrContainerNameInUse) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("successful name claims: %d", success)
	}
}
