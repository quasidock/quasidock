package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/quasidock/quasidock/internal/credentials"
	"github.com/quasidock/quasidock/internal/oci"
)

var (
	ErrInvalidReference = errors.New("invalid image reference")
	ErrRangeIgnored     = errors.New("registry sent the whole blob instead of range")
	ErrChunksRefused    = errors.New("registry refused chunked upload")
	ErrBroken           = errors.New("connection broken")
)

const (
	AcceptManifests = oci.MediaTypeManifest + ", " +
		"application/vnd.oci.image.index.v1+json, " +
		"application/vnd.docker.distribution.manifest.v2+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json"

	DefaultChunkSize = 64 << 20 // 64 MiB
	ChunkSizeVar     = "QUASIDOCK_UPLOAD_CHUNK_SIZE"
	MaxResumes       = 5
)

var downloadBroken atomic.Bool

type Reference struct {
	Host       string
	Repository string
	Tag        string
	Digest     string
}

func ParseReference(text string) (Reference, error) {
	rest := text
	var ref Reference

	if at := strings.IndexByte(rest, '@'); at != -1 {
		ref.Digest = rest[at+1:]
		if oci.DigestHex(ref.Digest) == "" {
			return Reference{}, ErrInvalidReference
		}
		rest = rest[:at]
	}

	slash := strings.IndexByte(rest, '/')
	if slash == -1 {
		return Reference{}, ErrInvalidReference
	}
	ref.Host = rest[:slash]
	if !looksLikeHost(ref.Host) {
		return Reference{}, ErrInvalidReference
	}
	rest = rest[slash+1:]

	if colon := strings.LastIndexByte(rest, ':'); colon != -1 {
		ref.Tag = rest[colon+1:]
		if !IsValidTag(ref.Tag) {
			return Reference{}, ErrInvalidReference
		}
		rest = rest[:colon]
	}

	ref.Repository = rest
	if !isValidRepository(ref.Repository) {
		return Reference{}, ErrInvalidReference
	}
	return ref, nil
}

func (ref Reference) ManifestRef() string {
	if ref.Digest != "" {
		return ref.Digest
	}
	if ref.Tag != "" {
		return ref.Tag
	}
	return "latest"
}

func (ref Reference) IsLocal() bool {
	name := ref.Host
	if c := strings.LastIndexByte(name, ':'); c != -1 {
		name = name[:c]
	}
	return name == "localhost" || name == "127.0.0.1" || name == "[::1]"
}

