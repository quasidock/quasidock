package run

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

// AttachOptions describes one live stream subscription. A nil DetachKeys uses
// Ctrl+P, Ctrl+Q; a pointer to an empty string disables keyboard detachment.
type AttachOptions struct {
	Container  string
	NoStdin    bool
	SigProxy   bool
	DetachKeys *string
}

func ValidateDetachKeys(value string) error { _, err := parseDetachKeys(value); return err }

func parseDetachKeys(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	var keys []byte
	for _, key := range strings.Split(value, ",") {
		switch {
		case len(key) == 1 && ((key[0] >= 'a' && key[0] <= 'z') || (key[0] >= 'A' && key[0] <= 'Z')):
			keys = append(keys, key[0])
		case len(key) == 6 && strings.HasPrefix(key, "ctrl-") && ((key[5] >= 'a' && key[5] <= 'z') || strings.ContainsRune("@[\\]^_", rune(key[5]))):
			keys = append(keys, key[5]&31)
		default:
			return nil, fmt.Errorf("invalid detach key %q: use comma-separated letters or ctrl- keys", key)
		}
	}
	return keys, nil
}

func (opts AttachOptions) keys() ([]byte, error) {
	if opts.DetachKeys == nil {
		return []byte{16, 17}, nil
	}
	return parseDetachKeys(*opts.DetachKeys)
}

func attachSupported(ctx *context.Context, rec *store.ContainerRecord) error {
	if !rec.State.Running || !ctx.Store.IsContainerRunning(rec.ID) {
		return fmt.Errorf("container %s is not running", rec.ShortID())
	}
	if rec.State.Paused {
		return errors.New("container is paused; unpause it first")
	}
	if rec.State.AttachProtocol != attachProtocol {
		return errors.New("container's supervisor does not support attach; use run -d, or stop and start it with this binary")
	}
	return nil
}

func Attach(ctx *context.Context, opts AttachOptions) (uint32, error) {
	if _, err := opts.keys(); err != nil {
		return 1, err
	}
	rec, op, err := lockTarget(ctx, opts.Container)
	if err != nil {
		return 1, err
	}
	defer op.Release()
	if err := attachSupported(ctx, rec); err != nil {
		return 1, err
	}
	c, response, err := dialAttachment(ctx, rec, opts, 0)
	if err != nil {
		return 1, err
	}
	defer func() { _ = c.Close() }()
	op.Release()
	return consumeAttachment(ctx, c, response, opts, nil)
}

// StartAttached subscribes before launching the child. -i also subscribes to
// output; it does not change the saved OpenStdin preference.
func StartAttached(ctx *context.Context, opts AttachOptions) (uint32, error) {
	if _, err := opts.keys(); err != nil {
		return 1, err
	}
	rec, op, err := lockTarget(ctx, opts.Container)
	if err != nil {
		return 1, err
	}
	defer op.Release()
	if ctx.Store.IsContainerRunning(rec.ID) {
		if err := attachSupported(ctx, rec); err != nil {
			return 1, err
		}
		c, response, err := dialAttachment(ctx, rec, opts, 0)
		if err != nil {
			return 1, err
		}
		defer func() { _ = c.Close() }()
		op.Release()
		return consumeAttachment(ctx, c, response, opts, nil)
	}
	rec, err = prepareStart(ctx, rec)
	if err != nil {
		return 1, err
	}
	det, ready, err := spawnSupervisor(ctx, rec, rec.RunSpec.HostCwd, true)
	if err != nil {
		recordLaunchFailure(ctx.Store, rec.ID, rec.State.RunID, err)
		return 1, err
	}
	defer win32.CloseHandle(det.Process)
	defer win32.CloseHandle(ready)
	c, response, err := dialAttachment(ctx, rec, opts, det.Process)
	if err != nil {
		// Complete the same bounded startup cancellation as an ordinary start.
		startupErr := awaitSupervisor(ctx, rec, det.Process, ready, 1)
		if startupErr != nil {
			recordLaunchFailure(ctx.Store, rec.ID, rec.State.RunID, startupErr)
		}
		return 1, err
	}
	defer func() { _ = c.Close() }()
	startup := make(chan error, 1)
	go func() {
		err := awaitSupervisor(ctx, rec, det.Process, ready, supervisorStartupTimeout)
		if err != nil {
			_ = c.Close()
		}
		startup <- err
	}()
	code, streamErr := consumeAttachment(ctx, c, response, opts, op.Release)
	_ = c.Close()
	if err := <-startup; err != nil {
		if !ctx.Store.IsContainerRunning(rec.ID) {
			recordLaunchFailure(ctx.Store, rec.ID, rec.State.RunID, err)
		}
		return 1, err
	}
	return code, streamErr
}

