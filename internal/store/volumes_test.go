package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/quasidock/quasidock/internal/mounts"
)

func TestAnonymousVolumeCleanupContinuesAfterFailure(t *testing.T) {
	s, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateContainer("owner", ContainerRecord{HostConfig: ContainerHostConfig{Mounts: []mounts.Spec{
		{Type: "volume", Anonymous: true, Target: "${data}/failed"},
		{Type: "volume", Anonymous: true, Target: "${data}/protected"},
		{Type: "volume", Anonymous: true, Target: "${data}/removed"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.LockContainer(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Release()
	failure := errors.New("simulated volume guard error")
	err = op.RemoveWithVolumes(func(path string) (bool, error) {
		switch path {
		case rec.Mounts[0].Source:
			return false, failure
		case rec.Mounts[1].Source:
			return true, nil
		default:
			return false, nil
		}
	})
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), rec.Mounts[0].Name) {
		t.Fatalf("missing partial cleanup error: %v", err)
	}
	if _, err := s.ReadContainer(rec.ID); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("container survived volume cleanup error: %v", err)
	}
	for _, m := range rec.Mounts[:2] {
		if _, err := s.ReadVolume(m.Name); err != nil {
			t.Fatalf("volume %s was not retained: %v", m.Name, err)
		}
	}
	if _, err := s.ReadVolume(rec.Mounts[2].Name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("independent volume deletion did not proceed: %v", err)
	}
	if err := s.RemoveVolume(rec.Mounts[0].Name); err != nil {
		t.Fatalf("failed cleanup could not be retried by name: %v", err)
	}
}

func TestAnonymousVolumeAllocationRollbackAndConcurrentPin(t *testing.T) {
	s, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	owner := ContainerRecord{HostConfig: ContainerHostConfig{Mounts: []mounts.Spec{
		{Type: "volume", Anonymous: true, Target: "${data}"},
	}}}
	abort := errors.New("initialize failed")
	if _, err := s.CreateContainer("failed", owner, func(*ContainerRecord) error { return abort }); !errors.Is(err, abort) {
		t.Fatalf("unexpected creation error: %v", err)
	}
	if all, err := s.ListVolumes(); err != nil || len(all) != 0 {
		t.Fatalf("failed creation left volumes: %v %v", all, err)
	}
	rec, err := s.CreateContainer("owner", owner)
	if err != nil {
		t.Fatal(err)
	}
	mount := rec.Mounts[0]
	file := filepath.Join(mount.Source, "sentinel")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	op, err := s.LockContainer(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Release()
	pinning, finishPin := make(chan struct{}), make(chan struct{})
	pinDone, removeDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := s.CreateContainer("consumer", ContainerRecord{HostConfig: ContainerHostConfig{
			Mounts: []mounts.Spec{{Type: "volume", Source: mount.Name, Target: "${data}"}},
		}}, func(*ContainerRecord) error {
			close(pinning)
			<-finishPin
			return nil
		})
		pinDone <- err
	}()
	select {
	case <-pinning:
	case err := <-pinDone:
		t.Fatalf("failed to register pin: %v", err)
	}
	go func() { removeDone <- op.RemoveWithVolumes(func(string) (bool, error) { return false, nil }) }()
	close(finishPin)
	if err := <-pinDone; err != nil {
		t.Fatal(err)
	}
	if err := <-removeDone; err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(file); err != nil || string(contents) != "keep" {
		t.Fatalf("cleanup deleted concurrently pinned data: %s %v", contents, err)
	}
}

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
