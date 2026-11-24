package store

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestListContainersCreationOrder(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	var records []*ContainerRecord
	for i, created := range []string{
		"2026-01-01T00:00:00Z", "2026-01-01T00:00:00.1Z",
		"2026-01-01T07:00:00.1+07:00", "2026-01-01T00:00:01Z", "invalid",
	} {
		c, err := st.CreateContainer("order-"+string(rune('a'+i)), ContainerRecord{Created: created})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, c)
	}
	first, second := records[1], records[2]
	if first.ID > second.ID {
		first, second = second, first
	}
	want := []*ContainerRecord{records[3], first, second, records[0], records[4]}
	got, err := st.ListContainers(true)
	if err != nil || len(got) != len(want) {
		t.Fatalf("list: %+v %v", got, err)
	}
	for i, c := range got {
		if c.ID != want[i].ID {
			t.Errorf("position %d: %s, want %s", i, c.Name, want[i].Name)
		}
	}
}

func TestPausedContainerReconciliation(t *testing.T) {
	for _, autoRemove := range []bool{false, true} {
		st, err := Open(t.TempDir(), false, "")
		if err != nil {
			t.Fatal(err)
		}
		rec, err := st.CreateContainer("paused", ContainerRecord{
			HostConfig: ContainerHostConfig{AutoRemove: autoRemove},
			State:      ContainerState{Status: "paused", Running: true, Paused: true, Launched: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		lk, err := OpenLock(st.ContainerRunLockPath(rec.ID), true, false)
		if err != nil {
			t.Fatal(err)
		}
		defer lk.Release()
		listed, err := st.ListContainers(false)
		if err != nil || len(listed) != 1 || !listed[0].State.Paused {
			t.Fatalf("paused container missing from ps: %+v %v", listed, err)
		}
		for _, dry := range []bool{true, false} {
			removed, _, active, err := st.PruneContainers(dry)
			if err != nil || len(removed) != 0 || active != 1 {
				t.Fatalf("prune touched paused container: %v %d %v", removed, active, err)
			}
		}
		lk.Release()
		current, err := st.ReadContainer(rec.ID)
		if autoRemove {
			if !errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("dead paused --rm container retained: %v", err)
			}
		} else if err != nil || current.State.Status != "exited" || current.State.Running || current.State.Paused {
			t.Fatalf("dead paused state was not reconciled: %+v %v", current, err)
		}
	}
}

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

func TestPruneCreatedContainers(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, runID string
		autoRemove  bool
		age         time.Duration
	}{
		{name: "unstarted"},
		{name: "unstarted-auto-remove", autoRemove: true},
		{name: "launching", runID: "launch-token"},
		{name: "abandoned-launch", runID: "old-token", age: time.Minute},
	} {
		_, err := st.CreateContainer(tc.name, ContainerRecord{
			Created:    time.Now().Add(-tc.age).UTC().Format(time.RFC3339Nano),
			HostConfig: ContainerHostConfig{AutoRemove: tc.autoRemove},
			State:      ContainerState{Status: "created", RunID: tc.runID},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, dry := range []bool{true, false} {
		removed, _, protected, err := st.PruneContainers(dry)
		if err != nil || len(removed) != 3 || protected != 1 {
			t.Fatalf("prune dry=%t: removed=%v protected=%d err=%v", dry, removed, protected, err)
		}
		for _, c := range removed {
			if c.BareName() == "launching" {
				t.Fatal("prune bypassed launch grace period")
			}
		}
		all, err := st.ListContainers(true)
		want := 1
		if dry {
			want = 4
		}
		if err != nil || len(all) != want {
			t.Fatalf("prune dry=%t left %d containers, want %d: %v", dry, len(all), want, err)
		}
	}
}
