package format

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quasidock/quasidock/internal/oci"
)

const (
	MaxExtractWorkers = 4
	GzipLevel         = 6
)

var (
	ErrGzipTrailingGarbage   = errors.New("gzip trailing garbage")
	ErrTarBadPath            = errors.New("tar bad path")
	ErrTarSymlinkUnsupported = errors.New("tar symlink unsupported")
)

// GzipWriter creates a deterministic gzip writer using level 6 compression,
// zero timestamp, and OS unknown (255).
func GzipWriter(w io.Writer) (*gzip.Writer, error) {
	gw, err := gzip.NewWriterLevel(w, GzipLevel)
	if err != nil {
		return nil, err
	}
	gw.Header.ModTime = time.Unix(0, 0).UTC()
	gw.Header.OS = 255
	return gw, nil
}

// Gunzip decompresses all concatenated gzip members from in to out,
// tolerating trailing zero-byte padding, but rejecting trailing non-zero garbage.
func Gunzip(in io.Reader, out io.Writer) error {
	br := bufio.NewReaderSize(in, 64*1024)
	zr, err := gzip.NewReader(br)
	if err != nil {
		return err
	}

	for {
		zr.Multistream(false)
		if _, err := io.Copy(out, zr); err != nil {
			return err
		}

		// Check for another member or padding.
		hasMore, err := checkAnotherGzipMember(br)
		if err != nil {
			return err
		}
		if !hasMore {
			return nil
		}

		if err := zr.Reset(br); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func checkAnotherGzipMember(br *bufio.Reader) (bool, error) {
	magic, err := br.Peek(2)
	if err == nil {
		if magic[0] == 0x1f && magic[1] == 0x8b {
			return true, nil
		}
	} else if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}

	// Scan through remaining bytes to verify they are all zero padding.
	for {
		b, err := br.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
		if b != 0 {
			return false, ErrGzipTrailingGarbage
		}
	}
}

// Inflate decompresses the gzip-compressed layer at gzPath into tarPath.
func Inflate(gzPath, tarPath string) error {
	in, err := os.Open(gzPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(tarPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	bw := bufio.NewWriterSize(out, 64*1024)
	if err := Gunzip(in, bw); err != nil {
		return err
	}
	return bw.Flush()
}

// LayerWriter writes layer entries with canonical metadata.
type LayerWriter struct {
	tw *tar.Writer
}

func NewLayerWriter(w io.Writer) *LayerWriter {
	return &LayerWriter{tw: tar.NewWriter(w)}
}

// AddDir adds a directory entry with mode 0755 and zero timestamp.
func (w *LayerWriter) AddDir(path string) error {
	clean := strings.TrimSuffix(path, "/") + "/"
	hdr := &tar.Header{
		Name:     clean,
		Mode:     0o755,
		Typeflag: tar.TypeDir,
		ModTime:  time.Unix(0, 0).UTC(),
		Format:   tar.FormatPAX,
	}
	return w.tw.WriteHeader(hdr)
}

// AddFile streams exactly size bytes from content into the layer.
func (w *LayerWriter) AddFile(path string, size int64, content io.Reader) error {
	hdr := &tar.Header{
		Name:     strings.TrimPrefix(path, "/"),
		Mode:     0o644,
		Size:     size,
		Typeflag: tar.TypeReg,
		ModTime:  time.Unix(0, 0).UTC(),
		Format:   tar.FormatPAX,
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return err
	}
	written, err := io.Copy(w.tw, content)
	if err != nil {
		return err
	}
	if written != size {
		return fmt.Errorf("short write for %s: expected %d, got %d", path, size, written)
	}
	return nil
}

// Finish closes the tar archive, writing end-of-archive blocks.
func (w *LayerWriter) Finish() error {
	return w.tw.Close()
}

type fileEntry struct {
	name   string
	offset int64
	size   int64
}

// Extract extracts the tar file at tarPath into destPath.
func Extract(tarPath, destPath string) error {
	if err := os.MkdirAll(destPath, 0o755); err != nil {
		return err
	}

	files, err := indexAndCreateDirs(tarPath, destPath)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}

	numWorkers := runtime.NumCPU()
	if numWorkers > MaxExtractWorkers {
		numWorkers = MaxExtractWorkers
	}
	if numWorkers > len(files) {
		numWorkers = len(files)
	}
	if numWorkers < 1 {
		numWorkers = 1
	}

	var next atomic.Int64
	var firstErr error
	var errOnce sync.Once
	setErr := func(err error) {
		if err != nil {
			errOnce.Do(func() {
				firstErr = err
				next.Store(int64(len(files)))
			})
		}
	}

	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for w := 0; w < numWorkers; w++ {
		go func() {
			defer wg.Done()
			f, err := os.Open(tarPath)
			if err != nil {
				setErr(err)
				return
			}
			defer f.Close()

			buf := make([]byte, 64*1024)
			for {
				idx := next.Add(1) - 1
				if idx >= int64(len(files)) {
					return
				}
				entry := files[idx]

				if _, err := f.Seek(entry.offset, io.SeekStart); err != nil {
					setErr(err)
					return
				}

				targetPath := filepath.Join(destPath, filepath.FromSlash(entry.name))
				out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
				if err != nil {
					setErr(err)
					return
				}

				_, err = io.CopyBuffer(out, io.LimitReader(f, entry.size), buf)
				closeErr := out.Close()
				if err != nil {
					setErr(err)
					return
				}
				if closeErr != nil {
					setErr(closeErr)
					return
				}
			}
		}()
	}
	wg.Wait()

	return firstErr
}

type countingReader struct {
	r   io.Reader
	pos int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.pos += int64(n)
	return n, err
}

func indexAndCreateDirs(tarPath, destPath string) ([]fileEntry, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cr := &countingReader{r: bufio.NewReaderSize(f, 64*1024)}
	tr := tar.NewReader(cr)

	createdDirs := make(map[string]struct{})
	var files []fileEntry

	for {
		hdr, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}

		cleanName := strings.Trim(hdr.Name, "/")
		if !oci.IsSafeRelPath(cleanName) {
			return nil, ErrTarBadPath
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := createDirOnce(destPath, createdDirs, cleanName); err != nil {
				return nil, err
			}
		case tar.TypeReg, tar.TypeRegA:
			parent := filepath.ToSlash(filepath.Dir(cleanName))
			if parent != "" && parent != "." {
				if err := createDirOnce(destPath, createdDirs, parent); err != nil {
					return nil, err
				}
			}
			// When tr.Next() returns, the underlying reader is positioned at the start of
			// the file data blocks. Note cr.pos is the logical offset in the tar.
			files = append(files, fileEntry{
				name:   cleanName,
				offset: cr.pos,
				size:   hdr.Size,
			})
		case tar.TypeSymlink:
			return nil, ErrTarSymlinkUnsupported
		}
	}

	return files, nil
}

func createDirOnce(destPath string, created map[string]struct{}, relDir string) error {
	if _, ok := created[relDir]; ok {
		return nil
	}
	full := filepath.Join(destPath, filepath.FromSlash(relDir))
	if err := os.MkdirAll(full, 0o755); err != nil {
		return err
	}
	created[relDir] = struct{}{}
	return nil
}
