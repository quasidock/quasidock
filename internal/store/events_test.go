package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func eventActions(t *testing.T, st *Store) []string {
	t.Helper()
	snapshot, err := st.ReadEventSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, entry := range snapshot.Events {
		actions = append(actions, entry.Event.Action)
	}
	return actions
}

func TestEventJournalConcurrentProcesses(t *testing.T) {
	if root := os.Getenv("QUASIDOCK_TEST_EVENT_WRITER"); root != "" {
		st, err := Open(root, false, "")
		if err != nil {
			t.Fatal(err)
		}
		for i := range 12 {
			if err := st.AppendEvent(Event{Type: "container", Action: "create", Actor: EventActor{ID: fmt.Sprintf("%d-%d", os.Getpid(), i)}}); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 3)
	for range 3 {
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestEventJournalConcurrentProcesses$")
		cmd.Env = append(os.Environ(), "QUASIDOCK_TEST_EVENT_WRITER="+st.Root)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() {
			err := cmd.Wait()
			if err != nil {
				err = fmt.Errorf("writer: %w: %s", err, output.String())
			}
			done <- err
		}()
	}
	var last uint64
	finished := 0
	for finished < 3 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			finished++
		case <-time.After(5 * time.Millisecond):
			snapshot, err := st.ReadEventSnapshot(ctx)
			if err != nil || snapshot.LastSequence < last {
				t.Fatalf("non-atomic publication: %+v %v", snapshot, err)
			}
			last = snapshot.LastSequence
		}
	}
	snapshot, err := st.ReadEventSnapshot(ctx)
	if err != nil || snapshot.LastSequence != 36 || len(snapshot.Events) != 36 {
		t.Fatalf("lost concurrent events: %+v %v", snapshot, err)
	}
	ids := make(map[string]bool)
	for _, entry := range snapshot.Events {
		if ids[entry.Event.Actor.ID] {
			t.Fatalf("duplicate event: %+v", entry)
		}
		ids[entry.Event.Actor.ID] = true
	}
}