func (ref Reference) BaseURL() string {
	apiHost := ref.Host
	if ref.Host == "docker.io" {
		apiHost = "registry-1.docker.io"
	}
	scheme := "https"
	if ref.IsLocal() {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s", scheme, apiHost)
}

func (ref Reference) String() string {
	var sb strings.Builder
	sb.WriteString(ref.Host)
	sb.WriteByte('/')
	sb.WriteString(ref.Repository)
	if ref.Tag != "" {
		sb.WriteByte(':')
		sb.WriteString(ref.Tag)
	}
	if ref.Digest != "" {
		sb.WriteByte('@')
		sb.WriteString(ref.Digest)
	}
	return sb.String()
}

func looksLikeHost(s string) bool {
	if !credentials.IsValidHost(s) {
		return false
	}
	return strings.ContainsAny(s, ".:") || s == "localhost"
}

func isValidRepository(s string) bool {
	parts := strings.Split(s, "/")
	for _, part := range parts {
		if len(part) == 0 {
			return false
		}
		if !isAlphaNum(part[0]) || !isAlphaNum(part[len(part)-1]) {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			switch {
			case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			case c == '.' || c == '_' || c == '-':
			default:
				return false
			}
		}
	}
	return true
}

func IsValidTag(s string) bool {
	if len(s) == 0 || len(s) > 128 || s[0] == '.' || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

func isAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// ---------------------------------------------------------------------------
// Registry Client

type Manifest struct {
	Bytes  []byte
	Digest string // sha256:<hex>
}

type FileHash struct {
	Hex  string
	Size uint64
}

type Span struct {
	Start uint64
	End   *uint64 // exclusive
	Len   *uint64
}

func (s Span) Length() *uint64 {
	if s.End != nil {
		diff := *s.End - s.Start
		return &diff
	}
	return s.Len
}

type UploadKind int

const (
	UploadMounted UploadKind = iota
	UploadStarted
)

type StartedUpload struct {
	Location string
	ChunkMin *uint64
}

type Upload struct {
	Kind    UploadKind
	Started StartedUpload
}

type Registry struct {
	Ref           Reference
	Host          string
	Actions       string
	PullFrom      []string
	Authorization string
	Credential    *credentials.Credential
	LookedUp      bool
	Refreshed     bool
	NoChunks      bool
	Client        *http.Client
	Verbose       bool
	Broken        error
}

func New(ref Reference, actions string, verbose bool) (*Registry, error) {
	normHost, err := credentials.NormalizeHost(ref.Host)
	if err != nil {
		return nil, fmt.Errorf("%q isn't a registry host: %w", ref.Host, err)
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}

	return &Registry{
		Ref:     ref,
		Host:    normHost,
		Actions: actions,
		Verbose: verbose,
		Client: &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (r *Registry) SetPullFrom(repos []string) {
	r.PullFrom = repos
}

func (r *Registry) URL(pathFormat string, args ...any) string {
	path := fmt.Sprintf(pathFormat, args...)
	return r.Ref.BaseURL() + path
}

// FetchManifest fetches the manifest for the reference.
func (r *Registry) FetchManifest() (Manifest, error) {
	return r.FetchManifestRef(r.Ref.ManifestRef())
}

// FetchManifestRef fetches the manifest for reference (tag or digest).
func (r *Registry) FetchManifestRef(reference string) (Manifest, error) {
	req := request{
		Method: "GET",
		URL:    r.URL("/v2/%s/manifests/%s", r.Ref.Repository, reference),
		Accept: AcceptManifests,
	}
	res, err := r.send(req)
	if err != nil {
		return Manifest{}, err
	}
	if res.Status != http.StatusOK {
		return Manifest{}, r.failStatus(res, "fetching the manifest")
	}

	h := sha256.Sum256(res.Body)
	dig := "sha256:" + hex.EncodeToString(h[:])
	return Manifest{
		Bytes:  res.Body,
		Digest: dig,
	}, nil
}

// FetchManifestIfThere fetches the manifest, returning nil if 404 Not Found.
func (r *Registry) FetchManifestIfThere(reference string) (*Manifest, error) {
	req := request{
		Method: "GET",
		URL:    r.URL("/v2/%s/manifests/%s", r.Ref.Repository, reference),
		Accept: AcceptManifests,
	}
	res, err := r.send(req)
	if err != nil {
		return nil, err
	}
	if res.Status == http.StatusNotFound {
		return nil, nil
	}
	if res.Status != http.StatusOK {
		return nil, r.failStatus(res, "fetching the manifest")
	}

	h := sha256.Sum256(res.Body)
	dig := "sha256:" + hex.EncodeToString(h[:])
	return &Manifest{
		Bytes:  res.Body,
		Digest: dig,
	}, nil
}

func (r *Registry) PushManifest(tag string, data []byte) error {
	req := request{
		Method:      "PUT",
		URL:         r.URL("/v2/%s/manifests/%s", r.Ref.Repository, tag),
		Body:        data,
		ContentType: oci.MediaTypeManifest,
	}
	res, err := r.send(req)
	if err != nil {
		return err
	}
	if res.Status != http.StatusCreated {
		return r.failStatus(res, "uploading the manifest")
	}
	return nil
}

func (r *Registry) HasBlob(digest string) (bool, error) {
	req := request{
		Method: "HEAD",
		URL:    r.URL("/v2/%s/blobs/%s", r.Ref.Repository, digest),
	}
	res, err := r.send(req)
	if err != nil {
		return false, err
	}
	switch res.Status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, r.failStatus(res, "checking for a blob")
	}
}

func (r *Registry) DownloadBlob(desc oci.Descriptor, dest string) (FileHash, error) {
	uSize := uint64(desc.Size)
	h, err := r.downloadTo(desc.Digest, &uSize, dest)
	if err != nil {
		return FileHash{}, err
	}
	if h == nil {
		return FileHash{}, r.failStatus(response{Status: http.StatusNotFound}, "downloading a blob")
	}
	if h.Size != uSize {
		_ = os.Remove(dest)
		return FileHash{}, fmt.Errorf("blob %s from %s doesn't match its digest", desc.Digest, r.Ref)
	}
	return *h, nil
}

func (r *Registry) DownloadBlobByDigest(digest string, dest string) (*FileHash, error) {
	return r.downloadTo(digest, nil, dest)
}

func (r *Registry) downloadTo(digest string, size *uint64, dest string) (*FileHash, error) {
	again := false
	for {
		h, err := r.downloadOnce(digest, size, dest)
		if err != nil {
			if errors.Is(err, ErrRangeIgnored) {
				if again {
					return nil, fmt.Errorf("blob %s from %s: the download broke off, and the registry can't go on from where it got to", digest, r.Ref)
				}
				again = true
				if r.Verbose {
					fmt.Fprintf(os.Stderr, "  %s: the registry sent the whole blob again; starting over\n", oci.ShortDigest(digest))
				}
				continue
			}
			return nil, err
		}
		return h, nil
	}
}

func (r *Registry) downloadOnce(digest string, size *uint64, dest string) (*FileHash, error) {
	_ = os.MkdirAll(filepath.Dir(dest), 0755)
	f, err := os.Create(dest)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(dest)
		}
	}()

	hasher := sha256.New()
	multi := io.MultiWriter(f, hasher)

	span := Span{Start: 0}
	if size != nil {
		span.Len = size
	}

	found, err := r.GetBlob(digest, span, multi)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	hashHex := hex.EncodeToString(hasher.Sum(nil))
	expectedHex := oci.DigestHex(digest)
	if expectedHex != "" && hashHex != expectedHex {
		return nil, fmt.Errorf("blob %s from %s doesn't match its digest", digest, r.Ref)
	}

	keep = true
	return &FileHash{
		Hex:  hashHex,
		Size: uint64(stat.Size()),
	}, nil
}

func (r *Registry) GetBlob(digest string, span Span, sink io.Writer) (bool, error) {
	blobURL := r.URL("/v2/%s/blobs/%s", r.Ref.Repository, digest)
	at := span.Start
	resumes := 0
	breakAfter := r.testDownloadBreak(span)

	for {
		ranged := at > 0 || span.End != nil
		var rangeHeader string
		if ranged {
			if span.End != nil {
				rangeHeader = fmt.Sprintf("bytes=%d-%d", at, *span.End-1)
			} else {
				rangeHeader = fmt.Sprintf("bytes=%d-", at)
			}
		}

		cw := &countWriter{w: sink}
		req := request{
			Method:     "GET",
			URL:        blobURL,
			Range:      rangeHeader,
			Sink:       cw,
			Resumable:  true,
			BreakAfter: breakAfter,
		}
		breakAfter = nil

		res, err := r.send(req)
		if err != nil {
			if errors.Is(err, ErrBroken) {
				resumes++
				if cw.n == 0 || resumes > MaxResumes {
					return false, fmt.Errorf("GET %s: %w", blobURL, r.Broken)
				}
				at += cw.n
				if r.Verbose {
					fmt.Fprintf(os.Stderr, "  resuming %s at %d bytes, after %v\n", oci.ShortDigest(digest), at, r.Broken)
				}
				continue
			}
			return false, err
		}

		switch res.Status {
		case http.StatusNotFound:
			return false, nil
		case http.StatusOK:
			if ranged {
				return false, ErrRangeIgnored
			}
		case http.StatusPartialContent:
			if !ranged || contentRangeStart(res.ContentRange) == nil || *contentRangeStart(res.ContentRange) != at {
				cr := res.ContentRange
				if cr == "" {
					cr = "no Content-Range"
				}
				return false, fmt.Errorf("blob %s from %s: the registry sent other bytes than asked for (%s)", digest, r.Ref, cr)
			}
		default:
			return false, r.failStatus(res, "downloading a blob")
		}

		at += cw.n
		if span.End != nil && at != *span.End {
			return false, fmt.Errorf("blob %s from %s: %d bytes short", digest, r.Ref, *span.End-at)
		}
		return true, nil
	}
}

type countWriter struct {
	w io.Writer
	n uint64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += uint64(n)
	return n, err
}

func (r *Registry) testDownloadBreak(span Span) *uint64 {
	val := os.Getenv("QUASIDOCK_TEST_DOWNLOAD_BREAK")
	if val == "" {
		return nil
	}
	n, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return nil
	}
	l := span.Length()
	if l == nil || *l <= n {
		return nil
	}
	if downloadBroken.Swap(true) {
		return nil
	}
	return &n
}

