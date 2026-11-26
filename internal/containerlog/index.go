// Package containerlog records capture times alongside byte-preserving logs.
package containerlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const indexVersion = 1

// IndexPath returns the metadata file associated with a raw container log.
func IndexPath(path string) string { return path + ".index.jsonl" }

// Span describes one serialized write to the raw log. Gaps have no known time.
type Span struct {
	Offset int64     `json:"offset"`
	Length int64     `json:"length"`
	Time   time.Time `json:"time"`
}

type indexHeader struct {
	Version int `json:"version"`
}

// IndexReader retains a cursor at the last complete, validated metadata record.
type IndexReader struct {
	path        string
	offset, end int64
	header      bool
}

func NewIndexReader(path string) *IndexReader { return &IndexReader{path: IndexPath(path)} }

// Batch is a fixed metadata snapshot. Size is sampled from the raw log after
// the index, so every published span must fit within it. Close releases the file.
type Batch struct {
	Size       int64
	Present    bool
	Incomplete bool
	reader     *IndexReader
	file       *os.File
	lines      *bufio.Reader
}

func (r *IndexReader) Read(raw *os.File) (*Batch, error) {
	b := &Batch{reader: r}
	f, err := os.Open(r.path)
	if err == nil {
		info, statErr := f.Stat()
		if statErr != nil {
			_ = f.Close()
			return nil, statErr
		}
		if info.Size() < r.offset {
			_ = f.Close()
			return nil, errors.New("container log index was truncated while reading")
		}
		b.file, b.Present = f, true
		b.lines = bufio.NewReader(io.NewSectionReader(f, r.offset, info.Size()-r.offset))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if r.header {
		return nil, errors.New("container log index disappeared while reading")
	}
	info, err := raw.Stat()
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	b.Size = info.Size()
	if r.end > b.Size {
		_ = b.Close()
		return nil, errors.New("container log was truncated while reading")
	}
	return b, nil
}

func (b *Batch) Next() (Span, error) {
	if b.lines == nil {
		return Span{}, io.EOF
	}
	for {
		// Metadata has a bounded size, independently of application line length.
		line, err := b.lines.ReadSlice('\n')
		if errors.Is(err, io.EOF) {
			b.Incomplete = len(line) > 0
			return Span{}, io.EOF
		}
		if err != nil {
			return Span{}, fmt.Errorf("reading container log index: %w", err)
		}
		if !b.reader.header {
			var header indexHeader
			if err := json.Unmarshal(line, &header); err != nil {
				return Span{}, fmt.Errorf("invalid container log index header: %w", err)
			}
			if header.Version != indexVersion {
				return Span{}, fmt.Errorf("unsupported container log index version %d", header.Version)
			}
			b.reader.header = true
			b.reader.offset += int64(len(line))
			continue
		}
		var span Span
		if err := json.Unmarshal(line, &span); err != nil {
			return Span{}, fmt.Errorf("invalid container log index record: %w", err)
		}
		if span.Offset < b.reader.end || span.Length <= 0 || span.Offset > b.Size || span.Length > b.Size-span.Offset || span.Time.IsZero() {
			return Span{}, errors.New("invalid container log index span")
		}
		b.reader.offset += int64(len(line))
		b.reader.end = span.Offset + span.Length
		return span, nil
	}
}

func (b *Batch) Close() error {
	if b.file != nil {
		return b.file.Close()
	}
	return nil
}
