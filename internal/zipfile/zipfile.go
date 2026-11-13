package zipfile

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/oci"
)

var (
	ErrUnsupportedCompression = errors.New("unsupported compression method")
	ErrZipBadFilename         = errors.New("zip bad filename")
	ErrZipBadDirectorySize    = errors.New("zip bad directory size")
)

// Entry represents an entry in a zip archive.
type Entry struct {
	// Name is '/'-separated, with no trailing slash.
	Name   string
	IsDir  bool
	Method uint16
	// Size is the uncompressed size.
	Size int64
	File *zip.File
}

// Open returns a reader of the entry's uncompressed bytes.
func (e Entry) Open() (io.ReadCloser, error) {
	if e.File == nil {
		return nil, errors.New("no file reference")
	}
	return e.File.Open()
}

// List lists an archive's entries in central-directory order.
func List(r io.ReaderAt, size int64) ([]Entry, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, err
	}

	var entries []Entry
	for _, f := range zr.File {
		if f.Method != zip.Store && f.Method != zip.Deflate {
			return nil, fmt.Errorf("%w: %d", ErrUnsupportedCompression, f.Method)
		}

		rawName := strings.ReplaceAll(f.Name, "\\", "/")
		isDir := strings.HasSuffix(rawName, "/")
		name := strings.TrimSuffix(rawName, "/")

		if !oci.IsSafeRelPath(name) {
			return nil, fmt.Errorf("%w: %s", ErrZipBadFilename, name)
		}
		if isDir && (f.UncompressedSize64 != 0 || f.UncompressedSize != 0) {
			return nil, ErrZipBadDirectorySize
		}

		entries = append(entries, Entry{
			Name:   name,
			IsDir:  isDir,
			Method: f.Method,
			Size:   int64(f.UncompressedSize64),
			File:   f,
		})
	}

	return entries, nil
}
