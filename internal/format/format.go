package format

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/estargz"
	"github.com/zigsaw-io/zigsaw/internal/oci"
)

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type TarWriterFunc func(w io.Writer) error

func WriteLayer(tmpPath, tarTmpPath string, f Format, writeTar TarWriterFunc, prefetch []string) (*Blob, error) {
	out, err := os.Create(tmpPath)
	if err != nil {
		return nil, err
	}
	defer out.Close()

	hasher := sha256.New()
	mw := io.MultiWriter(out, hasher)
	var written int64
	var annotations map[string]string

	switch f {
	case FormatTar:
		cw := &countingWriter{w: mw}
		if err := writeTar(cw); err != nil {
			return nil, err
		}
		written = cw.n

	case FormatGzip:
		cw := &countingWriter{w: mw}
		gw, err := GzipWriter(cw)
		if err != nil {
			return nil, err
		}
		if err := writeTar(gw); err != nil {
			gw.Close()
			return nil, err
		}
		if err := gw.Close(); err != nil {
			return nil, err
		}
		written = cw.n

	case FormatEstargz:
		tarFile, err := os.Create(tarTmpPath)
		if err != nil {
			return nil, err
		}
		if err := writeTar(tarFile); err != nil {
			tarFile.Close()
			_ = os.Remove(tarTmpPath)
			return nil, err
		}
		if err := tarFile.Close(); err != nil {
			_ = os.Remove(tarTmpPath)
			return nil, err
		}

		r, err := os.Open(tarTmpPath)
		if err != nil {
			_ = os.Remove(tarTmpPath)
			return nil, err
		}
		defer func() {
			r.Close()
			_ = os.Remove(tarTmpPath)
		}()

		cw := &countingWriter{w: mw}
		ann, err := estargz.Convert(r, cw, prefetch)
		if err != nil {
			return nil, err
		}
		annotations = ann
		written = cw.n

	default:
		return nil, fmt.Errorf("unknown layer format: %v", f)
	}

	if err := out.Sync(); err != nil {
		return nil, err
	}

	digest := fmt.Sprintf("sha256:%x", hasher.Sum(nil))
	return &Blob{
		Path:        tmpPath,
		Digest:      digest,
		Size:        written,
		MediaType:   f.MediaType(),
		Annotations: annotations,
	}, nil
}

type Format int

const (
	FormatUnknown Format = iota
	FormatTar
	FormatGzip
	FormatEstargz
)

func (f Format) String() string {
	switch f {
	case FormatTar:
		return "plain tar"
	case FormatGzip:
		return "gzip"
	case FormatEstargz:
		return "eStargz"
	default:
		return "unknown"
	}
}

func (f Format) Indexed() bool {
	return f == FormatEstargz
}

func (f Format) MediaType() string {
	switch f {
	case FormatTar:
		return oci.MediaTypeLayerTar
	case FormatGzip, FormatEstargz:
		return oci.MediaTypeLayerTarGzip
	default:
		return ""
	}
}

// FormatOfDescriptor determines the layer format described by desc.
func FormatOfDescriptor(desc oci.Descriptor) (Format, bool) {
	if desc.MediaType == oci.MediaTypeLayerTar {
		return FormatTar, true
	}
	if desc.MediaType != oci.MediaTypeLayerTarGzip {
		return FormatUnknown, false
	}
	if desc.Annotations != nil {
		if _, ok := desc.Annotations[estargz.TOCJSONDigestAnnotation]; ok {
			return FormatEstargz, true
		}
	}
	return FormatGzip, true
}

// Blob represents a layer written to a temporary file, ready to be moved into the store.
type Blob struct {
	Path        string
	Digest      string // sha256:...
	Size        int64
	MediaType   string
	Annotations map[string]string
}

func (b Blob) Descriptor(desc oci.Descriptor) oci.Descriptor {
	d := desc
	d.Annotations = b.Annotations
	return d
}

// CheckDeployable verifies that the indexed layer only contains safe relative paths and files/dirs.
func CheckDeployable(desc oci.Descriptor, r *estargz.Reader) error {
	short := oci.ShortDigest(desc.Digest)
	for _, f := range r.AllEntries() {
		if !estargz.IsLayerFile(f.Name) {
			continue
		}
		clean := strings.Trim(f.Name, "/")
		if !oci.IsSafeRelPath(clean) {
			return fmt.Errorf("layer %s has a file outside it: %s", short, f.Name)
		}
		if f.Type != "dir" && f.Type != "reg" {
			return fmt.Errorf("layer %s has %s, which isn't a file or directory", short, f.Name)
		}
	}
	return nil
}

// Deploy unpacks the layer described by desc from blobPath into destPath.
func Deploy(verbose bool, desc oci.Descriptor, blobPath, workDir, destPath string) error {
	f, ok := FormatOfDescriptor(desc)
	if !ok {
		return fmt.Errorf("layer type %s is not supported", desc.MediaType)
	}

	switch f {
	case FormatTar:
		return Extract(blobPath, destPath)
	case FormatGzip:
		tarPath := filepath.Join(workDir, "layer.tar")
		if err := Inflate(blobPath, tarPath); err != nil {
			return err
		}
		return Extract(tarPath, destPath)
	case FormatEstargz:
		return deployEstargz(desc, blobPath, destPath)
	default:
		return fmt.Errorf("unsupported format %v", f)
	}
}

func deployEstargz(desc oci.Descriptor, blobPath, destPath string) error {
	file, err := os.Open(blobPath)
	if err != nil {
		return err
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return err
	}

	sr := io.NewSectionReader(file, 0, fi.Size())
	r, err := estargz.Open(sr)
	if err != nil {
		short := oci.ShortDigest(desc.Digest)
		return fmt.Errorf("layer %s can't be read as eStargz: %w", short, err)
	}

	// Verify TOC digest matches descriptor annotation
	if desc.Annotations != nil {
		want := desc.Annotations[estargz.TOCJSONDigestAnnotation]
		if want != "" && want != r.TOCDigest() {
			short := oci.ShortDigest(desc.Digest)
			return fmt.Errorf("layer %s: its TOC isn't the one its descriptor names (%s vs %s)", short, want, r.TOCDigest())
		}
	}

	if err := CheckDeployable(desc, r); err != nil {
		return err
	}

	if err := r.Extract(destPath, MaxExtractWorkers); err != nil {
		short := oci.ShortDigest(desc.Digest)
		return fmt.Errorf("unpacking layer %s: %w", short, err)
	}

	return nil
}
