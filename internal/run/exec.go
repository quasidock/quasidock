package run

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

const execProtocol = 1
const execStartupTimeout = 30 * time.Second
const maxExecFrame = maxStreamFrame
const (
	execLaunch byte = iota
	execStarted
	execStdout
	execStderr
	execStdin
	execEOF
	execExit
	execError
	execCancel
	execInputAck
	commitAcquire
	commitAcquired
	commitRelease
	commitReleased
	statsQuery
	statsResult
)

type ExecOptions struct {
	Container                string
	Args                     []string
	Env                      []string
	Workdir                  string
	Detach, Interactive, TTY bool
}

type execRequest struct {
	Protocol                 int
	RunID                    string
	Args                     []string
	Env                      map[string]*string
	Workdir                  string
	Detach, Interactive, TTY bool
}

func execPipeName(id, runID string) string { return `\\.\pipe\quasidock-exec-` + id + "-" + runID }

type execStream = framedStream

func execEnvironment(host map[string]string, entries []string) (map[string]*string, error) {
	changes := make(map[string]*string)
	for _, entry := range entries {
		name, value, explicit := strings.Cut(entry, "=")
		if name == "" || strings.ContainsRune(entry, 0) {
			return nil, fmt.Errorf("invalid environment setting %q", entry)
		}
		var replacement *string
		if explicit {
			replacement = &value
		} else {
			for k, v := range host {
				if strings.EqualFold(k, name) {
					replacement = &v
					break
				}
			}
		}
		changes[strings.ToLower(name)] = replacement
	}
	return changes, nil
}

// Exec asks the live owner to launch a command using its existing runtime.
func Exec(ctx *context.Context, opts ExecOptions) (uint32, error) {
	if len(opts.Args) == 0 || opts.Args[0] == "" {
		return 1, errors.New("exec requires a command")
	}
	env, err := execEnvironment(ctx.Env, opts.Env)
	if err != nil {
		return 1, err
	}
	rec, op, err := lockTarget(ctx, opts.Container)
	if err != nil {
		return 1, err
	}
	defer op.Release()
	if !rec.State.Running || !ctx.Store.IsContainerRunning(rec.ID) {
		return 1, fmt.Errorf("container %s is not running", rec.ShortID())
	}
	if rec.State.Paused {
		return 1, errors.New("container is paused; unpause it first")
	}
	if rec.State.ExecProtocol != execProtocol {
		return 1, errors.New("container's running supervisor does not support exec; stop and start it with this binary")
	}
	request := execRequest{
		Protocol: execProtocol, RunID: rec.State.RunID, Args: opts.Args, Env: env,
		Workdir: opts.Workdir, Detach: opts.Detach, Interactive: opts.Interactive, TTY: opts.TTY,
	}
	data, err := json.Marshal(request)
	if err != nil {
		return 1, err
	}
	deadline := time.Now().Add(execStartupTimeout)
	file, err := win32.DialPipe(execPipeName(rec.ID, rec.State.RunID), deadline)
	if err != nil {
		return 1, fmt.Errorf("connecting to container exec: %w", err)
	}
	c := &execStream{File: file}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(deadline); err != nil {
		return 1, err
	}
	if err := c.send(execLaunch, data); err != nil {
		return 1, err
	}
	kind, data, err := c.receive()
	if err != nil {
		return 1, fmt.Errorf("waiting for exec launch: %w", err)
	}
	if kind == execError {
		return 1, errors.New(string(data))
	}
	if kind != execStarted {
		return 1, errors.New("invalid exec launch acknowledgement")
	}
	if ctx.Verbose {
		ctx.Note("exec %s: %s", rec.ShortID(), process.BuildCommandLine(opts.Args[0], opts.Args[1:]))
	}
	op.Release()
	if opts.Detach {
		return 0, nil
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		return 1, err
	}
	done := make(chan struct{})
	defer close(done)
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	var interrupted atomic.Bool
	go func() {
		select {
		case <-interrupts:
			interrupted.Store(true)
			_ = c.Close()
		case <-done:
		}
	}()
	inputAck := make(chan struct{}, 1)
	if opts.Interactive {
		go func() {
			buf := make([]byte, 64*1024)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					if c.send(execStdin, buf[:n]) != nil {
						return
					}
					select {
					case <-inputAck:
					case <-done:
						return
					}
				}
				if err != nil {
					_ = c.send(execEOF, nil)
					return
				}
			}
		}()
	}
	stdout, stderr := ctx.Stdout, ctx.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	for {
		kind, data, err = c.receive()
		if err != nil {
			if interrupted.Load() {
				return 130, nil
			}
			return 1, fmt.Errorf("reading exec response: %w", err)
		}
		switch kind {
		case execStdout:
			_, err = stdout.Write(data)
		case execStderr:
			_, err = stderr.Write(data)
		case execInputAck:
			select {
			case inputAck <- struct{}{}:
			default:
				return 1, errors.New("unexpected exec stdin acknowledgement")
			}
		case execExit:
			if len(data) != 4 {
				return 1, errors.New("invalid exec exit status")
			}
			return binary.LittleEndian.Uint32(data), nil
		case execError:
			return 1, errors.New(string(data))
		default:
			return 1, errors.New("invalid exec response")
		}
		if err != nil {
			return 1, err
		}
	}
}