func contentRangeStart(text string) *uint64 {
	if !strings.HasPrefix(strings.ToLower(text), "bytes ") {
		return nil
	}
	rest := strings.TrimLeft(text[len("bytes "):], " ")
	dash := strings.IndexByte(rest, '-')
	if dash == -1 {
		return nil
	}
	v, err := strconv.ParseUint(rest[:dash], 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

// ---------------------------------------------------------------------------
// Uploads

func (r *Registry) StartUpload(digest string, mountFrom string) (Upload, error) {
	if mountFrom != "" && containsString(r.PullFrom, mountFrom) {
		uploadURL := r.URL("%s", uploadsPath(r.Ref.Repository, digest, mountFrom))
		res, err := r.send(request{Method: "POST", URL: uploadURL})
		if err == nil {
			switch res.Status {
			case http.StatusCreated:
				return Upload{Kind: UploadMounted}, nil
			case http.StatusAccepted:
				loc, err := r.uploadLocation(res)
				if err != nil {
					return Upload{}, err
				}
				return Upload{
					Kind: UploadStarted,
					Started: StartedUpload{
						Location: loc,
						ChunkMin: res.ChunkMin,
					},
				}, nil
			default:
				if r.Verbose {
					fmt.Fprintf(os.Stderr, "  mounting %s from %s: HTTP %d\n", oci.ShortDigest(digest), mountFrom, res.Status)
				}
			}
		}
	}

	uploadURL := r.URL("%s", uploadsPath(r.Ref.Repository, "", ""))
	res, err := r.send(request{Method: "POST", URL: uploadURL})
	if err != nil {
		return Upload{}, err
	}
	if res.Status != http.StatusAccepted {
		return Upload{}, r.failStatus(res, "starting an upload")
	}

	loc, err := r.uploadLocation(res)
	if err != nil {
		return Upload{}, err
	}
	return Upload{
		Kind: UploadStarted,
		Started: StartedUpload{
			Location: loc,
			ChunkMin: res.ChunkMin,
		},
	}, nil
}

func (r *Registry) uploadLocation(res response) (string, error) {
	if res.Location == "" {
		return "", fmt.Errorf("%s started an upload without saying where to send it", r.Ref)
	}
	return res.Location, nil
}

func uploadsPath(repo, digest, from string) string {
	if digest != "" && from != "" {
		return fmt.Sprintf("/v2/%s/blobs/uploads/?mount=%s&from=%s", repo, url.QueryEscape(digest), url.QueryEscape(from))
	}
	return fmt.Sprintf("/v2/%s/blobs/uploads/", repo)
}

func (r *Registry) FinishUpload(started StartedUpload, desc oci.Descriptor, filePath string) error {
	chunk, err := r.chunkSize(started.ChunkMin)
	if err != nil {
		return err
	}
	descSize := uint64(desc.Size)
	if chunk == 0 || descSize <= chunk || r.NoChunks {
		return r.putWhole(started.Location, desc, filePath)
	}

	chunks, err := r.putChunks(started.Location, desc, filePath, chunk)
	if err != nil {
		if errors.Is(err, ErrChunksRefused) {
			r.NoChunks = true
			if r.Verbose {
				fmt.Fprintf(os.Stderr, "  %s doesn't take blobs in chunks; uploading %s in one request\n", r.Ref.Host, oci.ShortDigest(desc.Digest))
			}
			up, err := r.StartUpload(desc.Digest, "")
			if err != nil {
				return err
			}
			if up.Kind == UploadMounted {
				return nil
			}
			return r.putWhole(up.Started.Location, desc, filePath)
		}
		return err
	}

	if r.Verbose {
		fmt.Fprintf(os.Stderr, "  sent %s in %d chunks\n", oci.ShortDigest(desc.Digest), chunks)
	}
	return nil
}

func (r *Registry) chunkSize(chunkMin *uint64) (uint64, error) {
	size := uint64(DefaultChunkSize)
	if val := os.Getenv(ChunkSizeVar); val != "" {
		n, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be a number of bytes; it's %q", ChunkSizeVar, val)
		}
		size = n
	}
	if size == 0 {
		return 0, nil
	}
	if chunkMin != nil && *chunkMin > size {
		return *chunkMin, nil
	}
	return size, nil
}

func (r *Registry) putWhole(location string, desc oci.Descriptor, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	u, err := r.withDigest(location, desc.Digest)
	if err != nil {
		return err
	}

	descSize := uint64(desc.Size)
	req := request{
		Method:        "PUT",
		URL:           u,
		File:          f,
		FileSize:      descSize,
		ContentType:   "application/octet-stream",
		ContentLength: &descSize,
	}
	res, err := r.send(req)
	if err != nil {
		return err
	}
	if res.Status != http.StatusCreated {
		return r.failStatus(res, "uploading a blob")
	}
	return nil
}

func (r *Registry) putChunks(location string, desc oci.Descriptor, filePath string, chunk uint64) (uint32, error) {
	breakAt := testChunk("QUASIDOCK_TEST_UPLOAD_BREAK")
	skipAt := testChunk("QUASIDOCK_TEST_UPLOAD_SKIP")
	loseAt := testChunk("QUASIDOCK_TEST_UPLOAD_LOSE")

	at := location
	var offset uint64
	var chunks uint32
	var resumes uint32
	broke := false
	descSize := uint64(desc.Size)

	f, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	for {
		for offset < descSize {
			length := chunk
			if descSize-offset < length {
				length = descSize - offset
			}
			chunks++

			claimed := offset
			if skipAt != nil && *skipAt == chunks {
				claimed = offset + 1
			}

			targetURL := at
			if loseAt != nil && *loseAt == chunks {
				targetURL = lostURL(at)
			}
			absURL, err := r.absoluteURL(targetURL)
			if err != nil {
				return 0, err
			}

			breakHalfway := (breakAt != nil && *breakAt == chunks)

			req := request{
				Method:        "PATCH",
				URL:           absURL,
				File:          f,
				FileOffset:    offset,
				FileSize:      length,
				BreakHalfway:  breakHalfway,
				ContentType:   "application/octet-stream",
				ContentRange:  fmt.Sprintf("%d-%d", claimed, claimed+length-1),
				ContentLength: &length,
				Resumable:     true,
			}

			res, err := r.send(req)
			if err != nil {
				if errors.Is(err, ErrBroken) {
					broke = true
					var resumeErr error
					at, resumeErr = r.resumeUpload(at, desc, &offset, &resumes, "broke", r.Broken)
					if resumeErr != nil {
						return 0, resumeErr
					}
					continue
				}
				return 0, err
			}

			if r.Verbose {
				rangeStr := res.Range
				if rangeStr == "" {
					rangeStr = "none"
				}
				fmt.Fprintf(os.Stderr, "  chunk %d of %s (%d bytes from %d): HTTP %d, Range %s\n",
					chunks, oci.ShortDigest(desc.Digest), length, offset, res.Status, rangeStr)
			}

			switch res.Status {
			case http.StatusAccepted:
				if res.Location != "" {
					at = res.Location
				}
				if res.Range != "" {
					if next := nextOffset(res.Range); next != nil {
						offset = *next
					} else {
						offset += length
					}
				} else {
					offset += length
				}
			case http.StatusUnauthorized, http.StatusForbidden:
				return 0, r.failStatus(res, "uploading a blob")
			case http.StatusRequestEntityTooLarge:
				return 0, fmt.Errorf("%s refused a chunk of %d bytes as too large; set %s to fewer bytes", r.Ref.Host, length, ChunkSizeVar)
			case http.StatusNotFound:
				at, err = r.lostUpload(desc, &offset, &resumes, broke)
				if err != nil {
					return 0, err
				}
			default:
				if offset == 0 {
					return 0, ErrChunksRefused
				}
				if res.Status != http.StatusRequestedRangeNotSatisfiable && res.Status < 500 {
					return 0, r.failStatus(res, "uploading a blob")
				}
				at, err = r.resumeUpload(at, desc, &offset, &resumes, fmt.Sprintf("HTTP %d", res.Status), nil)
				if err != nil {
					return 0, err
				}
			}
		}

		finURL, err := r.withDigest(at, desc.Digest)
		if err != nil {
			return 0, err
		}
		res, err := r.send(request{Method: "PUT", URL: finURL})
		if err != nil {
			return 0, err
		}
		if res.Status == http.StatusCreated {
			return chunks, nil
		}
		if res.Status != http.StatusNotFound {
			return 0, r.failStatus(res, "uploading a blob")
		}
		at, err = r.lostUpload(desc, &offset, &resumes, broke)
		if err != nil {
			return 0, err
		}
	}
}

func (r *Registry) lostUpload(desc oci.Descriptor, offset *uint64, resumes *uint32, broke bool) (string, error) {
	short := oci.ShortDigest(desc.Digest)
	if !broke {
		fmt.Fprintf(os.Stderr, "%s lost the upload of %s at %d of %d bytes (HTTP 404); uploading it in one request\n",
			r.Ref.Host, short, *offset, desc.Size)
		return "", ErrChunksRefused
	}
	*resumes++
	if *resumes > 3 {
		return "", fmt.Errorf("uploading %s to %s failed %d times (the registry lost the upload); giving up", desc.Digest, r.Ref, *resumes-1)
	}
	fmt.Fprintf(os.Stderr, "%s lost the upload of %s, which broke off, at %d of %d bytes (HTTP 404); starting it again\n",
		r.Ref.Host, short, *offset, desc.Size)
	*offset = 0
	up, err := r.StartUpload(desc.Digest, "")
	if err != nil {
		return "", err
	}
	return up.Started.Location, nil
}

func (r *Registry) resumeUpload(at string, desc oci.Descriptor, offset *uint64, resumes *uint32, why string, failure error) (string, error) {
	short := oci.ShortDigest(desc.Digest)
	*resumes++
	if *resumes > 3 {
		return "", fmt.Errorf("uploading %s to %s failed %d times (%s); giving up", desc.Digest, r.Ref, *resumes-1, why)
	}

	var waited time.Duration
	var res response
	for {
		asked := at
		if os.Getenv("QUASIDOCK_TEST_UPLOAD_LOST") != "" {
			asked = lostURL(at)
		}
		absURL, err := r.absoluteURL(asked)
		if err != nil {
			return "", err
		}
		res, err = r.send(request{Method: "GET", URL: absURL})
		if err != nil {
			return "", err
		}
		if res.Status < 500 || waited >= 2*time.Second {
			break
		}
		if r.Verbose {
			fmt.Fprintf(os.Stderr, "  asking %s how much of %s it has: HTTP %d\n", r.Ref.Host, short, res.Status)
		}
		time.Sleep(500 * time.Millisecond)
		waited += 500 * time.Millisecond
	}

	if res.Status != http.StatusNoContent {
		if failure == nil {
			fmt.Fprintf(os.Stderr, "%s failed a chunk of %s (%s) and can't say how much of it it has (HTTP %d); uploading it in one request\n",
				r.Ref.Host, short, why, res.Status)
			return "", ErrChunksRefused
		}
		fmt.Fprintf(os.Stderr, "upload of %s broke off (%s), and %s can't say how much of it it has (HTTP %d); starting it again\n",
			short, why, r.Ref.Host, res.Status)
		*offset = 0
		up, err := r.StartUpload(desc.Digest, "")
		if err != nil {
			return "", err
		}
		return up.Started.Location, nil
	}

	if res.Range != "" {
		if next := nextOffset(res.Range); next != nil {
			*offset = *next
		} else {
			*offset = 0
		}
	} else {
		*offset = 0
	}

	what := "failed"
	if failure != nil {
		what = "broke off"
	}
	fmt.Fprintf(os.Stderr, "upload of %s %s (%s) at %d of %d bytes; resuming\n", short, what, why, *offset, desc.Size)
	if res.Location != "" {
		return res.Location, nil
	}
	return at, nil
}

func lostURL(at string) string {
	end := strings.IndexByte(at, '?')
	if end == -1 {
		return at + "-lost"
	}
	return at[:end] + "-lost" + at[end:]
}

func testChunk(name string) *uint32 {
	val := os.Getenv(name)
	if val == "" {
		return nil
	}
	n, err := strconv.ParseUint(val, 10, 32)
	if err != nil {
		return nil
	}
	u := uint32(n)
	return &u
}

func nextOffset(rangeStr string) *uint64 {
	text := rangeStr
	if strings.HasPrefix(strings.ToLower(text), "bytes=") {
		text = text[len("bytes="):]
	}
	dash := strings.IndexByte(text, '-')
	if dash == -1 {
		return nil
	}
	last, err := strconv.ParseUint(strings.TrimSpace(text[dash+1:]), 10, 64)
	if err != nil {
		return nil
	}
	if last == 0 {
		var z uint64 = 0
		return &z
	}
	next := last + 1
	return &next
}

func (r *Registry) absoluteURL(location string) (string, error) {
	if strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://") {
		return location, nil
	}
	return r.URL("%s", location), nil
}

