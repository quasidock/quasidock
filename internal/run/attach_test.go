package run

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/win32"
)

func TestDetachKeysAndStreamingFilter(t *testing.T) {
	for _, value := range []string{"", "ctrl-p,ctrl-q", "X,ctrl-@,ctrl-[,ctrl-\\,ctrl-],ctrl-^,ctrl-_"} {
		if _, err := parseDetachKeys(value); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
	}
	for _, value := range []string{"ctrl-", "ctrl-A", "escape", "ctrl-p,", ",a", "1"} {
		if _, err := parseDetachKeys(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	f := detachFilter{keys: []byte{16, 17}}
	if out, detached := f.feed([]byte{'a', 16}); string(out) != "a" || detached {
		t.Fatalf("prefix: %v %t", out, detached)
	}
	if out, detached := f.feed([]byte{16, 17, 'z'}); !bytes.Equal(out, []byte{16}) || !detached {
		t.Fatalf("overlap: %v %t", out, detached)
	}
	f = detachFilter{keys: []byte("aba")}
	if out, detached := f.feed([]byte("aaab")); string(out) != "aa" || detached {
		t.Fatalf("mismatch: %q %t", out, detached)
	}
	if out, detached := f.feed([]byte("a")); len(out) != 0 || !detached {
		t.Fatalf("split: %q %t", out, detached)
	}
}

func attachTestRuntime(t *testing.T, stdin bool) (*attachRuntime, string, string) {
	t.Helper()
	id, _ := newRunID()
	runID, _ := newRunID()
	id += id
	r, err := newAttachRuntime(id, runID, stdin, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.finish(0, nil) })
	return r, id, runID
}

func attachTestDial(t *testing.T, id, runID string, stdin bool) (*framedStream, byte, []byte) {
	t.Helper()
	f, err := win32.DialPipe(attachPipeName(id, runID), time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	c := &framedStream{File: f}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(attachRequest{Protocol: attachProtocol, RunID: runID, Stdin: stdin})
	if err := c.send(attachHello, data); err != nil {
		t.Fatal(err)
	}
	kind, data, err := c.receive()
	if err != nil {
		t.Fatal(err)
	}
	return c, kind, data
}

func expectAttachFrame(t *testing.T, c *framedStream, want byte) []byte {
	t.Helper()
	kind, data, err := c.receive()
	if err != nil || kind != want {
		t.Fatalf("frame: got %d %q %v, want %d", kind, data, err, want)
	}
	return data
}

func TestAttachStreamsWriterLeaseAndEOF(t *testing.T) {
	r, id, runID := attachTestRuntime(t, true)
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	r.setInput(writer)
	c, kind, _ := attachTestDial(t, id, runID, true)
	if kind != attachReady {
		t.Fatalf("handshake: %d", kind)
	}
	r.markStarted(func() error { return nil })
	expectAttachFrame(t, c, attachStarted)
	_, kind, data := attachTestDial(t, id, runID, true)
	if kind != attachError || !strings.Contains(string(data), "--no-stdin") {
		t.Fatalf("competing writer: %d %s", kind, data)
	}
	viewer, kind, _ := attachTestDial(t, id, runID, false)
	if kind != attachReady {
		t.Fatal(kind)
	}
	expectAttachFrame(t, viewer, attachStarted)
	if err := c.send(attachStdin, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(input, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("stdin: %s %v", buf, err)
	}
	expectAttachFrame(t, c, attachInputAck)
	_ = c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		released := r.writer == nil
		r.mu.Unlock()
		if released {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detach retained writer")
		}
		time.Sleep(time.Millisecond)
	}
	c, kind, _ = attachTestDial(t, id, runID, true)
	if kind != attachReady {
		t.Fatal(kind)
	}
	expectAttachFrame(t, c, attachStarted)
	if err := c.send(attachEOF, nil); err != nil {
		t.Fatal(err)
	}
	expectAttachFrame(t, c, attachInputAck)
	if n, err := input.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("EOF: %d %v", n, err)
	}
	readOnly, kind, data := attachTestDial(t, id, runID, true)
	var response attachResponse
	if kind != attachReady || json.Unmarshal(data, &response) != nil || response.Stdin {
		t.Fatalf("closed stdin: %d %s", kind, data)
	}
	expectAttachFrame(t, readOnly, attachStarted)
	if _, err := (attachWriter{r, attachStdout}).Write([]byte("out")); err != nil {
		t.Fatal(err)
	}
	if _, err := (attachWriter{r, attachStderr}).Write([]byte("err")); err != nil {
		t.Fatal(err)
	}
	for _, conn := range []*framedStream{c, viewer, readOnly} {
		if string(expectAttachFrame(t, conn, attachStdout)) != "out" || string(expectAttachFrame(t, conn, attachStderr)) != "err" {
			t.Fatal("stream contents")
		}
	}
	done := make(chan struct{})
	go func() { r.finish(37, nil); close(done) }()
	for _, conn := range []*framedStream{c, viewer, readOnly} {
		if code := binary.LittleEndian.Uint32(expectAttachFrame(t, conn, attachExit)); code != 37 {
			t.Fatal(code)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("completion blocked")
	}
}

func TestAttachMalformedFramesAndBlockedIO(t *testing.T) {
	r, id, runID := attachTestRuntime(t, true)
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	r.setInput(writer)
	r.markStarted(func() error { return nil })
	dial := func() *framedStream {
		f, err := win32.DialPipe(attachPipeName(id, runID), time.Now().Add(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		c := &framedStream{File: f}
		t.Cleanup(func() { _ = c.Close() })
		if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, request := range [][]byte{[]byte("invalid"), []byte(`{"Protocol":1,"RunID":"stale"}`)} {
		c := dial()
		if err := c.send(attachHello, request); err != nil {
			t.Fatal(err)
		}
		expectAttachFrame(t, c, attachError)
	}
	oversized := dial()
	var header [5]byte
	binary.LittleEndian.PutUint32(header[1:], maxExecFrame+1)
	if _, err := oversized.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := oversized.receive(); err == nil {
		t.Fatal("accepted oversized frame")
	}
	c, kind, _ := attachTestDial(t, id, runID, true)
	if kind != attachReady {
		t.Fatal(kind)
	}
	expectAttachFrame(t, c, attachStarted)
	if err := c.send(attachStdin, bytes.Repeat([]byte("x"), 64*1024)); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.receive(); !os.IsTimeout(err) {
		t.Fatalf("input did not block: %v", err)
	}
	_ = c.Close()
	_ = dial() // Client that never completes its handshake.
	done := make(chan struct{})
	go func() { r.finish(0, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown blocked on input or handshake")
	}
}

func TestAttachSlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	r, id, runID := attachTestRuntime(t, false)
	r.markStarted(func() error { return nil })
	c, kind, _ := attachTestDial(t, id, runID, false)
	if kind != attachReady {
		t.Fatal(kind)
	}
	expectAttachFrame(t, c, attachStarted)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_, _ = attachWriter{r, attachStdout}.Write(bytes.Repeat([]byte("x"), 32*1024))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slow viewer blocked publisher")
	}
	r.mu.Lock()
	remaining := len(r.sessions)
	r.mu.Unlock()
	if remaining != 0 {
		t.Fatal("slow viewer retained")
	}
	viewer, kind, _ := attachTestDial(t, id, runID, false)
	if kind != attachReady {
		t.Fatal(kind)
	}
	expectAttachFrame(t, viewer, attachStarted)
	if _, err := (attachWriter{r, attachStdout}).Write([]byte("still running")); err != nil {
		t.Fatal(err)
	}
	if string(expectAttachFrame(t, viewer, attachStdout)) != "still running" {
		t.Fatal("new viewer missed output")
	}
	_ = viewer.Close()
}
