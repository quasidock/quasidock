package store

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestContainerOperationProtectsRestart(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.CreateContainer("restarting", ContainerRecord{
		Created:    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		HostConfig: ContainerHostConfig{AutoRemove: true},
		State:      ContainerState{Status: "running", Running: true, Launched: true, RunID: "old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	op, err := st.LockContainer(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Release()
	if _, err := st.ReadContainer(rec.ID); err != nil {
		t.Fatalf("reconciliation removed a reserved restart: %v", err)
	}
	for _, dry := range []bool{true, false} {
		removed, _, _, err := st.PruneContainers(dry)
		if err != nil || len(removed) != 0 {
			t.Fatalf("prune during operation: %v %v", removed, err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- st.RemoveContainer(rec.ID) }()
	select {
	case err := <-done:
		t.Fatalf("removal bypassed operation lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	op.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("removal did not resume after operation finished")
	}
	if _, err := os.Stat(st.ContainerDir(rec.ID)); !os.IsNotExist(err) {
		t.Fatalf("container directory remains: %v", err)
	}
}

func TestAutoRemoveAfterAbandonedRestartReservation(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.CreateContainer("abandoned", ContainerRecord{
		HostConfig: ContainerHostConfig{AutoRemove: true},
		State:      ContainerState{Status: "running", Running: true, Launched: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	op, err := st.LockContainer(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Release()
	if _, err := st.ReadContainer(rec.ID); err != nil {
		t.Fatal(err)
	}
	// Simulate the command dying after the old supervisor has exited, without
	// either process remaining to perform its ordinary exit cleanup.
	op.Release()
	if _, err := st.ReadContainer(rec.ID); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("abandoned reservation disabled automatic removal: %v", err)
	}
}
