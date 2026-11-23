package run

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quasidock/quasidock/internal/win32"
)

const attachProtocol = 1
const attachWriteTimeout = 5 * time.Second
const attachOutputLimit = 1024 * 1024

const (
	attachHello byte = iota
	attachReady
	attachStarted
	attachStdout
	attachStderr
	attachStdin
	attachEOF
	attachInputAck
	attachTERM
	attachExit
	attachError
	attachSignalError
	attachInputClosed
)

type attachRequest struct {
	Protocol int
	RunID    string
	Stdin    bool
}

type attachResponse struct {
	Stdin, TTY bool
}

type attachFrame struct {
	kind byte
	data []byte
}

type attachInputRequest struct {
	session *attachSession
	data    []byte
	eof     bool
}

// The runtime owns stdin independently of its current writer. A blocked write
// survives detachment without retaining session workers or closing child stdin.
type attachRuntime struct {
	mu                              sync.Mutex
	wg                              sync.WaitGroup
	once                            sync.Once
	listener                        *win32.PipeListener
	runID                           string
	tty, inputOpen, started, ending bool
	input                           *os.File
	inputReady                      chan struct{}
	inputQueue                      chan attachInputRequest
	done                            chan struct{}
	gate                            chan struct{}
	gateOnce                        sync.Once
	sessions                        map[*attachSession]struct{}
	connections                     map[*framedStream]struct{}
	writer                          *attachSession
	signal                          func() error
	admit                           func() error
}

type attachSession struct {
	r       *attachRuntime
	c       *framedStream
	stdin   bool
	queue   chan attachFrame
	input   chan attachInputRequest
	done    chan struct{}
	once    sync.Once
	bytes   atomic.Int64
	pending atomic.Bool
}

func attachPipeName(id, runID string) string { return `\\.\pipe\quasidock-attach-` + id + "-" + runID }

func newAttachRuntime(id, runID string, openStdin, tty bool) (*attachRuntime, error) {
	l, err := win32.ListenPipe(attachPipeName(id, runID))
	if err != nil {
		return nil, err
	}
	r := &attachRuntime{
		listener: l, runID: runID, tty: tty, inputOpen: openStdin,
		inputReady: make(chan struct{}), inputQueue: make(chan attachInputRequest),
		done: make(chan struct{}), gate: make(chan struct{}),
		sessions: make(map[*attachSession]struct{}), connections: make(map[*framedStream]struct{}),
	}
	r.wg.Add(2)
	go r.accept()
	go r.pumpInput()
	return r, nil
}

func (r *attachRuntime) accept() {
	defer r.wg.Done()
	for {
		f, err := r.listener.Accept()
		if err != nil {
			return
		}
		c := &framedStream{File: f}
		r.mu.Lock()
		if r.ending {
			r.mu.Unlock()
			c.Close()
			continue
		}
		r.connections[c] = struct{}{}
		r.wg.Add(1)
		r.mu.Unlock()
		go func() { defer r.wg.Done(); r.serve(c) }()
	}
}

func (s *attachSession) close() {
	s.once.Do(func() {
		close(s.done)
		s.c.Close()
		s.r.mu.Lock()
		delete(s.r.sessions, s)
		if s.r.writer == s {
			s.r.writer = nil
		}
		s.r.mu.Unlock()
	})
}

func (s *attachSession) enqueue(kind byte, data []byte) {
	if s.bytes.Add(int64(len(data))) > attachOutputLimit {
		s.close()
		return
	}
	select {
	case s.queue <- attachFrame{kind, data}:
	case <-s.done:
	default:
		s.close()
	}
}

