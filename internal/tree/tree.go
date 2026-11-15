package tree

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/zipfile"
)

var (
	ErrPathConflict       = errors.New("path conflict")
	ErrTarBadPath         = errors.New("tar bad path")
	ErrTarLinkNotToAFile  = errors.New("tar link not to a file")
	ErrUnsupportedContent = errors.New("unsupported content type")
)

type ContentKind int

const (
	ContentDir ContentKind = iota
	ContentFile
	ContentZip
	ContentTar
)

type Content struct {
	Kind ContentKind

	// For ContentFile:
	FilePath string
	FileSize int64

	// For ContentZip:
	ZipEntry zipfile.Entry

	// For ContentTar:
	TarFile   io.ReaderAt
	TarOffset int64
	TarSize   int64
}

type Node struct {
	// Path is '/'-separated, spelled as first added.
	Path    string
	Content Content
}

// Tree represents an in-memory virtual filesystem tree of layer contents.
type Tree struct {
	// nodes is keyed by lowercased path for Windows case-insensitivity.
	nodes    map[string]Node
	conflict string
}

func New() *Tree {
	return &Tree{nodes: make(map[string]Node)}
}

// Conflict returns the path that caused an ErrPathConflict.
func (t *Tree) Conflict() string {
	return t.conflict
}

// AddDir adds path and its parent directories.
func (t *Tree) AddDir(path string) error {
	norm := NormalizePath(path)
	if norm == "" {
		return nil
	}
	_, err := t.ensureDir(norm)
	return err
}

func (t *Tree) ensureDir(path string) (string, error) {
	var spelled string
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" {
			continue
		}
		var candidate string
		if spelled == "" {
			candidate = part
		} else {
			candidate = spelled + "/" + part
		}
		key := strings.ToLower(candidate)
		existing, ok := t.nodes[key]
		if !ok {
			t.nodes[key] = Node{
				Path:    candidate,
				Content: Content{Kind: ContentDir},
			}
			spelled = candidate
		} else if existing.Content.Kind != ContentDir {
			t.conflict = existing.Path
			return "", ErrPathConflict
		} else {
			spelled = existing.Path
		}
	}
	return spelled, nil
}

// AddFile adds a file to the tree at path. Overwriting an existing file replaces it;
// overwriting a directory or vice-versa returns ErrPathConflict.
func (t *Tree) AddFile(path string, content Content) error {
	norm := NormalizePath(path)
	dir := filepath.ToSlash(filepath.Dir(norm))
	var parent string
	var err error
	if dir != "" && dir != "." {
		parent, err = t.ensureDir(dir)
		if err != nil {
			return err
		}
	}
	base := filepath.Base(norm)
	var full string
	if parent == "" {
		full = base
	} else {
		full = parent + "/" + base
	}

	key := strings.ToLower(full)
	existing, ok := t.nodes[key]
	if !ok {
		t.nodes[key] = Node{Path: full, Content: content}
	} else if existing.Content.Kind == ContentDir {
		t.conflict = existing.Path
		return ErrPathConflict
	} else {
		t.nodes[key] = Node{Path: existing.Path, Content: content}
	}
	return nil
}

// AddTree adds another tree's paths under base ("" for top).
func (t *Tree) AddTree(other *Tree, base string) error {
	normBase := NormalizePath(base)
	for _, node := range other.nodes {
		var p string
		if normBase == "" {
			p = node.Path
		} else {
			p = normBase + "/" + node.Path
		}
		if node.Content.Kind == ContentDir {
			if err := t.AddDir(p); err != nil {
				return err
			}
		} else {
			if err := t.AddFile(p, node.Content); err != nil {
				return err
			}
		}
	}
	return nil
}

// IsFile returns true if path exists and is a file.
func (t *Tree) IsFile(path string) bool {
	p, ok := t.FilePath(path)
	return ok && p != ""
}

// FilePath returns the file's path as spelled in the tree, or false if not found/dir.
func (t *Tree) FilePath(path string) (string, bool) {
	key := strings.ToLower(NormalizePath(path))
	node, ok := t.nodes[key]
	if !ok || node.Content.Kind == ContentDir {
		return "", false
	}
	return node.Path, true
}

// Cleanup removes nodes matching any of the cleanup patterns.
func (t *Tree) Cleanup(patterns []string) {
	if len(patterns) == 0 {
		return
	}
	for key, node := range t.nodes {
		if CleanupMatches(patterns, node.Path) {
			delete(t.nodes, key)
		}
	}
}

