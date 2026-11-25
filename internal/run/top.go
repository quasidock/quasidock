package run

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

const topProtocol = 1

var ErrTopRunChanged = errors.New("container run changed while listing processes")

type topRequest struct {
	Protocol int
	RunID    string
}

type topMessage struct {
	RunID   string
	Process *win32.ProcessInfo `json:",omitempty"`
}

type TopSnapshot struct {
	RunID     string
	Processes []win32.ProcessInfo
}

// ContainerTop queries the run selected by rec. A completed snapshot is checked
// against the store before being returned; it never follows a replacement run.
func ContainerTop(cancel stdcontext.Context, ctx *context.Context, rec *store.ContainerRecord) (TopSnapshot, error) {
	if err := cancel.Err(); err != nil {
		return TopSnapshot{}, err
	}
	if !rec.State.Running {
		return TopSnapshot{}, fmt.Errorf("container %s is not running", rec.BareName())
	}
	if rec.State.TopProtocol != topProtocol {
		return TopSnapshot{}, errors.New("container's supervisor does not support top; stop and start it with this binary")
	}
	requestCtx, stop := stdcontext.WithTimeout(cancel, 5*time.Second)
	defer stop()
	snapshot, err := requestTop(requestCtx, ctx, rec)
	if cancel.Err() != nil {
		return TopSnapshot{}, cancel.Err()
	}
	current, readErr := ctx.Store.ReadContainer(rec.ID)
	if errors.Is(readErr, store.ErrContainerNotFound) || (readErr == nil && (!current.State.Running || current.State.RunID != rec.State.RunID)) {
		return TopSnapshot{}, ErrTopRunChanged
	}
	if requestCtx.Err() != nil {
		return TopSnapshot{}, requestCtx.Err()
	}
	if err != nil {
		return TopSnapshot{}, err
	}
	if readErr != nil {
		return TopSnapshot{}, readErr
	}
	return snapshot, nil
}

func requestTop(cancel stdcontext.Context, ctx *context.Context, rec *store.ContainerRecord) (TopSnapshot, error) {
	deadline, _ := cancel.Deadline()
	var file *os.File
	var err error
	for cancel.Err() == nil {
		attempt := time.Now().Add(100 * time.Millisecond)
		if deadline.Before(attempt) {
			attempt = deadline
		}
		file, err = win32.DialPipe(execPipeName(rec.ID, rec.State.RunID), attempt)
		if err == nil || !errors.Is(err, os.ErrDeadlineExceeded) {
			break
		}
		current, readErr := ctx.Store.ReadContainer(rec.ID)
		if errors.Is(readErr, store.ErrContainerNotFound) || (readErr == nil && (!current.State.Running || current.State.RunID != rec.State.RunID)) {
			return TopSnapshot{}, ErrTopRunChanged
		}
		if readErr != nil {
			return TopSnapshot{}, readErr
		}
	}
	if file == nil {
		if cancel.Err() != nil {
			err = cancel.Err()
		}
		return TopSnapshot{}, fmt.Errorf("connect to top supervisor: %w", err)
	}
	c := &execStream{File: file}
	defer func() { _ = c.Close() }()
	closeOnCancel := stdcontext.AfterFunc(cancel, func() { _ = c.Close() })
	defer closeOnCancel()
	if err := c.SetDeadline(deadline); err != nil {
		return TopSnapshot{}, err
	}
	data, _ := json.Marshal(topRequest{topProtocol, rec.State.RunID})
	if err := c.send(topQuery, data); err != nil {
		return TopSnapshot{}, err
	}
	return receiveTop(c, rec.State.RunID)
}

func receiveTop(c *execStream, runID string) (TopSnapshot, error) {
	snapshot := TopSnapshot{RunID: runID, Processes: []win32.ProcessInfo{}}
	seen := make(map[uint32]bool)
	for {
		kind, data, err := c.receive()
		if err != nil {
			return TopSnapshot{}, fmt.Errorf("read top response: %w", err)
		}
		if kind == execError {
			if string(data) == ErrTopRunChanged.Error() {
				return TopSnapshot{}, ErrTopRunChanged
			}
			return TopSnapshot{}, errors.New(string(data))
		}
		if kind != topProcess && kind != topComplete {
			return TopSnapshot{}, errors.New("invalid top response")
		}
		var message topMessage
		if err := json.Unmarshal(data, &message); err != nil {
			return TopSnapshot{}, fmt.Errorf("invalid top response: %w", err)
		}
		if message.RunID != runID {
			return TopSnapshot{}, ErrTopRunChanged
		}
		if kind == topComplete {
			if message.Process != nil {
				return TopSnapshot{}, errors.New("invalid top completion")
			}
			return snapshot, nil
		}
		if message.Process == nil || message.Process.PID == 0 || seen[message.Process.PID] {
			return TopSnapshot{}, errors.New("invalid top process")
		}
		seen[message.Process.PID] = true
		snapshot.Processes = append(snapshot.Processes, *message.Process)
	}
}

func (r *execRuntime) serveTop(c *execStream, data []byte) {
	cancel, stop := stdcontext.WithTimeout(stdcontext.Background(), 5*time.Second)
	defer stop()
	deadline, _ := cancel.Deadline()
	_ = c.SetDeadline(deadline)
	var request topRequest
	if json.Unmarshal(data, &request) != nil || request.Protocol != topProtocol || request.RunID != r.runID {
		_ = c.send(execError, []byte("top protocol or container run changed"))
		return
	}
	r.mu.Lock()
	active := !r.closing && win32.WaitForSingleObject(r.primary, 0) == win32.WAIT_TIMEOUT
	r.mu.Unlock()
	if !active {
		_ = c.send(execError, []byte(ErrTopRunChanged.Error()))
		return
	}
	// This worker holds a wg reference, so Close cannot release the borrowed
	// job/primary handles until collection finishes. No operation lock is needed.
	processes, err := win32.QueryJobProcesses(cancel, r.job)
	if err != nil {
		_ = c.send(execError, []byte(err.Error()))
		return
	}
	for _, process := range processes {
		data, err = json.Marshal(topMessage{RunID: r.runID, Process: &process})
		if err != nil {
			_ = c.send(execError, []byte(err.Error()))
			return
		}
		if err := c.send(topProcess, data); err != nil {
			return
		}
	}
	r.mu.Lock()
	active = !r.closing && win32.WaitForSingleObject(r.primary, 0) == win32.WAIT_TIMEOUT
	r.mu.Unlock()
	if !active {
		_ = c.send(execError, []byte(ErrTopRunChanged.Error()))
		return
	}
	data, _ = json.Marshal(topMessage{RunID: r.runID})
	_ = c.send(topComplete, data)
}
