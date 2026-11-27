// Package linkrelay defines the control and stream protocols shared by the
// unsandboxed link brokers and their job-scoped relay processes.
package linkrelay

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/rpc"
	"sync"
	"time"
)

const (
	MaxFrame    = 16 * 1024
	Timeout     = 3 * time.Second
	StreamStart = 1
)

type Route struct {
	Alias string
	Port  uint16
}

type Endpoint struct {
	Route
	Listen uint16
}

type DialRequest struct {
	SourceID string
	RunID    string
	Route
}

type Socket struct {
	ID            uint64
	Local, Remote netip.AddrPort
}

type OpenRequest struct {
	Remote netip.AddrPort
}

type AttachRequest struct {
	ID     uint64
	Handle uintptr
}

func Call(client *rpc.Client, method string, args, reply any) error {
	call := client.Go(method, args, reply, make(chan *rpc.Call, 1))
	timer := time.NewTimer(Timeout + time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		_ = client.Close()
		return fmt.Errorf("link relay did not acknowledge %s", method)
	case result := <-call.Done:
		return result.Error
	}
}

func WriteFrame(w io.Writer, data []byte) error {
	if len(data) > MaxFrame {
		return fmt.Errorf("link frame exceeds %d bytes", MaxFrame)
	}
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	_, err := w.Write(data)
	return err
}

func ReadFrame(r io.Reader, buf []byte) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(header[:])
	if size > uint32(len(buf)) || size > MaxFrame {
		return nil, fmt.Errorf("invalid link frame size %d", size)
	}
	data := buf[:size]
	_, err := io.ReadFull(r, data)
	return data, err
}

func WriteRequest(w io.Writer, request DialRequest) error {
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return WriteFrame(w, data)
}

func ReadRequest(r io.Reader) (DialRequest, error) {
	var request DialRequest
	var buf [MaxFrame]byte
	data, err := ReadFrame(r, buf[:])
	if err != nil {
		return request, err
	}
	err = json.Unmarshal(data, &request)
	return request, err
}

// Stream owns one TCP connection and an already-connected pipe.
// Shutdown cancels both pumps.
type Stream struct {
	TCP  *net.TCPConn
	Pipe io.ReadWriteCloser
	once sync.Once
}

func (s *Stream) Shutdown() {
	s.once.Do(func() {
		_ = s.TCP.Close()
		_ = s.Pipe.Close()
	})
}

func (s *Stream) Run() {
	defer s.Shutdown()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var buf [MaxFrame]byte
		for {
			n, err := s.TCP.Read(buf[:])
			if n > 0 && WriteFrame(s.Pipe, buf[:n]) != nil {
				s.Shutdown()
				return
			}
			if err != nil {
				if err != io.EOF || WriteFrame(s.Pipe, nil) != nil {
					s.Shutdown()
				}
				return
			}
		}
	}()
	var buf [MaxFrame]byte
	for {
		data, err := ReadFrame(s.Pipe, buf[:])
		if err != nil {
			s.Shutdown()
			break
		}
		if len(data) == 0 {
			_ = s.TCP.CloseWrite()
			break
		}
		if _, err := s.TCP.Write(data); err != nil {
			s.Shutdown()
			break
		}
	}
	<-done
}
