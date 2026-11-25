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

const statsProtocol = 1

// ErrStatsRunChanged means the sample belongs to a run that stopped or changed.
// Callers must discard its CPU baseline and refresh the container record.
var ErrStatsRunChanged = errors.New("container run changed while sampling stats")

type statsRequest struct {
	Protocol int
	RunID    string
}

// StatsSample contains raw counters. SampledAt is assigned by the client so
// consecutive samples retain Go's monotonic clock for CPU rate calculations.
type StatsSample struct {
	win32.JobStats
	RunID     string
	SampledAt time.Time `json:"-"`
}

// ContainerStats queries a live supervisor without taking its operation lock.
func ContainerStats(cancel stdcontext.Context, ctx *context.Context, rec *store.ContainerRecord) (StatsSample, error) {
	if err := cancel.Err(); err != nil {
		return StatsSample{}, err
	}
	if !rec.State.Running {
		return StatsSample{}, ErrStatsRunChanged
	}
	if rec.State.StatsProtocol != statsProtocol {
		return StatsSample{}, errors.New("container's supervisor does not support stats; stop and start it with this binary")
	}
	requestCtx, stop := stdcontext.WithTimeout(cancel, 5*time.Second)
	defer stop()
	deadline, _ := requestCtx.Deadline()
	var file *os.File
	var err error
	for requestCtx.Err() == nil {
		attempt := time.Now().Add(100 * time.Millisecond)
		if deadline.Before(attempt) {
			attempt = deadline
		}
		file, err = win32.DialPipe(execPipeName(rec.ID, rec.State.RunID), attempt)
		if err == nil || !errors.Is(err, os.ErrDeadlineExceeded) {
			break
		}
		if !ctx.Store.IsContainerRunning(rec.ID) {
			return StatsSample{}, ErrStatsRunChanged
		}
	}
	if file == nil {
		if requestCtx.Err() != nil {
			err = requestCtx.Err()
		}
		return StatsSample{}, fmt.Errorf("connect to stats supervisor: %w", err)
	}
	c := &execStream{File: file}
	defer func() { _ = c.Close() }()
	closeOnCancel := stdcontext.AfterFunc(requestCtx, func() { _ = c.Close() })
	defer closeOnCancel()
	if err := c.SetDeadline(deadline); err != nil {
		return StatsSample{}, err
	}
	data, _ := json.Marshal(statsRequest{statsProtocol, rec.State.RunID})
	if err := c.send(statsQuery, data); err != nil {
		return StatsSample{}, err
	}
	kind, data, err := c.receive()
	if err != nil {
		return StatsSample{}, fmt.Errorf("read stats response: %w", err)
	}
	var sample StatsSample
	if kind == execError {
		err = errors.New(string(data))
		if string(data) == ErrStatsRunChanged.Error() {
			err = ErrStatsRunChanged
		}
	} else if kind != statsResult {
		err = errors.New("invalid stats response")
	} else if err = json.Unmarshal(data, &sample); err == nil && sample.RunID != rec.State.RunID {
		err = ErrStatsRunChanged
	}
	sample.SampledAt = time.Now()
	// A run can exit or restart while the pipe response is in flight.
	current, readErr := ctx.Store.ReadContainer(rec.ID)
	if errors.Is(readErr, store.ErrContainerNotFound) || (readErr == nil && (!current.State.Running || current.State.RunID != rec.State.RunID)) {
		return StatsSample{}, ErrStatsRunChanged
	}
	if err != nil {
		return StatsSample{}, err
	}
	if readErr != nil {
		return StatsSample{}, readErr
	}
	return sample, nil
}

func (r *execRuntime) serveStats(c *execStream, data []byte) {
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	var request statsRequest
	if json.Unmarshal(data, &request) != nil || request.Protocol != statsProtocol || request.RunID != r.runID {
		_ = c.send(execError, []byte("stats protocol or container run changed"))
		return
	}
	r.mu.Lock()
	var stats win32.JobStats
	var err error
	if r.closing || win32.WaitForSingleObject(r.primary, 0) != win32.WAIT_TIMEOUT {
		err = ErrStatsRunChanged
	} else {
		stats, err = win32.QueryJobStats(r.job)
	}
	r.mu.Unlock()
	// Never hold the runtime lock while sending to a possibly slow reader.
	if err != nil {
		_ = c.send(execError, []byte(err.Error()))
		return
	}
	data, _ = json.Marshal(StatsSample{JobStats: stats, RunID: r.runID})
	_ = c.send(statsResult, data)
}