func dialAttachment(ctx *context.Context, rec *store.ContainerRecord, opts AttachOptions, supervisor win32.HANDLE) (*framedStream, attachResponse, error) {
	deadline := time.Now().Add(execStartupTimeout)
	var f *os.File
	var err error
	for {
		attempt := deadline
		if supervisor != 0 && time.Until(deadline) > 100*time.Millisecond {
			attempt = time.Now().Add(100 * time.Millisecond)
		}
		f, err = win32.DialPipe(attachPipeName(rec.ID, rec.State.RunID), attempt)
		if err == nil {
			break
		}
		if supervisor == 0 || !os.IsTimeout(err) || !time.Now().Before(deadline) {
			return nil, attachResponse{}, fmt.Errorf("connecting to container attach: %w", err)
		}
		if win32.WaitForSingleObject(supervisor, 0) != win32.WAIT_TIMEOUT {
			if updated, e := ctx.Store.ReadContainer(rec.ID); e == nil && updated.State.Error != "" {
				return nil, attachResponse{}, errors.New(updated.State.Error)
			}
			return nil, attachResponse{}, errors.New("container supervisor exited before attachment")
		}
	}
	c := &framedStream{File: f}
	fail := func(err error) (*framedStream, attachResponse, error) {
		_ = c.Close()
		return nil, attachResponse{}, err
	}
	if err := c.SetDeadline(deadline); err != nil {
		return fail(err)
	}
	request, _ := json.Marshal(attachRequest{Protocol: attachProtocol, RunID: rec.State.RunID, Stdin: !opts.NoStdin})
	if err := c.send(attachHello, request); err != nil {
		return fail(err)
	}
	kind, data, err := c.receive()
	if err != nil {
		return fail(fmt.Errorf("waiting for attachment: %w", err))
	}
	if kind == attachError {
		return fail(errors.New(string(data)))
	}
	var response attachResponse
	if kind != attachReady || json.Unmarshal(data, &response) != nil {
		return fail(errors.New("invalid attach acknowledgement"))
	}
	return c, response, nil
}

type detachFilter struct{ keys, pending []byte }

func (f *detachFilter) feed(p []byte) ([]byte, bool) {
	if len(f.keys) == 0 {
		return p, false
	}
	var out []byte
	for _, b := range p {
		f.pending = append(f.pending, b)
		for !bytes.HasPrefix(f.keys, f.pending) {
			out = append(out, f.pending[0])
			f.pending = f.pending[1:]
		}
		if bytes.Equal(f.pending, f.keys) {
			f.pending = nil
			return out, true
		}
	}
	return out, false
}

