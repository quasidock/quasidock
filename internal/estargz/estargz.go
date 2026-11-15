package estargz

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	ce "github.com/containerd/stargz-snapshotter/estargz"
	"github.com/quasidock/quasidock/internal/oci"
)

const (
	TOCJSONDigestAnnotation         = ce.TOCJSONDigestAnnotation
	StoreUncompressedSizeAnnotation = ce.StoreUncompressedSizeAnnotation
	TOCTarName                      = ce.TOCTarName
	PrefetchLandmark                = ce.PrefetchLandmark
	NoPrefetchLandmark              = ce.NoPrefetchLandmark
	FooterSize                      = ce.FooterSize
	DefaultChunkSize                = 4 * 1024 * 1024 // 4 MiB
)

var (
	ErrReservedName = errors.New("estargz reserved name")
	ErrTOCMismatch  = errors.New("TOC digest does not match descriptor")
)

// IsReserved returns true if name is one of the reserved eStargz file names.
func IsReserved(name string) bool {
	clean := strings.Trim(name, "/")
	return clean == TOCTarName || clean == PrefetchLandmark || clean == NoPrefetchLandmark
}

// IsLayerFile returns true if name is an actual app layer file, not a reserved eStargz landmark or TOC.
func IsLayerFile(name string) bool {
	return len(name) > 0 && !IsReserved(name)
}

// Convert converts a plain tar stream into an eStargz layer.
// It writes the eStargz blob to out and returns descriptor annotations.
func Convert(tarReader io.Reader, out io.Writer, prefetch []string) (map[string]string, error) {
	// eStargz.Build requires an io.SectionReader (seekable).
	// Buffer or write to a temp file if tarReader is not an os.File.
	var sr *io.SectionReader
	var tmpFile *os.File

	if f, ok := tarReader.(*os.File); ok {
		fi, err := f.Stat()
		if err != nil {
			return nil, err
		}
		sr = io.NewSectionReader(f, 0, fi.Size())
	} else {
		var err error
		tmpFile, err = os.CreateTemp("", "quasidock-tar-convert-*")
		if err != nil {
			return nil, err
		}
		defer func() {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
		}()

		n, err := io.Copy(tmpFile, tarReader)
		if err != nil {
			return nil, err
		}
		sr = io.NewSectionReader(tmpFile, 0, n)
	}

	// Verify no reserved names are in the tar top-level
	if err := checkNoReservedNames(sr); err != nil {
		return nil, err
	}

	var opts []ce.Option
	opts = append(opts, ce.WithChunkSize(DefaultChunkSize))
	opts = append(opts, ce.WithCompressionLevel(6))

	if len(prefetch) > 0 {
		var missed []string
		opts = append(opts, ce.WithPrioritizedFiles(prefetch))
		opts = append(opts, ce.WithAllowPrioritizeNotFound(&missed))
	}

	blob, err := ce.Build(sr, opts...)
	if err != nil {
		return nil, err
	}
	defer blob.Close()

	if _, err := io.Copy(out, blob); err != nil {
		return nil, err
	}

	uncompressedSize, err := blob.UncompressedSize()
	if err != nil {
		return nil, err
	}

	annotations := map[string]string{
		TOCJSONDigestAnnotation:         blob.TOCDigest().String(),
		StoreUncompressedSizeAnnotation: strconv.FormatInt(uncompressedSize, 10),
	}

	return annotations, nil
}

func checkNoReservedNames(sr *io.SectionReader) error {
	tr := tar.NewReader(io.NewSectionReader(sr, 0, sr.Size()))
	for {
		hdr, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		clean := strings.Trim(hdr.Name, "/")
		if IsReserved(clean) {
			return fmt.Errorf("%w: %s", ErrReservedName, clean)
		}
	}
	return nil
}

// Reader wraps the stargz-snapshotter estargz.Reader.
type Reader struct {
	r          *ce.Reader
	sr         *io.SectionReader
	payloadEnd int64
}