// execRuntime borrows launchContainer's resources. Close joins all workers before
// their tokens, mounts, registry, relays, and write watcher can be released.
type execRuntime struct {
	commitContext       *context.Context
	commitRecord        *store.ContainerRecord
	capturing           bool
	mu                  sync.Mutex
	wg                  sync.WaitGroup
	listener            *win32.PipeListener
	closing             bool
	paused              bool
	runID               string
	primary, job, token win32.HANDLE
	security            *win32.SECURITY_CAPABILITIES
	env                 []environment.Var
	cwd, systemRoot     string
	placeholders        oci.Placeholders
	connections         map[*execStream]struct{}
	children            map[*process.Child]struct{}
}

func (r *execRuntime) start(id, runID string) error {
	l, err := win32.ListenPipe(execPipeName(id, runID))
	if err != nil {
		return err
	}
	r.listener, r.runID = l, runID
	r.connections = make(map[*execStream]struct{})
	r.children = make(map[*process.Child]struct{})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			file, err := l.Accept()
			if err != nil {
				return
			}
			c := &execStream{File: file}
			r.mu.Lock()
			if r.closing {
				r.mu.Unlock()
				_ = c.Close()
				return
			}
			r.connections[c] = struct{}{}
			r.wg.Add(1)
			r.mu.Unlock()
			go func() { defer r.wg.Done(); r.serve(c) }()
		}
	}()
	return nil
}

// stop prevents launches and cancels I/O without waiting for worker cleanup.
// KILL must acknowledge promptly; the primary-exit path then joins workers.
func (r *execRuntime) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
}

func (r *execRuntime) stopLocked() {
	if r.closing {
		return
	}
	r.closing = true
	if r.listener != nil {
		_ = r.listener.Close()
	}
	for c := range r.connections {
		_ = c.Close()
	}
	for child := range r.children {
		win32.TerminateJobObject(child.Job, 1)
	}
}

func (r *execRuntime) Close() {
	r.stop()
	r.wg.Wait()
}

