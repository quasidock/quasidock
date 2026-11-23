package run

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
)

const maxStreamFrame = 1024 * 1024

// framedStream serializes writes shared by output, input acknowledgements, and
// control responses. Exec and attach retain independent message vocabularies.
type framedStream struct {
	*os.File
	mu sync.Mutex
}

func (s *framedStream) send(kind byte, data []byte) error {
	if len(data) > maxStreamFrame {
		return errors.New("stream frame is too large")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var header [5]byte
	header[0] = kind
	binary.LittleEndian.PutUint32(header[1:], uint32(len(data)))
	if _, err := s.Write(header[:]); err != nil {
		return err
	}
	// An empty frame is complete after its header. Even a zero-byte write may
	// fail if a detached exec caller closes immediately after acknowledgement.
	if len(data) == 0 {
		return nil
	}
	_, err := s.Write(data)
	return err
}

func (s *framedStream) receive() (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(s, header[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(header[1:])
	if n > maxStreamFrame {
		return 0, nil, errors.New("stream frame is too large")
	}
	data := make([]byte, n)
	_, err := io.ReadFull(s, data)
	return header[0], data, err
}
