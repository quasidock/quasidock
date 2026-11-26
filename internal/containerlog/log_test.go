package containerlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readSpans(t *testing.T, path string, reader *IndexReader) ([]Span, bool) {
	t.Helper()
	raw, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	batch, err := reader.Read(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	var spans []Span
	for {
		span, err := batch.Next()
		if errors.Is(err, io.EOF) {
			return spans, batch.Incomplete
		}
		if err != nil {
			t.Fatal(err)
		}
		spans = append(spans, span)
	}
}

func TestConcurrentCaptureAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.log")
	legacy := []byte("legacy\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path, func(err error) { t.Errorf("capture warning: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(123, 7)
	w.now = func() time.Time { return stamp }
	var wg sync.WaitGroup
	for worker := range 2 {
		wg.Go(func() {
			for i := range 40 {
				if _, err := fmt.Fprintf(w, "%d:%d 日本語\r\n", worker, i); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		})
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = Open(path, func(err error) { t.Errorf("restart warning: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return stamp.Add(time.Second) }
	final := []byte{'f', 'i', 'n', 'a', 'l', 0, 0xff}
	if _, err := w.Write(final); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	spans, incomplete := readSpans(t, path, NewIndexReader(path))
	if incomplete || len(spans) != 81 || !bytes.HasPrefix(data, legacy) || !bytes.HasSuffix(data, final) {
		t.Fatalf("capture: %d spans, incomplete=%t, data=%q", len(spans), incomplete, data)
	}
	offset := int64(len(legacy))
	seen := map[string]bool{}
	for i, span := range spans {
		wantTime := stamp
		if i == 80 {
			wantTime = stamp.Add(time.Second)
		}
		if span.Offset != offset || !span.Time.Equal(wantTime) {
			t.Fatalf("span %d: %+v, expected offset %d at %v", i, span, offset, wantTime)
		}
		value := string(data[span.Offset : span.Offset+span.Length])
		if seen[value] {
			t.Fatalf("duplicate captured write: %q", value)
		}
		seen[value] = true
		offset += span.Length
	}
	if offset != int64(len(data)) {
		t.Fatalf("unindexed capture: end=%d size=%d", offset, len(data))
	}
}

func TestMetadataFailurePreservesRawOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.log")
	var warnings []error
	w, err := Open(path, func(err error) { warnings = append(warnings, err) })
	if err != nil {
		t.Fatal(err)
	}
	if err := w.index.Close(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"first\n", "second\n"} {
		if n, err := w.Write([]byte(value)); err != nil || n != len(value) {
			t.Fatalf("metadata failure affected raw output: %d %v", n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "first\nsecond\n" || len(warnings) != 1 {
		t.Fatalf("raw=%q warnings=%v err=%v", data, warnings, err)
	}
}

func TestIndexSnapshotsPartialPublicationAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.log")
	w, err := Open(path, func(err error) { t.Errorf("warning: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	raw, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	r := NewIndexReader(path)
	batch, err := r.Read(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := batch.Next(); !errors.Is(err, io.EOF) || batch.Size != 0 {
		t.Fatalf("snapshot included a later append: size=%d err=%v", batch.Size, err)
	}
	batch.Close()
	spans, incomplete := readSpans(t, path, r)
	if incomplete || len(spans) != 1 || spans[0].Length != 6 {
		t.Fatalf("next snapshot: %+v incomplete=%t", spans, incomplete)
	}
	// Publish raw bytes followed by a metadata record in two writes.
	if _, err := w.raw.Write([]byte("next")); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(Span{Offset: 6, Length: 4, Time: time.Unix(10, 1)})
	if _, err := w.index.Write(record[:len(record)/2]); err != nil {
		t.Fatal(err)
	}
	spans, incomplete = readSpans(t, path, r)
	if !incomplete || len(spans) != 0 {
		t.Fatalf("consumed partial index: %+v incomplete=%t", spans, incomplete)
	}
	if _, err := w.index.Write(append(record[len(record)/2:], '\n')); err != nil {
		t.Fatal(err)
	}
	spans, incomplete = readSpans(t, path, r)
	if incomplete || len(spans) != 1 || spans[0].Offset != 6 {
		t.Fatalf("lost completed index: %+v incomplete=%t", spans, incomplete)
	}
	// A crashed writer's incomplete record can be discarded, leaving its raw
	// bytes untimestamped. A new writer appends after that gap.
	if _, err := w.index.Write([]byte("{\"offset\":")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(path, func(err error) { t.Errorf("recovery warning: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Write([]byte("again\n")); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	spans, incomplete = readSpans(t, path, NewIndexReader(path))
	if incomplete || len(spans) != 3 || spans[2].Offset != 10 {
		t.Fatalf("recovery: %+v incomplete=%t", spans, incomplete)
	}
}

func TestCorruptIndexesAreNotOverwritten(t *testing.T) {
	for _, metadata := range []string{
		"{\"version\":2}\n",
		"garbage\n",
		"{\"version\":1}\n{}\n",
		"{\"version\":1}\n{\"offset\":0,\"length\":100,\"time\":\"2026-01-01T00:00:00Z\"}\n",
	} {
		t.Run(strings.TrimSpace(metadata), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "container.log")
			if err := os.WriteFile(IndexPath(path), []byte(metadata), 0o600); err != nil {
				t.Fatal(err)
			}
			warnings := 0
			w, err := Open(path, func(error) { warnings++ })
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte("still captured\n")); err != nil {
				t.Fatal(err)
			}
			w.Close()
			data, err := os.ReadFile(IndexPath(path))
			if err != nil || string(data) != metadata || warnings != 1 {
				t.Fatalf("metadata=%q warnings=%d err=%v", data, warnings, err)
			}
		})
	}
}
