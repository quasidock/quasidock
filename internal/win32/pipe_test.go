package win32_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/win32"
)

func TestPrivatePipeStreamsAndCancellation(t *testing.T) {
	name := fmt.Sprintf(`\\.\pipe\quasidock-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	l, err := win32.ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if other, err := win32.ListenPipe(name); err == nil {
		_ = other.Close()
		t.Fatal("allowed duplicate listener")
	}
	server := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			server <- err
			return
		}
		defer func() { _ = c.Close() }()
		if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			server <- err
			return
		}
		_, err = io.Copy(c, io.LimitReader(c, 1024*1024))
		server <- err
	}()
	c, err := win32.DialPipe(name, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 1024*1024)
	written := make(chan error, 1)
	go func() { _, err := c.Write(payload); written <- err }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("corrupt pipe stream")
	}
	go func() {
		file, err := l.Accept()
		if file != nil {
			_ = file.Close()
		}
		server <- err
	}()
	_ = l.Close()
	select {
	case err := <-server:
		if err == nil {
			t.Fatal("closed accept succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel Accept")
	}
}