func (r *Registry) withDigest(location, digest string) (string, error) {
	abs, err := r.absoluteURL(location)
	if err != nil {
		return "", err
	}
	sep := "?"
	if strings.ContainsRune(location, '?') {
		sep = "&"
	}
	return fmt.Sprintf("%s%sdigest=%s", abs, sep, url.QueryEscape(digest)), nil
}

// ---------------------------------------------------------------------------
// HTTP Layer & Auth

type request struct {
	Method        string
	URL           string
	Body          []byte
	File          *os.File
	FileOffset    uint64
	FileSize      uint64
	BreakHalfway  bool
	ContentType   string
	ContentRange  string
	ContentLength *uint64
	Accept        string
	Range         string
	Resumable     bool
	Sink          io.Writer
	BreakAfter    *uint64
}

type response struct {
	Link            string
	Status          int
	Body            []byte
	Location        string
	WWWAuthenticate string
	Range           string
	ContentRange    string
	ChunkMin        *uint64
}

func (r *Registry) send(req request) (response, error) {
	res, err := r.follow(req, r.Authorization)
	if err != nil {
		return response{}, err
	}
	if res.Status != http.StatusUnauthorized {
		return res, nil
	}
	challenge := res.WWWAuthenticate
	if challenge == "" {
		return res, nil
	}
	if r.Authorization != "" {
		if r.Refreshed || !strings.HasPrefix(strings.ToLower(r.Authorization), "bearer ") {
			return res, nil
		}
		r.Refreshed = true
	}
	if err := r.authenticate(challenge); err != nil {
		return response{}, err
	}
	return r.follow(req, r.Authorization)
}