func consumeAttachment(ctx *context.Context, c *framedStream, response attachResponse, opts AttachOptions, onStarted func()) (uint32, error) {
	keys, _ := opts.keys()
	done := make(chan struct{})
	var workers sync.WaitGroup
	var localExit atomic.Int32 // 1: keyboard detach, 2: Ctrl+C without proxy
	inputErrors := make(chan error, 1)
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	interrupt := func() {
		if !opts.SigProxy {
			localExit.Store(2)
			_ = c.Close()
			return
		}
		_ = c.SetWriteDeadline(time.Now().Add(attachWriteTimeout))
		if c.send(attachTERM, nil) != nil {
			_ = c.Close()
		}
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			select {
			case <-interrupts:
				interrupt()
			case <-done:
				return
			}
		}
	}()
	var inputThread win32.HANDLE
	var inputFinished chan struct{}
	var consoleMode win32.DWORD
	var console bool
	inputDone := make(chan struct{})
	var stopInputOnce sync.Once
	stopInput := func() {
		stopInputOnce.Do(func() {
			close(inputDone)
			if inputThread != 0 {
				// Cancellation can race with entry into ReadFile. Repeat until the
				// dedicated reader has observed done and left its Windows thread.
				for {
					win32.CancelSynchronousIo(inputThread)
					select {
					case <-inputFinished:
						goto joined
					case <-time.After(time.Millisecond):
					}
				}
			}
		joined:
			if inputThread != 0 {
				win32.CloseHandle(inputThread)
			}
			if console {
				win32.SetConsoleMode(win32.GetStdHandle(win32.STD_INPUT_HANDLE), consoleMode)
			}
		})
	}
	defer func() {
		close(done)
		_ = c.Close()
		stopInput()
		workers.Wait()
	}()
	inputAck := make(chan struct{}, 1)
	startInput := func() error {
		if !response.Stdin {
			return nil
		}
		h := win32.GetStdHandle(win32.STD_INPUT_HANDLE)
		console = response.TTY && win32.GetConsoleMode(h, &consoleMode)
		if console && !win32.SetConsoleMode(h, consoleMode&^(win32.ENABLE_LINE_INPUT|win32.ENABLE_ECHO_INPUT|win32.ENABLE_PROCESSED_INPUT)) {
			return errors.New("setting attach console mode failed")
		}
		frames := make(chan attachFrame, 8)
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case frame := <-frames:
					_ = c.SetWriteDeadline(time.Now().Add(attachWriteTimeout))
					if c.send(frame.kind, frame.data) != nil {
						_ = c.Close()
						return
					}
					select {
					case <-inputAck:
					case <-inputDone:
						return
					case <-done:
						return
					}
				case <-done:
					return
				case <-inputDone:
					return
				}
			}
		}()
		threadReady := make(chan error, 1)
		inputFinished = make(chan struct{})
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer close(inputFinished)
			runtime.LockOSThread()
			// Let Go retire this thread on goroutine exit. Cancellation must
			// never target a thread already reused for an unrelated syscall.
			thread, err := win32.OpenCurrentThreadForCancellation()
			inputThread = thread
			threadReady <- err
			if err != nil {
				return
			}
			fail := func(err error) {
				select {
				case inputErrors <- err:
				default:
				}
				_ = c.Close()
			}
			emit := func(kind byte, data []byte) bool {
				frame := attachFrame{kind, append([]byte(nil), data...)}
				if console {
					select {
					case frames <- frame:
						return true
					case <-done:
						return false
					case <-inputDone:
						return false
					default:
						fail(errors.New("attach console input buffer is full"))
						return false
					}
				}
				select {
				case frames <- frame:
					return true
				case <-done:
					return false
				case <-inputDone:
					return false
				}
			}
			filter := detachFilter{keys: keys}
			buf := make([]byte, 64*1024)
			for {
				select {
				case <-done:
					return
				case <-inputDone:
					return
				default:
				}
				n, readErr := os.Stdin.Read(buf)
				p := buf[:n]
				detached := false
				if console {
					p, detached = filter.feed(p)
				}
				if console {
					for {
						i := bytes.IndexByte(p, 3)
						if i < 0 {
							break
						}
						if i > 0 && !emit(attachStdin, p[:i]) {
							return
						}
						interrupt()
						if !opts.SigProxy {
							return
						}
						p = p[i+1:]
					}
				}
				if len(p) > 0 && !emit(attachStdin, p) {
					return
				}
				if detached {
					localExit.Store(1)
					_ = c.Close()
					return
				}
				if readErr != nil {
					select {
					case <-done:
						return
					case <-inputDone:
						return
					default:
					}
					if !errors.Is(readErr, io.EOF) {
						fail(readErr)
						return
					}
					if len(filter.pending) != 0 && console && !emit(attachStdin, filter.pending) {
						return
					}
					emit(attachEOF, nil)
					return
				}
			}
		}()
		return <-threadReady
	}
	stdout, stderr := ctx.Stdout, ctx.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	started := false
	for {
		kind, data, err := c.receive()
		if err != nil {
			switch localExit.Load() {
			case 1:
				return 0, nil
			case 2:
				return 130, nil
			}
			select {
			case inputErr := <-inputErrors:
				return 1, inputErr
			default:
			}
			return 1, fmt.Errorf("attach disconnected before exit status: %w", err)
		}
		switch kind {
		case attachStarted:
			if started || len(data) != 0 {
				return 1, errors.New("invalid attach start acknowledgement")
			}
			started = true
			if err := c.SetDeadline(time.Time{}); err != nil {
				return 1, err
			}
			if onStarted != nil {
				onStarted()
			}
			if err := startInput(); err != nil {
				return 1, err
			}
		case attachStdout:
			_, err = stdout.Write(data)
		case attachStderr:
			_, err = stderr.Write(data)
		case attachInputAck:
			select {
			case inputAck <- struct{}{}:
			default:
				return 1, errors.New("unexpected attach stdin acknowledgement")
			}
		case attachInputClosed:
			if len(data) != 0 {
				return 1, errors.New("invalid attach stdin closure")
			}
			stopInput()
		case attachSignalError:
			_, err = fmt.Fprintf(stderr, "attach: TERM: %s\n", data)
		case attachError:
			return 1, errors.New(string(data))
		case attachExit:
			if len(data) != 4 {
				return 1, errors.New("invalid attach exit status")
			}
			return binary.LittleEndian.Uint32(data), nil
		default:
			return 1, errors.New("invalid attach response")
		}
		if err != nil {
			return 1, err
		}
	}
}