// Sorted returns all nodes sorted alphabetically by Path, placing parent dirs before children.
func (t *Tree) Sorted() []Node {
	nodes := make([]Node, 0, len(t.nodes))
	for _, n := range t.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Path < nodes[j].Path
	})
	return nodes
}

// WriteTar writes the tree as a deterministic uncompressed tar layer.
func (t *Tree) WriteTar(w io.Writer) error {
	lw := format.NewLayerWriter(w)
	sorted := t.Sorted()
	for _, node := range sorted {
		if node.Content.Kind == ContentDir {
			if err := lw.AddDir(node.Path); err != nil {
				return err
			}
			continue
		}

		r, size, closer, err := openContent(node.Content)
		if err != nil {
			return fmt.Errorf("adding %s to layer: %w", node.Path, err)
		}
		addErr := lw.AddFile(node.Path, size, r)
		if closer != nil {
			closer.Close()
		}
		if addErr != nil {
			return fmt.Errorf("adding %s to layer: %w", node.Path, addErr)
		}
	}
	return lw.Finish()
}

// ContentHash returns the SHA256 digest of the tree as an uncompressed tar layer.
func (t *Tree) ContentHash() ([32]byte, string, error) {
	h := sha256.New()
	if err := t.WriteTar(h); err != nil {
		return [32]byte{}, "", err
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, hex.EncodeToString(sum[:]), nil
}

// FromDir scans the directory at dirPath and creates a Tree of its contents.
func FromDir(dirPath string) (*Tree, error) {
	t := New()
	if err := t.AddDirFiles(dirPath, ""); err != nil {
		return nil, err
	}
	return t, nil
}

// AddDirFiles walks dirPath and adds all files and directories below it under base.
func (t *Tree) AddDirFiles(dirPath, base string) error {
	normBase := NormalizePath(base)
	if normBase != "" {
		if err := t.AddDir(normBase); err != nil {
			return err
		}
	}

	return filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dirPath, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		var p string
		if normBase == "" {
			p = rel
		} else {
			p = normBase + "/" + rel
		}

		// Reject symlinks/reparse points
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("%s is a link or special file; app files can only be files and directories", path)
		}

		if info.IsDir() {
			return t.AddDir(p)
		}
		return t.AddFile(p, Content{
			Kind:     ContentFile,
			FilePath: path,
			FileSize: info.Size(),
		})
	})
}

// WriteFiles writes all nodes in the tree below destPath.
func (t *Tree) WriteFiles(destPath string) error {
	sorted := t.Sorted()
	for _, node := range sorted {
		target := filepath.Join(destPath, filepath.FromSlash(node.Path))
		if node.Content.Kind == ContentDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		r, _, closer, err := openContent(node.Content)
		if err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			if closer != nil {
				closer.Close()
			}
			return err
		}

		_, copyErr := io.Copy(out, r)
		closeErr := out.Close()
		if closer != nil {
			closer.Close()
		}
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

type Synced struct {
	Files   []string
	Written int
	Removed int
}

// SyncFiles syncs tree contents into destPath, preserving unchanged files
// and deleting obsolete files listed in previous.
func (t *Tree) SyncFiles(destPath string, previous []string) (*Synced, error) {
	sorted := t.Sorted()
	var files []string
	written := 0

	for _, node := range sorted {
		target := filepath.Join(destPath, filepath.FromSlash(node.Path))
		if node.Content.Kind == ContentDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
			continue
		}

		files = append(files, node.Path)
		same, err := sameContents(target, node.Content)
		if err == nil && same {
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}

		r, _, closer, err := openContent(node.Content)
		if err != nil {
			return nil, err
		}

		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			if closer != nil {
				closer.Close()
			}
			return nil, err
		}

		_, copyErr := io.Copy(out, r)
		closeErr := out.Close()
		if closer != nil {
			closer.Close()
		}
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		written++
	}

	removed := 0
	for _, p := range previous {
		key := strings.ToLower(NormalizePath(p))
		if node, ok := t.nodes[key]; ok && node.Content.Kind != ContentDir {
			continue
		}
		target := filepath.Join(destPath, filepath.FromSlash(p))
		if err := os.Remove(target); err == nil {
			removed++
		}
	}

	return &Synced{
		Files:   files,
		Written: written,
		Removed: removed,
	}, nil
}