func TestEventJournalRetentionAndReopen(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	for i := range EventHistoryLimit + 7 {
		if err := st.AppendEvent(Event{Type: "container", Action: strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := Open(st.Root, false, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reopened.ReadEventSnapshot(context.Background())
	if err != nil || snapshot.LastSequence != EventHistoryLimit+7 || len(snapshot.Events) != EventHistoryLimit {
		t.Fatalf("retention: %+v %v", snapshot, err)
	}
	if snapshot.Events[0].Sequence != 8 || snapshot.Events[0].Event.Action != "7" {
		t.Fatalf("wrong retained boundary: %+v", snapshot.Events[0])
	}
	for _, entry := range snapshot.Events {
		event := entry.Event
		if event.Time != time.Unix(0, event.TimeNano).Unix() || event.TimeNano < before.UnixNano() || event.TimeNano > time.Now().UnixNano() {
			t.Fatalf("bad timestamp: %+v", event)
		}
	}
}

func TestEventJournalFailures(t *testing.T) {
	for _, data := range []string{"{", `{"version":2}`, `{"version":1,"lastSequence":1,"events":[]}`, `{"version":1,"lastSequence":1,"events":[{"sequence":2}]}`} {
		t.Run(data, func(t *testing.T) {
			st, err := Open(t.TempDir(), false, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(st.Path("events.json"), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			var warnings bytes.Buffer
			st.EventWarnings = &warnings
			if _, err := st.ReadEventSnapshot(context.Background()); err == nil {
				t.Fatal("accepted corrupt journal")
			}
			if err := st.AppendEvent(Event{}); err == nil {
				t.Fatal("overwrote corrupt journal")
			}
			rec, err := st.CreateContainer("still-created", ContainerRecord{})
			if err != nil {
				t.Fatalf("journal failure blocked creation: %v", err)
			}
			if err := st.RemoveContainer(rec.ID); err != nil {
				t.Fatalf("journal failure blocked removal: %v", err)
			}
			if !strings.Contains(warnings.String(), "recording container create") || !strings.Contains(warnings.String(), "recording container destroy") {
				t.Fatalf("missing warnings: %s", warnings.String())
			}
			preserved, err := os.ReadFile(st.Path("events.json"))
			if err != nil || string(preserved) != data {
				t.Fatalf("corrupt journal modified: %q %v", preserved, err)
			}
		})
	}
	t.Run("cancellable reader and bounded writer", func(t *testing.T) {
		st, err := Open(t.TempDir(), false, "")
		if err != nil {
			t.Fatal(err)
		}
		lock, err := OpenLock(st.Path("events.lock"), true, true)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Release()
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(30*time.Millisecond, cancel)
		if _, err := st.ReadEventSnapshot(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("reader did not cancel: %v", err)
		}
		started := time.Now()
		if err := st.AppendEvent(Event{}); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second {
			t.Fatalf("writer did not time out: %v (%v)", err, time.Since(started))
		}
	})
}

func TestContainerEventTransitions(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.CreateContainer("transitions", ContainerRecord{ImageRef: "example:1", Image: "sha256:abc", State: ContainerState{RunID: "run"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"running", "paused", "paused", "running", "exited", "exited"} {
		_, err := st.UpdateContainer(rec.ID, func(c *ContainerRecord) error {
			c.State.Status = status
			c.State.Running = status != "exited"
			c.State.Paused = status == "paused"
			c.State.Launched = true
			c.State.ExitCode = 37
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RemoveContainer(rec.ID); err != nil {
		t.Fatal(err)
	}
	want := []string{"create", "start", "pause", "unpause", "die", "destroy"}
	if got := eventActions(t, st); !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions: %v, want %v", got, want)
	}
	snapshot, _ := st.ReadEventSnapshot(context.Background())
	for _, entry := range snapshot.Events {
		attrs := entry.Event.Actor.Attributes
		if entry.Event.Actor.ID != rec.ID || attrs["name"] != "transitions" || attrs["imageID"] != rec.Image || attrs["runID"] != "run" {
			t.Fatalf("lost actor metadata: %+v", entry)
		}
		if entry.Event.Action == "die" && attrs["exitCode"] != "37" {
			t.Fatalf("lost exit code: %+v", entry)
		}
	}
}

func TestContainerEventReconciliationAndPrune(t *testing.T) {
	for _, autoRemove := range []bool{false, true} {
		t.Run(strconv.FormatBool(autoRemove), func(t *testing.T) {
			st, err := Open(t.TempDir(), false, "")
			if err != nil {
				t.Fatal(err)
			}
			rec, err := st.CreateContainer("abandoned", ContainerRecord{
				HostConfig: ContainerHostConfig{AutoRemove: autoRemove},
				State:      ContainerState{Status: "paused", Running: true, Paused: true, Launched: true, RunID: "abandoned-run"},
			})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				_, err := st.ReadContainer(rec.ID)
				if autoRemove && !errors.Is(err, ErrContainerNotFound) || !autoRemove && err != nil {
					t.Fatal(err)
				}
			}
			want := []string{"create", "die"}
			if autoRemove {
				want = append(want, "destroy")
			}
			if got := eventActions(t, st); !reflect.DeepEqual(got, want) {
				t.Fatalf("reconciliation: %v, want %v", got, want)
			}
			snapshot, _ := st.ReadEventSnapshot(context.Background())
			if attrs := snapshot.Events[1].Event.Actor.Attributes; attrs["reconciled"] != "true" || attrs["exitCode"] != "137" {
				t.Fatalf("missing reconciliation metadata: %v", attrs)
			}
			if _, _, _, err := st.PruneContainers(true); err != nil {
				t.Fatal(err)
			}
			if got := eventActions(t, st); !reflect.DeepEqual(got, want) {
				t.Fatalf("dry run recorded events: %v", got)
			}
			if _, _, _, err := st.PruneContainers(false); err != nil {
				t.Fatal(err)
			}
			if !autoRemove {
				want = append(want, "destroy")
			}
			if got := eventActions(t, st); !reflect.DeepEqual(got, want) {
				t.Fatalf("prune: %v, want %v", got, want)
			}
		})
	}
}