func (r *execRuntime) command(req execRequest) (process.SpawnSpec, error) {
	if req.Protocol != execProtocol || req.RunID != r.runID {
		return process.SpawnSpec{}, errors.New("exec protocol or container run changed")
	}
	if len(req.Args) == 0 || req.Args[0] == "" {
		return process.SpawnSpec{}, errors.New("exec requires a command")
	}
	for _, arg := range req.Args {
		if strings.ContainsRune(arg, 0) {
			return process.SpawnSpec{}, errors.New("exec argument contains NUL")
		}
	}
	env := append([]environment.Var(nil), r.env...)
	for name, value := range req.Env {
		if name == "" || strings.ContainsAny(name, "=\x00") || (value != nil && strings.ContainsRune(*value, 0)) {
			return process.SpawnSpec{}, errors.New("invalid exec environment setting")
		}
		if value != nil {
			environment.Set(&env, name, *value)
		} else {
			kept := env[:0]
			for _, kv := range env {
				if !strings.EqualFold(kv.Name, name) {
					kept = append(kept, kv)
				}
			}
			env = kept
		}
	}
	cwd := r.cwd
	if req.Workdir != "" {
		cwd = r.placeholders.Expand(req.Workdir)
		if !filepath.IsAbs(cwd) {
			if filepath.VolumeName(cwd) != "" || strings.HasPrefix(cwd, `\`) || strings.HasPrefix(cwd, "/") {
				return process.SpawnSpec{}, errors.New("exec workdir must be absolute or an unambiguous relative path")
			}
			cwd = filepath.Join(r.cwd, cwd)
		}
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return process.SpawnSpec{}, fmt.Errorf("exec workdir: %w", err)
	}
	if !info.IsDir() {
		return process.SpawnSpec{}, errors.New("exec workdir is not a directory")
	}
	name := req.Args[0]
	if oci.IsPlaceholderPath(name) {
		name = r.placeholders.Expand(name)
	}
	search := []string{r.placeholders.App}
	for _, kv := range env {
		if strings.EqualFold(kv.Name, "PATH") {
			for _, dir := range filepath.SplitList(kv.Value) {
				if dir != "" {
					if !filepath.IsAbs(dir) {
						dir = filepath.Join(cwd, dir)
					}
					search = append(search, dir)
				}
			}
			break
		}
	}
	// System32 is already part of the effective PATH; an override may remove it.
	executable, ok := findExecCommand(cwd, name, search)
	if !ok {
		return process.SpawnSpec{}, fmt.Errorf("exec command %q was not found", req.Args[0])
	}
	cmd := newCommand(executable, req.Args[1:])
	line := process.BuildCommandLine(executable, cmd.Args)
	if cmd.Batch {
		executable = filepath.Join(r.systemRoot, "System32", "cmd.exe")
		line, err = process.BuildBatchCommandLine(executable, cmd.Exe, cmd.Args)
		if err != nil {
			return process.SpawnSpec{}, err
		}
	}
	block, err := environment.EncodeBlock(env)
	if err != nil {
		return process.SpawnSpec{}, err
	}
	return process.SpawnSpec{
		Exe: executable, CommandLine: line, EnvBlock: block, Cwd: cwd,
		Security: r.security, Token: r.token, ParentJobs: []win32.HANDLE{r.job},
		CreationFlags:          win32.CREATE_NEW_PROCESS_GROUP,
		PreserveConsoleHandler: true,
	}, nil
}

func findExecCommand(cwd, name string, search []string) (string, bool) {
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		if !filepath.IsAbs(name) {
			name = filepath.Join(cwd, name)
		}
		info, err := os.Stat(name)
		return name, err == nil && !info.IsDir()
	}
	exts := []string{""}
	if filepath.Ext(name) == "" {
		exts = []string{".exe", ".com", ".cmd", ".bat"}
	}
	for _, dir := range append([]string{cwd}, search...) {
		for _, ext := range exts {
			path := filepath.Join(dir, name+ext)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path, true
			}
		}
	}
	return "", false
}

func (r *execRuntime) recordExecEvent(action, id string, attrs map[string]string) {
	if r.commitContext == nil || r.commitRecord == nil {
		return
	}
	if attrs == nil {
		attrs = make(map[string]string)
	}
	attrs["execID"] = id
	r.commitContext.Store.RecordContainerEvent(r.commitRecord, action, attrs)
}

func (r *execRuntime) serve(c *execStream) {
	defer func() {
		_ = c.Close()
		r.mu.Lock()
		delete(r.connections, c)
		r.mu.Unlock()
	}()
	_ = c.SetDeadline(time.Now().Add(execStartupTimeout))
	kind, data, err := c.receive()
	if err != nil {
		return
	}
	if kind == commitAcquire {
		r.serveCapture(c, data)
		return
	}
	if kind == statsQuery {
		r.serveStats(c, data)
		return
	}
	var req execRequest
	if kind != execLaunch || json.Unmarshal(data, &req) != nil {
		_ = c.send(execError, []byte("invalid exec request"))
		return
	}
	spec, err := r.command(req)
	if err != nil {
		_ = c.send(execError, []byte(err.Error()))
		return
	}
	var files []*os.File
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	var stdio [3]win32.HANDLE
	var input *os.File
	var outputs [2]*os.File
	for i := range stdio {
		if req.Detach || (i == 0 && !req.Interactive) {
			access := os.O_WRONLY
			if i == 0 {
				access = os.O_RDONLY
			}
			f, e := os.OpenFile(`\\.\NUL`, access, 0)
			if e != nil {
				_ = c.send(execError, []byte(e.Error()))
				return
			}
			files = append(files, f)
			stdio[i] = win32.HANDLE(f.Fd())
		} else {
			read, write, e := os.Pipe()
			if e != nil {
				_ = c.send(execError, []byte(e.Error()))
				return
			}
			files = append(files, read, write)
			switch i {
			case 0:
				stdio[0], input = win32.HANDLE(read.Fd()), write
			case 1:
				stdio[1], outputs[0] = win32.HANDLE(write.Fd()), read
			case 2:
				stdio[2], outputs[1] = win32.HANDLE(write.Fd()), read
			}
		}
	}
	spec.Stdio = &stdio
	execID, err := newRunID()
	if err != nil {
		_ = c.send(execError, []byte(err.Error()))
		return
	}
	r.mu.Lock()
	if r.closing || win32.WaitForSingleObject(r.primary, 0) != win32.WAIT_TIMEOUT {
		r.mu.Unlock()
		_ = c.send(execError, []byte("container is stopping"))
		return
	}
	if r.paused {
		r.mu.Unlock()
		_ = c.send(execError, []byte("container is paused; unpause it first"))
		return
	}
	r.recordExecEvent("exec_create", execID, nil)
	child, err := process.Start(spec)
	if err == nil {
		r.children[child] = struct{}{}
		r.recordExecEvent("exec_start", execID, nil)
	}
	r.mu.Unlock()
	if err != nil {
		_ = c.send(execError, []byte(err.Error()))
		return
	}
	defer func() {
		process.EndJob(child.Job)
		win32.WaitForSingleObject(child.Process, win32.INFINITE)
		attrs := make(map[string]string)
		if code, err := win32.GetExitCodeProcess(child.Process); err == nil {
			attrs["exitCode"] = strconv.FormatUint(uint64(code), 10)
		}
		r.recordExecEvent("exec_die", execID, attrs)
		r.mu.Lock()
		delete(r.children, child)
		child.Close()
		r.mu.Unlock()
	}()
	// Close our copies of the child's pipe endpoints, allowing EOF to propagate.
	for _, f := range files {
		for _, h := range stdio {
			if win32.HANDLE(f.Fd()) == h {
				_ = f.Close()
				break
			}
		}
	}
	if err := c.send(execStarted, nil); err != nil {
		return
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		return
	}
	if req.Detach {
		_ = c.Close()
		win32.WaitForSingleObject(child.Process, win32.INFINITE)
		return
	}
	cancel := func() { win32.TerminateJobObject(child.Job, 1) }
	var outputWG, inputWG sync.WaitGroup
	for i, output := range outputs {
		outputWG.Add(1)
		go func() {
			defer outputWG.Done()
			buf := make([]byte, 64*1024)
			for {
				n, err := output.Read(buf)
				if n > 0 && c.send(byte(int(execStdout)+i), buf[:n]) != nil {
					cancel()
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	// One acknowledged stdin frame at a time keeps backpressure bounded while
	// leaving the reader free to observe cancellation or a disconnected caller.
	queue := make(chan []byte, 1)
	var inputPending atomic.Bool
	if input != nil {
		inputWG.Add(1)
		go func() {
			defer inputWG.Done()
			defer func() { _ = input.Close() }()
			for chunk := range queue {
				if _, err := input.Write(chunk); err != nil {
					return
				}
				inputPending.Store(false)
				if c.send(execInputAck, nil) != nil {
					cancel()
					return
				}
			}
		}()
	}
	inputWG.Add(1)
	go func() {
		defer inputWG.Done()
		defer close(queue)
		eof := input == nil
		for {
			kind, data, err := c.receive()
			if err != nil {
				cancel()
				return
			}
			switch kind {
			case execStdin:
				if eof || !inputPending.CompareAndSwap(false, true) {
					cancel()
					return
				}
				queue <- data
			case execEOF:
				if eof || inputPending.Load() {
					cancel()
					return
				}
				eof = true
				_ = input.Close()
			case execCancel:
				cancel()
				return
			default:
				cancel()
				return
			}
		}
	}()
	win32.WaitForSingleObject(child.Process, win32.INFINITE)
	code, waitErr := win32.GetExitCodeProcess(child.Process)
	process.EndJob(child.Job)
	outputWG.Wait()
	if waitErr == nil {
		var status [4]byte
		binary.LittleEndian.PutUint32(status[:], uint32(code))
		_ = c.send(execExit, status[:])
	} else {
		_ = c.send(execError, []byte(waitErr.Error()))
	}
	_ = c.Close()
	inputWG.Wait()
}
