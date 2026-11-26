package containerlog

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

// Writer serializes raw output and publishes timestamps only after its bytes
// have been written. Metadata failures never prevent raw logging or attachment.
type Writer struct {
	mu     sync.Mutex
	raw    *os.File
	index  *os.File
	offset int64
	now    func() time.Time
	warn   func(error)
}

// Open requires exclusive runtime ownership of the container log. Only an
// incomplete terminal index record is discarded when recovering an old writer.
func Open(path string, warn func(error)) (*Writer, error) {
	raw, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := raw.Stat()
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	w := &Writer{raw: raw, offset: info.Size(), now: time.Now, warn: warn}
	if err := w.openIndex(path); err != nil {
		w.disableIndex(err)
	}
	return w, nil
}

func (w *Writer) openIndex(path string) error {
	f, err := os.OpenFile(IndexPath(path), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	w.index = f
	r := NewIndexReader(path)
	b, err := r.Read(w.raw)
	if err != nil {
		return err
	}
	defer func() { _ = b.Close() }()
	for {
		_, err := b.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if b.Incomplete {
		if err := f.Truncate(r.offset); err != nil {
			return err
		}
	}
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return err
	}
	if !r.header {
		return writeRecord(f, indexHeader{Version: indexVersion})
	}
	return nil
}

func writeRecord(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func (w *Writer) disableIndex(err error) {
	if w.index != nil {
		_ = w.index.Close()
		w.index = nil
	}
	if w.warn != nil {
		w.warn(err)
		w.warn = nil
	}
}

func (w *Writer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ts := w.now().UTC()
	n, err := w.raw.Write(data)
	if n > 0 {
		span := Span{Offset: w.offset, Length: int64(n), Time: ts}
		w.offset += int64(n)
		if w.index != nil {
			if indexErr := writeRecord(w.index, span); indexErr != nil {
				w.disableIndex(indexErr)
			}
		}
	}
	return n, err
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.index != nil {
		if err := w.index.Close(); err != nil {
			w.disableIndex(err)
		}
		w.index = nil
	}
	return w.raw.Close()
}