func (r *Registry) follow(req request, auth string) (response, error) {
	cur := req
	curAuth := auth
	redirects := 0

	for {
		res, err := r.exchange(cur, curAuth)
		if err != nil {
			return response{}, err
		}
		if (res.Status < 300 || res.Status >= 400) || cur.Body != nil || cur.File != nil {
			return res, nil
		}
		redirects++
		if redirects == 5 {
			return response{}, fmt.Errorf("%s: too many redirects", req.URL)
		}
		if res.Location == "" {
			return response{}, fmt.Errorf("%s: redirect without a location", cur.URL)
		}
		nextURL, err := resolveURL(cur.URL, res.Location)
		if err != nil {
			return response{}, err
		}
		cur.URL = nextURL
		if !strings.EqualFold(authority(cur.URL), authority(req.URL)) {
			curAuth = ""
			if r.Verbose {
				fmt.Fprintf(os.Stderr, "  redirected to %s, without the registry token\n", authority(cur.URL))
			}
		}
	}
}

func resolveURL(base, location string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	locURL, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	return baseURL.ResolveReference(locURL).String(), nil
}

func authority(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	return parsed.Host
}

func (r *Registry) exchange(req request, auth string) (response, error) {
	res, err := r.exchangeUnchecked(req, auth)
	if err != nil {
		if req.Resumable {
			r.Broken = err
			return response{}, ErrBroken
		}
		return response{}, fmt.Errorf("%s %s: %w", req.Method, req.URL, err)
	}
	return res, nil
}

