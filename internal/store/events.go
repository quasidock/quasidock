package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"
)

const EventHistoryLimit = 256

// Event is the public events JSON/template value. Journal cursors are separate
// so scripts need not depend on the local persistence format.
type Event struct {
	Type     string     `json:"Type"`
	Action   string     `json:"Action"`
	Actor    EventActor `json:"Actor"`
	Scope    string     `json:"scope"`
	Time     int64      `json:"time"`
	TimeNano int64      `json:"timeNano"`
}

type EventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes"`
}

type EventEntry struct {
	Sequence uint64 `json:"sequence"`
	Event    Event  `json:"event"`
}

type EventSnapshot struct {
	Version      int          `json:"version"`
	LastSequence uint64       `json:"lastSequence"`
	Events       []EventEntry `json:"events"`
}

// eventLock never acquires another store lock. Producers may already own the
// container index or runtime lock; readers must release this before output.
func (s *Store) eventLock(ctx context.Context, exclusive bool) (*Lock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := TryLock(s.Path("events.lock"), exclusive)
		if err != nil || lock != nil {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (s *Store) readEventsNoLock() (EventSnapshot, error) {
	data, err := os.ReadFile(s.Path("events.json"))
	if errors.Is(err, os.ErrNotExist) {
		return EventSnapshot{Version: 1}, nil
	}
	if err != nil {
		return EventSnapshot{}, err
	}
	var snapshot EventSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return EventSnapshot{}, fmt.Errorf("reading event journal: %w", err)
	}
	if snapshot.Version != 1 || len(snapshot.Events) > EventHistoryLimit ||
		uint64(len(snapshot.Events)) != min(snapshot.LastSequence, uint64(EventHistoryLimit)) {
		return EventSnapshot{}, errors.New("invalid or unsupported event journal")
	}
	for i, entry := range snapshot.Events {
		if entry.Sequence != snapshot.LastSequence-uint64(len(snapshot.Events)-1-i) {
			return EventSnapshot{}, errors.New("invalid event journal sequence")
		}
	}
	return snapshot, nil
}

// ReadEventSnapshot copies the retained history under a shared, cancellable
// lock. A missing journal is empty; malformed journals are never reset silently.
func (s *Store) ReadEventSnapshot(ctx context.Context) (EventSnapshot, error) {
	lock, err := s.eventLock(ctx, false)
	if err != nil {
		return EventSnapshot{}, err
	}
	defer lock.Release()
	return s.readEventsNoLock()
}

// AppendEvent serializes writers across processes and atomically publishes the
// bounded history. State changes and this journal are deliberately not a single
// transaction. Call RecordContainerEvent for warning-only lifecycle recording.
func (s *Store) AppendEvent(event Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := s.eventLock(ctx, true)
	if err != nil {
		return err
	}
	defer lock.Release()
	snapshot, err := s.readEventsNoLock()
	if err != nil {
		return err
	}
	if snapshot.LastSequence == math.MaxUint64 {
		return errors.New("event journal sequence exhausted")
	}
	now := time.Now().UTC()
	event.Time, event.TimeNano = now.Unix(), now.UnixNano()
	snapshot.LastSequence++
	snapshot.Events = append(snapshot.Events, EventEntry{Sequence: snapshot.LastSequence, Event: event})
	if len(snapshot.Events) > EventHistoryLimit {
		snapshot.Events = snapshot.Events[len(snapshot.Events)-EventHistoryLimit:]
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	// A sibling temporary file stays on the same volume and outside prune's tmp
	// directory. Readers open and close the snapshot while holding events.lock.
	f, err := os.CreateTemp(s.Root, ".events-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return RenameRetrying(f.Name(), s.Path("events.json"), 1000)
}

// RecordContainerEvent captures metadata before records can be removed and
// never changes the outcome of the operation being observed.
func (s *Store) RecordContainerEvent(rec *ContainerRecord, action string, attributes map[string]string) {
	attrs := map[string]string{
		"name": rec.BareName(), "image": rec.ImageRef, "imageID": rec.Image,
	}
	if rec.State.RunID != "" {
		attrs["runID"] = rec.State.RunID
	}
	for key, value := range attributes {
		attrs[key] = value
	}
	err := s.AppendEvent(Event{Type: "container", Action: action, Scope: "local",
		Actor: EventActor{ID: rec.ID, Attributes: attrs}})
	if err != nil {
		out := s.EventWarnings
		if out == nil {
			out = os.Stderr
		}
		_, _ = fmt.Fprintf(out, "warning: recording container %s event for %s: %v\n", action, rec.ShortID(), err)
	}
}

func (s *Store) recordContainerTransition(before ContainerState, rec *ContainerRecord, reconciled bool) {
	if before.RunID != rec.State.RunID {
		// A new run first saves its created state; only its later launch counts.
		return
	}
	if !before.Running && rec.State.Running && rec.State.Launched {
		s.RecordContainerEvent(rec, "start", nil)
	}
	if before.Running && rec.State.Running && before.Paused != rec.State.Paused {
		action := "unpause"
		if rec.State.Paused {
			action = "pause"
		}
		s.RecordContainerEvent(rec, action, nil)
	}
	if (before.Running || before.Launched && before.Status != "exited") && rec.State.Status == "exited" && !rec.State.Running {
		attrs := map[string]string{"exitCode": strconv.Itoa(rec.State.ExitCode)}
		if reconciled {
			attrs["reconciled"] = "true"
		}
		s.RecordContainerEvent(rec, "die", attrs)
	}
}