func (r *attachRuntime) serve(c *framedStream) {
	defer func() {
		c.Close()
		r.mu.Lock()
		delete(r.connections, c)
		r.mu.Unlock()
	}()
	_ = c.SetDeadline(time.Now().Add(execStartupTimeout))
	kind, data, err := c.receive()
	var req attachRequest
	if err != nil {
		return
	}
	if kind != attachHello || json.Unmarshal(data, &req) != nil || req.Protocol != attachProtocol || req.RunID != r.runID {
		_ = c.send(attachError, []byte("attach protocol or container run changed"))
		return
	}
	s := &attachSession{r: r, c: c, queue: make(chan attachFrame, 64), input: make(chan attachInputRequest, 1), done: make(chan struct{})}
	r.mu.Lock()
	var reject string
	if r.ending {
		reject = "container is stopping"
	} else if r.admit != nil {
		if err := r.admit(); err != nil {
			reject = err.Error()
		}
	}
	if reject == "" && req.Stdin && r.inputOpen && r.writer != nil {
		reject = "container stdin already has a writer; use --no-stdin"
	}
	if reject != "" {
		r.mu.Unlock()
		_ = c.send(attachError, []byte(reject))
		return
	}
	s.stdin = req.Stdin && r.inputOpen
	if s.stdin {
		r.writer = s
	}
	r.sessions[s] = struct{}{}
	if r.started {
		s.queue <- attachFrame{kind: attachStarted}
	}
	// The session is visible to publishers before the startup gate opens.
	response, _ := json.Marshal(attachResponse{Stdin: s.stdin, TTY: r.tty})
	r.mu.Unlock()
	defer s.close()
	if c.send(attachReady, response) != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for {
			select {
			case frame := <-s.queue:
				_ = c.SetWriteDeadline(time.Now().Add(attachWriteTimeout))
				err := c.send(frame.kind, frame.data)
				s.bytes.Add(-int64(len(frame.data)))
				if err != nil || frame.kind == attachExit || frame.kind == attachError {
					s.close()
					return
				}
			case <-s.done:
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for {
			select {
			case input := <-s.input:
				select {
				case r.inputQueue <- input:
				case <-s.done:
					return
				case <-r.done:
					return
				}
			case <-s.done:
				return
			}
		}
	}()
	defer func() { s.close(); workers.Wait() }()
	r.gateOnce.Do(func() { close(r.gate) })
	eof := !s.stdin
	for {
		kind, data, err := c.receive()
		if err != nil {
			return
		}
		switch kind {
		case attachStdin, attachEOF:
			r.mu.Lock()
			running := r.started && !r.ending
			r.mu.Unlock()
			if !running || eof || len(data) > 64*1024 || !s.pending.CompareAndSwap(false, true) {
				return
			}
			if kind == attachEOF {
				if len(data) != 0 {
					return
				}
				eof = true
			}
			select {
			case s.input <- attachInputRequest{s, data, kind == attachEOF}:
			case <-s.done:
				return
			}
		case attachTERM:
			if len(data) != 0 {
				return
			}
			r.mu.Lock()
			var signalErr error
			if !r.started || r.ending {
				signalErr = errors.New("container is not running")
			} else {
				signalErr = r.signal()
			}
			r.mu.Unlock()
			if signalErr != nil {
				s.enqueue(attachSignalError, []byte(signalErr.Error()))
			}
		default:
			return
		}
	}
}

func (r *attachRuntime) setInput(f *os.File) {
	r.mu.Lock()
	r.input = f
	r.mu.Unlock()
	close(r.inputReady)
}

func (r *attachRuntime) pumpInput() {
	defer r.wg.Done()
	select {
	case <-r.inputReady:
	case <-r.done:
		return
	}
	for {
		select {
		case req := <-r.inputQueue:
			r.mu.Lock()
			input := r.input
			r.mu.Unlock()
			var err error
			if input == nil {
				err = errors.New("container stdin is closed")
			} else if req.eof {
				r.mu.Lock()
				r.inputOpen = false
				r.mu.Unlock()
				err = input.Close()
			} else {
				_, err = input.Write(req.data)
			}
			req.session.pending.Store(false)
			if err != nil {
				// A child can close stdin before exiting. Preserve its output and
				// final status instead of turning that into a failed attachment.
				r.mu.Lock()
				r.inputOpen = false
				r.input = nil
				if r.writer == req.session {
					r.writer = nil
				}
				r.mu.Unlock()
				if input != nil {
					input.Close()
				}
				req.session.enqueue(attachInputClosed, nil)
			} else {
				req.session.enqueue(attachInputAck, nil)
			}
		case <-r.done:
			return
		}
	}
}

// markStarted queues acknowledgement before any pipe pumps can publish data.
func (r *attachRuntime) markStarted(signal func() error) {
	r.mu.Lock()
	r.started, r.signal = true, signal
	var stalled []*attachSession
	for s := range r.sessions {
		select {
		case s.queue <- attachFrame{kind: attachStarted}:
		default:
			stalled = append(stalled, s)
		}
	}
	r.mu.Unlock()
	for _, s := range stalled {
		s.close()
	}
}

type attachWriter struct {
	r    *attachRuntime
	kind byte
}

func (w attachWriter) Write(p []byte) (int, error) {
	data := append([]byte(nil), p...)
	w.r.mu.Lock()
	sessions := make([]*attachSession, 0, len(w.r.sessions))
	for s := range w.r.sessions {
		sessions = append(sessions, s)
	}
	w.r.mu.Unlock()
	for _, s := range sessions {
		s.enqueue(w.kind, data)
	}
	return len(p), nil
}

func (r *attachRuntime) stopAccepting() {
	r.mu.Lock()
	r.ending = true
	r.mu.Unlock()
	r.listener.Close()
}

func (r *attachRuntime) finish(code uint32, runErr error) {
	r.once.Do(func() {
		r.stopAccepting()
		r.mu.Lock()
		input := r.input
		sessions := make([]*attachSession, 0, len(r.sessions))
		for s := range r.sessions {
			sessions = append(sessions, s)
		}
		// Close incomplete handshakes; established sessions receive completion.
		for c := range r.connections {
			found := false
			for _, s := range sessions {
				if s.c == c {
					found = true
					break
				}
			}
			if !found {
				c.Close()
			}
		}
		r.mu.Unlock()
		close(r.done)
		if input != nil {
			input.Close()
		}
		var status [4]byte
		binary.LittleEndian.PutUint32(status[:], code)
		for _, s := range sessions {
			if runErr != nil {
				s.enqueue(attachError, []byte(runErr.Error()))
			} else {
				s.enqueue(attachExit, status[:])
			}
		}
		// Bound total flushing even when many clients stop reading at once.
		timer := time.AfterFunc(attachWriteTimeout, func() {
			for _, s := range sessions {
				s.close()
			}
		})
		r.wg.Wait()
		timer.Stop()
	})
}