// Open opens an eStargz reader from a SectionReader.
func Open(sr *io.SectionReader) (*Reader, error) {
	r, err := ce.Open(sr)
	if err != nil {
		return nil, err
	}
	tocOffset, _, err := ce.OpenFooter(sr)
	if err != nil {
		return nil, err
	}
	return &Reader{r: r, sr: sr, payloadEnd: tocOffset}, nil
}

// Raw returns the underlying *ce.Reader.
func (r *Reader) Raw() *ce.Reader {
	return r.r
}

// TOCDigest returns the digest of the TOC JSON.
func (r *Reader) TOCDigest() string {
	return r.r.TOCDigest().String()
}

// Lookup finds a TOCEntry by path.
func (r *Reader) Lookup(path string) (*ce.TOCEntry, bool) {
	return r.r.Lookup(path)
}

// OpenFile returns a reader for the uncompressed contents of the file at name.
func (r *Reader) OpenFile(name string) (*io.SectionReader, error) {
	return r.r.OpenFile(name)
}

// Prioritized returns the prioritized file entries (before PrefetchLandmark).
func (r *Reader) Prioritized() []*ce.TOCEntry {
	entries := r.AllEntries()
	for i, e := range entries {
		if e.Name == PrefetchLandmark {
			return entries[:i]
		}
		if e.Name == NoPrefetchLandmark {
			return nil
		}
	}
	return nil
}

// AllEntries returns all entries in the eStargz layer in traversal order.
func (r *Reader) AllEntries() []*ce.TOCEntry {
	var entries []*ce.TOCEntry
	root, ok := r.r.Lookup("")
	if !ok {
		return nil
	}
	var walk func(ent *ce.TOCEntry)
	walk = func(ent *ce.TOCEntry) {
		ent.ForeachChild(func(baseName string, child *ce.TOCEntry) bool {
			entries = append(entries, child)
			if child.Type == "dir" {
				walk(child)
			}
			return true
		})
	}
	walk(root)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Offset == entries[j].Offset {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Offset < entries[j].Offset
	})
	return entries
}

// Extract unpacks all layer files into destPath using multiple workers.
func (r *Reader) Extract(destPath string, numWorkers int) error {
	if err := os.MkdirAll(destPath, 0o755); err != nil {
		return err
	}

	all := r.AllEntries()
	var files []*ce.TOCEntry
	createdDirs := make(map[string]struct{})

	for _, e := range all {
		if !IsLayerFile(e.Name) {
			continue
		}
		cleanName := strings.Trim(e.Name, "/")
		if !oci.IsSafeRelPath(cleanName) {
			return fmt.Errorf("unsafe path in layer: %s", cleanName)
		}

		if e.Type == "dir" {
			targetDir := filepath.Join(destPath, filepath.FromSlash(cleanName))
			if err := os.MkdirAll(targetDir, 0o755); err != nil {
				return err
			}
			createdDirs[cleanName] = struct{}{}
		} else if e.Type == "reg" {
			parent := filepath.ToSlash(filepath.Dir(cleanName))
			if parent != "" && parent != "." {
				if _, ok := createdDirs[parent]; !ok {
					targetDir := filepath.Join(destPath, filepath.FromSlash(parent))
					if err := os.MkdirAll(targetDir, 0o755); err != nil {
						return err
					}
					createdDirs[parent] = struct{}{}
				}
			}
			files = append(files, e)
		}
	}

	if len(files) == 0 {
		return nil
	}

	if numWorkers <= 0 {
		numWorkers = runtime.NumCPU()
	}
	if numWorkers > 4 {
		numWorkers = 4
	}
	if numWorkers > len(files) {
		numWorkers = len(files)
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
			buf := make([]byte, 64*1024)
			for {
				idx := next.Add(1) - 1
				if idx >= int64(len(files)) {
					return
				}
				e := files[idx]

				cleanName := strings.Trim(e.Name, "/")
				targetPath := filepath.Join(destPath, filepath.FromSlash(cleanName))

				fr, err := r.r.OpenFile(e.Name)
				if err != nil {
					setErr(err)
					return
				}

				out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
				if err != nil {
					setErr(err)
					return
				}

				_, err = io.CopyBuffer(out, fr, buf)
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
