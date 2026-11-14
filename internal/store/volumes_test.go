package store

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/quasidock/quasidock/internal/mounts"
)

func TestVolumesPinningAndDryRun(t *testing.T) {
	s, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.CreateVolume("Work")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.CreateVolume("work")
	if err != nil || *v2 != *v {
		t.Fatalf("non-idempotent create: %+v %v", v2, err)
	}
	if err := os.WriteFile(filepath.Join(v.Mountpoint, "sentinel"), []byte("survive"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateContainer("writer", ContainerRecord{HostConfig: ContainerHostConfig{Mounts: []mounts.Spec{{Type: "volume", Source: "WORK", Target: "${data}"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Mounts) != 1 || c.Mounts[0].Name != "Work" || c.Mounts[0].Source != v.Mountpoint {
		t.Fatalf("resolved mounts: %+v", c.Mounts)
	}
	if err := s.RemoveVolume("work"); err == nil {
		t.Fatal("removed referenced volume")
	}
	if _, err := s.UpdateContainer(c.ID, func(c *ContainerRecord) error { c.State.Running = true; c.HostConfig.AutoRemove = true; return nil }); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.ContainerStatePath(c.ID))
	if all, _, err := s.PruneVolumes(true); err != nil || len(all) != 0 {
		t.Fatalf("dry run unpinned stale record: %v %v", all, err)
	}
	after, _ := os.ReadFile(s.ContainerStatePath(c.ID))
	if !bytes.Equal(before, after) {
		t.Fatal("dry run mutated state")
	}
	// Real reconciliation removes the stale --rm container, never its volume.
	if _, err := s.ListContainers(true); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(v.Mountpoint, "sentinel")); err != nil || string(b) != "survive" {
		t.Fatal("container reconciliation deleted data")
	}
	if all, _, err := s.PruneVolumes(true); err != nil || len(all) != 1 {
		t.Fatalf("dry prune: %v %v", all, err)
	}
	if _, err := s.ReadVolume("work"); err != nil {
		t.Fatal("dry run removed volume")
	}
	if all, _, err := s.PruneVolumes(false); err != nil || len(all) != 1 {
		t.Fatalf("prune: %v %v", all, err)
	}
}

func TestVolumeRunRemovalRace(t *testing.T) {
	s, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err := s.CreateVolume("race"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		var c *ContainerRecord
		var createErr error
		go func() {
			defer wg.Done()
			c, createErr = s.CreateContainer("", ContainerRecord{HostConfig: ContainerHostConfig{Mounts: []mounts.Spec{{Type: "volume", Source: "race", Target: "${data}"}}}})
		}()
		go func() { defer wg.Done(); _ = s.RemoveVolume("race") }()
		wg.Wait()
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, err := os.Stat(c.Mounts[0].Source); err != nil {
			t.Fatalf("container references deleted volume: %v", err)
		}
		if err := s.RemoveContainer(c.ID); err != nil {
			t.Fatal(err)
		}
	}
}