func sameContents(diskPath string, content Content) (bool, error) {
	fi, err := os.Stat(diskPath)
	if err != nil {
		return false, err
	}
	r, size, closer, err := openContent(content)
	if err != nil {
		return false, err
	}
	if closer != nil {
		defer closer.Close()
	}
	if fi.Size() != size {
		return false, nil
	}

	f, err := os.Open(diskPath)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf1 := make([]byte, 4096)
	buf2 := make([]byte, 4096)
	for {
		n1, err1 := io.ReadFull(r, buf1)
		n2, err2 := io.ReadFull(f, buf2)
		if n1 != n2 || !bytes.Equal(buf1[:n1], buf2[:n2]) {
			return false, nil
		}
		if errors.Is(err1, io.EOF) || errors.Is(err1, io.ErrUnexpectedEOF) {
			break
		}
		if err1 != nil || err2 != nil {
			return false, nil
		}
	}
	return true, nil
}

func openContent(c Content) (io.Reader, int64, io.Closer, error) {
	switch c.Kind {
	case ContentDir:
		return nil, 0, nil, errors.New("cannot open directory content")
	case ContentFile:
		f, err := os.Open(c.FilePath)
		if err != nil {
			return nil, 0, nil, err
		}
		return f, c.FileSize, f, nil
	case ContentZip:
		rc, err := c.ZipEntry.Open()
		if err != nil {
			return nil, 0, nil, err
		}
		return rc, c.ZipEntry.Size, rc, nil
	case ContentTar:
		sr := io.NewSectionReader(c.TarFile, c.TarOffset, c.TarSize)
		return sr, c.TarSize, nil, nil
	default:
		return nil, 0, nil, ErrUnsupportedContent
	}
}