func (r *Registry) exchangeUnchecked(req request, auth string) (response, error) {
	var bodyReader io.Reader
	if req.Body != nil {
		bodyReader = bytes.NewReader(req.Body)
	} else if req.File != nil {
		section := io.NewSectionReader(req.File, int64(req.FileOffset), int64(req.FileSize))
		if req.BreakHalfway {
			bodyReader = &breakHalfwayReader{r: section, limit: int64(req.FileSize / 2)}
		} else {
			bodyReader = section
		}
	}

	hreq, err := http.NewRequest(req.Method, req.URL, bodyReader)
	if err != nil {
		return response{}, err
	}

	if auth != "" {
		hreq.Header.Set("Authorization", auth)
	}
	if req.ContentType != "" {
		hreq.Header.Set("Content-Type", req.ContentType)
	}
	if req.Accept != "" {
		hreq.Header.Set("Accept", req.Accept)
	}
	if req.ContentRange != "" {
		hreq.Header.Set("Content-Range", req.ContentRange)
	}
	if req.Range != "" {
		hreq.Header.Set("Range", req.Range)
	}
	if req.ContentLength != nil {
		hreq.ContentLength = int64(*req.ContentLength)
	}
	hreq.Header.Set("Accept-Encoding", "identity")

	hres, err := r.Client.Do(hreq)
	if err != nil {
		return response{}, err
	}
	defer hres.Body.Close()

	res := response{
		Status:          hres.StatusCode,
		Location:        hres.Header.Get("Location"),
		WWWAuthenticate: hres.Header.Get("Www-Authenticate"),
		Link:            hres.Header.Get("Link"),
		Range:           hres.Header.Get("Range"),
		ContentRange:    hres.Header.Get("Content-Range"),
	}
	if minLen := hres.Header.Get("OCI-Chunk-Min-Length"); minLen != "" {
		if n, err := strconv.ParseUint(minLen, 10, 64); err == nil {
			res.ChunkMin = &n
		}
	}

	if req.Sink != nil && req.Range != "" && res.Status == http.StatusOK {
		// All of it, not the part asked for: discarded
		return res, nil
	} else if req.Sink != nil && res.Status >= 200 && res.Status < 300 {
		var src io.Reader = hres.Body
		if req.BreakAfter != nil {
			src = &breakHalfwayReader{r: hres.Body, limit: int64(*req.BreakAfter)}
		}
		_, err := io.Copy(req.Sink, src)
		if err != nil {
			return response{}, err
		}
	} else {
		data, err := io.ReadAll(io.LimitReader(hres.Body, 16<<20))
		if err != nil {
			return response{}, err
		}
		res.Body = data
	}

	return res, nil
}

type breakHalfwayReader struct {
	r     io.Reader
	limit int64
	read  int64
}

func (b *breakHalfwayReader) Read(p []byte) (int, error) {
	if b.read >= b.limit {
		return 0, io.ErrUnexpectedEOF
	}
	toRead := int64(len(p))
	if b.limit-b.read < toRead {
		toRead = b.limit - b.read
	}
	n, err := b.r.Read(p[:toRead])
	b.read += int64(n)
	if err == nil && b.read >= b.limit {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// ---------------------------------------------------------------------------
// Authentication

type ChallengeScheme int

const (
	SchemeBasic ChallengeScheme = iota
	SchemeBearer
)

type Challenge struct {
	Scheme  ChallengeScheme
	Realm   string
	Service string
}

func ParseChallenge(text string) *Challenge {
	space := strings.IndexByte(text, ' ')
	if space == -1 {
		space = len(text)
	}
	schemeStr := strings.ToLower(text[:space])
	var scheme ChallengeScheme
	switch schemeStr {
	case "bearer":
		scheme = SchemeBearer
	case "basic":
		scheme = SchemeBasic
	default:
		return nil
	}

	c := &Challenge{Scheme: scheme}
	i := space
	for i < len(text) {
		for i < len(text) && (text[i] == ' ' || text[i] == ',') {
			i++
		}
		eq := strings.IndexByte(text[i:], '=')
		if eq == -1 {
			break
		}
		eq += i
		key := strings.TrimSpace(text[i:eq])
		var val string
		if eq+1 < len(text) && text[eq+1] == '"' {
			closeQuote := strings.IndexByte(text[eq+2:], '"')
			if closeQuote == -1 {
				return nil
			}
			closeQuote += eq + 2
			val = text[eq+2 : closeQuote]
			i = closeQuote + 1
		} else {
			end := strings.IndexByte(text[eq+1:], ',')
			if end == -1 {
				end = len(text)
			} else {
				end += eq + 1
			}
			val = strings.TrimSpace(text[eq+1 : end])
			i = end
		}
		if strings.EqualFold(key, "realm") {
			c.Realm = val
		} else if strings.EqualFold(key, "service") {
			c.Service = val
		}
	}
	return c
}

func (r *Registry) authenticate(challengeText string) error {
	ch := ParseChallenge(challengeText)
	if ch == nil {
		return fmt.Errorf("%s asked for authentication quasidock doesn't support: %s", r.Ref.Host, challengeText)
	}

	cred, err := r.findCredential()
	if err != nil {
		return err
	}

	var basic string
	if cred != nil {
		basic = basicAuthorization(*cred)
	}

	switch ch.Scheme {
	case SchemeBasic:
		if basic == "" {
			return r.failNeedsLogin()
		}
		r.Authorization = basic
	case SchemeBearer:
		var scopes []string
		scopes = append(scopes, fmt.Sprintf("repository:%s:%s", r.Ref.Repository, r.Actions))
		for _, from := range r.PullFrom {
			scopes = append(scopes, fmt.Sprintf("repository:%s:pull", from))
		}

		tok, err := r.fetchToken(*ch, basic, scopes)
		if err == nil && tok != "" {
			r.Authorization = tok
			return nil
		}

		// Try without pull_from
		if len(r.PullFrom) > 0 {
			if r.Verbose {
				fmt.Fprintf(os.Stderr, "  %s refused a token that also reads %s; asking without\n", r.Ref.Host, strings.Join(r.PullFrom, ", "))
			}
			r.PullFrom = nil
			tok, err = r.fetchToken(*ch, basic, scopes[:1])
			if err == nil && tok != "" {
				r.Authorization = tok
				return nil
			}
		}

		// Try anonymous if Docker cred refused
		if cred != nil && cred.IsDocker() && r.Actions == "pull" {
			fmt.Fprintf(os.Stderr, "  %s refused the credentials %s; pulling without them\n", r.Ref.Host, cred.Origin())
			tok, err = r.fetchToken(*ch, "", scopes[:1])
			if err == nil && tok != "" {
				r.Authorization = tok
				return nil
			}
		}

		if cred != nil {
			return r.failRefused(*cred)
		}
		return r.failNeedsLogin()
	}
	return nil
}

func basicAuthorization(c credentials.Credential) string {
	plain := c.User + ":" + c.Secret
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(plain))
}

func (r *Registry) fetchToken(ch Challenge, basic string, scopes []string) (string, error) {
	if ch.Realm == "" {
		return "", fmt.Errorf("%s asked for a token without saying where to get one", r.Ref.Host)
	}
	if basic != "" && !MayReceiveCredentials(ch.Realm, r.Ref.IsLocal()) {
		return "", fmt.Errorf("%s asked for its credentials to be sent to %s over plain HTTP; quasidock won't send them", r.Ref.Host, ch.Realm)
	}

	tokURL := TokenURL(ch.Realm, ch.Service, scopes)
	req := request{Method: "GET", URL: tokURL}
	res, err := r.exchange(req, basic)
	if err != nil {
		return "", err
	}
	if res.Status == http.StatusUnauthorized || res.Status == http.StatusForbidden {
		return "", nil
	}
	if res.Status != http.StatusOK {
		return "", fmt.Errorf("getting a token from %s: HTTP %d", ch.Realm, res.Status)
	}

	var parsed struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return "", fmt.Errorf("%s returned a token response quasidock can't read: %w", ch.Realm, err)
	}
	t := parsed.Token
	if t == "" {
		t = parsed.AccessToken
	}
	if t == "" {
		return "", fmt.Errorf("%s returned no token", ch.Realm)
	}
	return "Bearer " + t, nil
}

func MayReceiveCredentials(realm string, isLocal bool) bool {
	return isLocal || strings.HasPrefix(strings.ToLower(realm), "https://")
}

func TokenURL(realm, service string, scopes []string) string {
	var sb strings.Builder
	sb.WriteString(realm)
	sep := '?'
	if strings.ContainsRune(realm, '?') {
		sep = '&'
	}
	if service != "" {
		sb.WriteRune(sep)
		sb.WriteString("service=")
		sb.WriteString(url.QueryEscape(service))
		sep = '&'
	}
	for _, scope := range scopes {
		sb.WriteRune(sep)
		sb.WriteString("scope=")
		sb.WriteString(url.QueryEscape(scope))
		sep = '&'
	}
	return sb.String()
}