// NormalizePath normalizes a path: converts backslashes to slashes, strips empty and "." components.
func NormalizePath(p string) string {
	parts := strings.FieldsFunc(p, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	var clean []string
	for _, part := range parts {
		if part != "." && part != "" {
			clean = append(clean, part)
		}
	}
	return strings.Join(clean, "/")
}

// StripComponents drops the first n components of a '/'-separated path.
func StripComponents(path string, n int) (string, bool) {
	rest := path
	for i := 0; i < n; i++ {
		slash := strings.IndexByte(rest, '/')
		if slash == -1 {
			return "", false
		}
		rest = rest[slash+1:]
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

// CleanupMatches checks if path matches any cleanup pattern.
// Patterns starting with '/' match component prefixes ("/include" matches "include", "include/zlib.h").
// Other patterns match file names using globMatch ("*.pdb", "__pycache__").
func CleanupMatches(patterns []string, path string) bool {
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, "/") {
			want := strings.Trim(pattern, "/")
			if startsWithComponents(path, want) {
				return true
			}
		} else {
			parts := strings.Split(path, "/")
			for _, part := range parts {
				if GlobMatch(pattern, part) {
					return true
				}
			}
		}
	}
	return false
}

func startsWithComponents(path, prefix string) bool {
	wantParts := strings.FieldsFunc(prefix, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	haveParts := strings.Split(path, "/")
	if len(haveParts) < len(wantParts) {
		return false
	}
	for i, w := range wantParts {
		if !strings.EqualFold(w, haveParts[i]) {
			return false
		}
	}
	return true
}

// GlobMatch matches a filename against a pattern with '*' and '?', case-insensitively.
func GlobMatch(pattern, name string) bool {
	p := 0
	n := 0
	starP := -1
	starN := 0

	for n < len(name) {
		if p < len(pattern) && (pattern[p] == '?' || strings.EqualFold(string(pattern[p]), string(name[n]))) {
			p++
			n++
		} else if p < len(pattern) && pattern[p] == '*' {
			starP = p
			starN = n
			p++
		} else if starP != -1 {
			p = starP + 1
			starN++
			n = starN
		} else {
			return false
		}
	}

	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// AddZipEntries adds entries from a zip archive under dest with strip components.
func (t *Tree) AddZipEntries(entries []zipfile.Entry, dest string, strip int) error {
	base := NormalizePath(dest)
	for _, e := range entries {
		p, ok := entryPath(base, e.Name, strip)
		if !ok {
			continue
		}
		if e.IsDir {
			if err := t.AddDir(p); err != nil {
				return err
			}
		} else {
			if err := t.AddFile(p, Content{
				Kind:     ContentZip,
				ZipEntry: e,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// TarEntry describes an entry in a plain tar archive.
type TarEntry struct {
	Name   string
	IsDir  bool
	Offset int64
	Size   int64
}

type tarLink struct {
	name   string
	target string
}

type resolvedType int

const (
	resDir resolvedType = iota
	resFile
	resLink
)

type resolved struct {
	kind   resolvedType
	entry  TarEntry
	target tarLink
}

// ListTar lists entries in a plain tar archive, resolving symlinks to other files in the tar.
func ListTar(r io.ReaderAt, size int64) ([]TarEntry, error) {
	sr := io.NewSectionReader(r, 0, size)
	cr := &countingReader{r: sr}
	tr := tar.NewReader(cr)

	var entries []TarEntry
	var links []tarLink

	for {
		hdr, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}

		name := strings.Trim(hdr.Name, "/")
		if !oci.IsSafeRelPath(name) {
			return nil, ErrTarBadPath
		}

		if hdr.Typeflag == tar.TypeSymlink {
			links = append(links, tarLink{
				name:   name,
				target: hdr.Linkname,
			})
			continue
		}

		entries = append(entries, TarEntry{
			Name:   name,
			IsDir:  hdr.Typeflag == tar.TypeDir,
			Offset: cr.pos,
			Size:   hdr.Size,
		})
	}

	if len(links) == 0 {
		return entries, nil
	}

	byName := make(map[string]resolved)
	for _, e := range entries {
		norm := NormalizePath(e.Name)
		if e.IsDir {
			byName[norm] = resolved{kind: resDir}
		} else {
			byName[norm] = resolved{kind: resFile, entry: e}
		}
	}
	for _, l := range links {
		norm := NormalizePath(l.name)
		byName[norm] = resolved{kind: resLink, target: l}
	}

	for _, l := range links {
		target, ok := resolveLink(byName, l)
		if !ok {
			return nil, ErrTarLinkNotToAFile
		}
		entries = append(entries, TarEntry{
			Name:   l.name,
			IsDir:  false,
			Offset: target.Offset,
			Size:   target.Size,
		})
	}

	return entries, nil
}

func resolveLink(byName map[string]resolved, link tarLink) (TarEntry, bool) {
	curr := link
	for hop := 0; hop < 64; hop++ {
		path, ok := linkTargetPath(curr.name, curr.target)
		if !ok {
			return TarEntry{}, false
		}
		res, ok := byName[path]
		if !ok {
			return TarEntry{}, false
		}
		if res.kind == resFile {
			return res.entry, true
		}
		if res.kind == resDir {
			return TarEntry{}, false
		}
		curr = res.target
	}
	return TarEntry{}, false
}

func linkTargetPath(linkName, target string) (string, bool) {
	if target == "" || target[0] == '/' || target[0] == '\\' || strings.Contains(target, ":") {
		return "", false
	}
	parts := strings.Split(NormalizePath(linkName), "/")
	if len(parts) > 0 {
		parts = parts[:len(parts)-1]
	}

	targetParts := strings.FieldsFunc(target, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	for _, part := range targetParts {
		if part == "." {
			continue
		}
		if part == ".." {
			if len(parts) == 0 {
				return "", false
			}
			parts = parts[:len(parts)-1]
			continue
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "/"), true
}

// AddTarEntries adds entries from a tar archive under dest with strip components.
func (t *Tree) AddTarEntries(archive io.ReaderAt, entries []TarEntry, dest string, strip int) error {
	base := NormalizePath(dest)
	for _, e := range entries {
		p, ok := entryPath(base, e.Name, strip)
		if !ok {
			continue
		}
		if e.IsDir {
			if err := t.AddDir(p); err != nil {
				return err
			}
		} else {
			if err := t.AddFile(p, Content{
				Kind:      ContentTar,
				TarFile:   archive,
				TarOffset: e.Offset,
				TarSize:   e.Size,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func entryPath(base, name string, strip int) (string, bool) {
	rel, ok := StripComponents(NormalizePath(name), strip)
	if !ok {
		return "", false
	}
	rel = NormalizePath(rel)
	if rel == "" {
		return "", false
	}
	if base == "" {
		return rel, true
	}
	return base + "/" + rel, true
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