func (r *Registry) findCredential() (*credentials.Credential, error) {
	if r.LookedUp {
		return r.Credential, nil
	}
	c, err := credentials.Lookup(r.Host)
	if err != nil {
		return nil, err
	}
	r.Credential = c
	r.LookedUp = true
	if r.Verbose {
		if c != nil {
			fmt.Fprintf(os.Stderr, "  credentials for %s: %s, %s\n", r.Host, c.User, c.Origin())
		} else {
			defReg := os.Getenv("QUASIDOCK_REGISTRY")
			if defReg == "" {
				defReg = credentials.DefaultRegistry
			}
			if other := credentials.EnvHostOtherThan(defReg, r.Host); other != "" {
				fmt.Fprintf(os.Stderr, "  no credentials for %s; %s and %s only go to %s\n", r.Host, credentials.UsernameVar, credentials.PasswordVar, other)
			} else {
				fmt.Fprintf(os.Stderr, "  no credentials for %s\n", r.Host)
			}
		}
	}
	return r.Credential, nil
}

func (r *Registry) failNeedsLogin() error {
	maybeMissing := ""
	if r.Actions == "pull" {
		maybeMissing = " doesn't exist, or"
	}
	github := ""
	if r.Host == "ghcr.io" {
		github = " with a GitHub token that has the right package scopes"
	}
	return fmt.Errorf("%s%s needs credentials for %s: run `quasidock login %s`%s%s",
		r.Ref, maybeMissing, r.Actions, r.Host, github, r.envNote())
}

func (r *Registry) failRefused(c credentials.Credential) error {
	switch c.Source {
	case credentials.SourceEnvironment:
		return fmt.Errorf("%s refused the credentials %s", r.Ref.Host, c.Origin())
	case credentials.SourceCredentialManager:
		return fmt.Errorf("%s refused the credentials %s; run `quasidock login %s` again", r.Ref.Host, c.Origin(), r.Host)
	default:
		return fmt.Errorf("%s refused the credentials %s; run `docker login %s` again, or `quasidock login %s`", r.Ref.Host, c.Origin(), r.Host, r.Host)
	}
}

func (r *Registry) envNote() string {
	defReg := os.Getenv("QUASIDOCK_REGISTRY")
	if defReg == "" {
		defReg = credentials.DefaultRegistry
	}
	other := credentials.EnvHostOtherThan(defReg, r.Host)
	if other == "" {
		return ""
	}
	return fmt.Sprintf("\n  (%s and %s only go to %s, the default registry)", credentials.UsernameVar, credentials.PasswordVar, other)
}

func (r *Registry) failStatus(res response, what string) error {
	code := res.Status
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		cred, _ := r.findCredential()
		if cred != nil {
			return fmt.Errorf("%s for %s: access denied (HTTP %d); the registry refused the credentials %s, or they don't allow %s",
				what, r.Ref, code, cred.Origin(), r.Actions)
		}
		return fmt.Errorf("%s for %s: access denied (HTTP %d); for private images and pushing, run `quasidock login %s`%s",
			what, r.Ref, code, r.Host, r.envNote())
	case http.StatusNotFound:
		return fmt.Errorf("%s for %s: not found", what, r.Ref)
	}

	var errResp struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(res.Body, &errResp) == nil && len(errResp.Errors) > 0 {
		return fmt.Errorf("%s for %s: HTTP %d %s: %s", what, r.Ref, code, errResp.Errors[0].Code, errResp.Errors[0].Message)
	}
	return fmt.Errorf("%s for %s: HTTP %d", what, r.Ref, code)
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

type LoginCheck int

const (
	LoginAccepted LoginCheck = iota
	LoginRefused
	LoginNotAsked
)

// CheckLogin tests credentials against registry root /v2/.
func CheckLogin(host string, cred credentials.Credential, verbose bool) (LoginCheck, error) {
	ref := Reference{Host: host, Repository: ""}
	reg, err := New(ref, "", verbose)
	if err != nil {
		return LoginRefused, err
	}

	root := reg.URL("/v2/")
	res, err := reg.follow(request{Method: "GET", URL: root}, "")
	if err != nil {
		return LoginRefused, err
	}
	switch res.Status {
	case http.StatusOK:
		return LoginNotAsked, nil
	case http.StatusUnauthorized:
	default:
		return LoginRefused, fmt.Errorf("%s answered %s with HTTP %d; is it an OCI registry?", host, root, res.Status)
	}

	ch := ParseChallenge(res.WWWAuthenticate)
	if ch == nil {
		return LoginRefused, fmt.Errorf("%s refused %s without saying how to authenticate", host, root)
	}

	basic := basicAuthorization(cred)
	var auth string
	switch ch.Scheme {
	case SchemeBasic:
		auth = basic
	case SchemeBearer:
		tok, err := reg.fetchToken(*ch, basic, nil)
		if err != nil || tok == "" {
			return LoginRefused, nil
		}
		auth = tok
	}

	again, err := reg.follow(request{Method: "GET", URL: root}, auth)
	if err != nil {
		return LoginRefused, err
	}
	switch again.Status {
	case http.StatusOK:
		return LoginAccepted, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return LoginRefused, nil
	default:
		return LoginRefused, fmt.Errorf("%s answered %s with HTTP %d after authentication", host, root, again.Status)
	}
}
