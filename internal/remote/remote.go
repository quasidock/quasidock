package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/estargz"
	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/prompt"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/signature"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/trust"
)

const (
	DefaultRegistry = "ghcr.io/zigsaw-io"
	RegistryVar     = "QUASIDOCK_REGISTRY"
	MaxBlobWorkers  = 4
	MinReuseTenths  = 1
)

var (
	ErrInvalidID = errors.New("invalid app id")
)

type Target struct {
	Text  string
	Ref   registry.Reference
	AppID *string
}

type Short struct {
	Text  string
	AppID string
}

// ExpandShort expands <app-id>[:tag][@digest] into <registry>/<app id, lowercased>[:tag][@digest].
// Returns nil for a full reference (which contains a '/').
func ExpandShort(reg, text string) (*Short, error) {
	if strings.ContainsRune(text, '/') {
		return nil, nil
	}
	idEnd := strings.IndexAny(text, ":@")
	if idEnd == -1 {
		idEnd = len(text)
	}
	id := text[:idEnd]
	if !oci.IsValidID(id) {
		return nil, ErrInvalidID
	}
	baseReg := strings.TrimRight(reg, "/")
	full := fmt.Sprintf("%s/%s%s", baseReg, strings.ToLower(id), text[idEnd:])
	return &Short{
		Text:  full,
		AppID: id,
	}, nil
}

// Resolve resolves an image text (short app ID or full reference) into a Target.
func Resolve(envRegistry, text string) (Target, error) {
	if envRegistry == "" {
		envRegistry = DefaultRegistry
	}
	short, err := ExpandShort(envRegistry, text)
	if err != nil {
		return Target{}, fmt.Errorf("%q isn't an app id, or an image reference like ghcr.io/owner/app:tag", text)
	}
	full := text
	if short != nil {
		full = short.Text
	}
	ref, err := registry.ParseReference(full)
	if err != nil {
		if short != nil {
			return Target{}, fmt.Errorf("%s (from %s) isn't a valid image reference; check %s", full, text, RegistryVar)
		}
		return Target{}, fmt.Errorf("%q isn't an image reference like ghcr.io/owner/app:tag or localhost:5000/app@sha256:...", text)
	}
	var appID *string
	if short != nil {
		appID = &short.AppID
	}
	return Target{
		Text:  full,
		Ref:   ref,
		AppID: appID,
	}, nil
}

// ---------------------------------------------------------------------------
// Index and Manifest Classification

type IndexEntry struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type ClassifiedManifest struct {
	IsIndex  bool
	Image    *oci.Manifest
	Index    []IndexEntry
	RawBytes []byte
}

func Classify(ref registry.Reference, data []byte) (*ClassifiedManifest, error) {
	var probe struct {
		MediaType string            `json:"mediaType"`
		Manifests []json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%s returned a manifest that isn't JSON: %w", ref, err)
	}

	if probe.Manifests != nil {
		var index struct {
			Manifests []IndexEntry `json:"manifests"`
		}
		if err := json.Unmarshal(data, &index); err != nil {
			return nil, fmt.Errorf("%s returned an index quasidock can't read: %w", ref, err)
		}
		return &ClassifiedManifest{
			IsIndex:  true,
			Index:    index.Manifests,
			RawBytes: data,
		}, nil
	}

	var m oci.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s returned a manifest quasidock can't read: %w", ref, err)
	}
	return &ClassifiedManifest{
		IsIndex:  false,
		Image:    &m,
		RawBytes: data,
	}, nil
}

func PickPlatform(entries []IndexEntry) *IndexEntry {
	for i := range entries {
		p := entries[i].Platform
		if p == nil {
			continue
		}
		if strings.EqualFold(p.OS, "windows") && strings.EqualFold(p.Architecture, "amd64") {
			return &entries[i]
		}
	}
	return nil
}

func DescribePlatforms(entries []IndexEntry) string {
	var names []string
	seen := make(map[string]bool)
	for _, e := range entries {
		p := e.Platform
		if p == nil || p.OS == "unknown" {
			continue
		}
		name := fmt.Sprintf("%s/%s", p.OS, p.Architecture)
		if p.Variant != "" {
			name = fmt.Sprintf("%s/%s/%s", p.OS, p.Architecture, p.Variant)
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "without platforms"
	}
	return "with images for " + strings.Join(names, ", ")
}

type ResolvedImage struct {
	Bytes    []byte
	Digest   string
	Manifest *oci.Manifest
	Index    *string // index digest if multi-platform
}

func ResolveImage(reg *registry.Registry, ref registry.Reference, unless string) (*ResolvedImage, error) {
	top, err := reg.FetchManifest()
	if err != nil {
		return nil, err
	}
	if ref.Digest != "" && ref.Digest != top.Digest {
		return nil, fmt.Errorf("%s returned a manifest with digest %s", ref, top.Digest)
	}

	cls, err := Classify(ref, top.Bytes)
	if err != nil {
		return nil, err
	}

	if !cls.IsIndex {
		if unless != "" && unless == top.Digest {
			return nil, nil
		}
		return &ResolvedImage{
			Bytes:    top.Bytes,
			Digest:   top.Digest,
			Manifest: cls.Image,
		}, nil
	}

	entry := PickPlatform(cls.Index)
	if entry == nil {
		return nil, fmt.Errorf("%s is a multi-platform index %s; none for windows/amd64, which quasidock runs", ref, DescribePlatforms(cls.Index))
	}
	if unless != "" && unless == entry.Digest {
		return nil, nil
	}

	child, err := reg.FetchManifestRef(entry.Digest)
	if err != nil {
		return nil, err
	}
	if child.Digest != entry.Digest {
		return nil, fmt.Errorf("%s returned a manifest with digest %s for %s", ref, child.Digest, entry.Digest)
	}

	childCls, err := Classify(ref, child.Bytes)
	if err != nil {
		return nil, err
	}
	if childCls.IsIndex {
		return nil, fmt.Errorf("%s: its entry for windows/amd64 is an index too, which quasidock doesn't follow", ref)
	}

	return &ResolvedImage{
		Bytes:    child.Bytes,
		Digest:   child.Digest,
		Manifest: childCls.Image,
		Index:    &top.Digest,
	}, nil
}

// ---------------------------------------------------------------------------
// Signatures

type SignatureCheckKind int

const (
	SigNoKey SignatureCheckKind = iota
	SigMissing
	SigSigned
	SigWrong
)

type SignatureCheck struct {
	Kind        SignatureCheckKind
	Fingerprint string
	Why         string
}

func CheckSignature(st *store.Store, reg *registry.Registry, ref registry.Reference, index *string, digest string) (SignatureCheck, error) {
	repository := fmt.Sprintf("%s/%s", ref.Host, ref.Repository)
	keys, err := trust.ForImage(st, repository)
	if err != nil {
		return SignatureCheck{}, err
	}
	if len(keys) == 0 {
		return SignatureCheck{Kind: SigNoKey}, nil
	}

	if index != nil {
		res, err := checkSignatureOf(reg, keys, repository, *index)
		if err != nil {
			return SignatureCheck{}, err
		}
		if res.Kind != SigMissing {
			return res, nil
		}
	}
	return checkSignatureOf(reg, keys, repository, digest)
}

func checkSignatureOf(reg *registry.Registry, keys []*signature.Key, repository, digest string) (SignatureCheck, error) {
	tag, err := signature.TagFor(digest)
	if err != nil {
		return SignatureCheck{}, err
	}

	found, err := reg.FetchManifestIfThere(tag)
	if err != nil {
		return SignatureCheck{}, err
	}
	if found == nil {
		return SignatureCheck{Kind: SigMissing}, nil
	}

	cls, err := Classify(reg.Ref, found.Bytes)
	if err != nil {
		return SignatureCheck{}, err
	}
	if cls.IsIndex {
		return SignatureCheck{Kind: SigWrong, Why: "what's where its signature goes isn't a signature"}, nil
	}

	why := "what's where its signature goes has no signature in it"
	byAKey := false

	for _, layer := range cls.Image.Layers {
		if layer.MediaType != signature.PayloadMediaType {
			continue
		}
		if layer.Annotations == nil {
			continue
		}
		sigText, ok := layer.Annotations[signature.SignatureAnnotation]
		if !ok {
			continue
		}
		if layer.Size > signature.MaxPayloadSize {
			why = "its signature is about something too big to be an image's"
			continue
		}

		payload := make([]byte, layer.Size)
		buf := &fixedBufferWriter{buf: payload}
		there, err := reg.GetBlob(layer.Digest, registry.Span{Len: uint64Ptr(uint64(layer.Size))}, buf)
		if err != nil {
			return SignatureCheck{}, err
		}
		if !there || buf.n != int(layer.Size) {
			why = "what its signature is about isn't there"
			continue
		}

		var matchedKey *signature.Key
		for _, k := range keys {
			if k.Verifies(string(payload), sigText) {
				matchedKey = k
				break
			}
		}
		if matchedKey == nil {
			if !byAKey {
				why = "it's signed, but not by any key that counts for it"
			}
			continue
		}
		byAKey = true

		if err := signature.CheckPayload(payload, repository, digest); err != nil {
			switch {
			case errors.Is(err, signature.ErrNotAPayload):
				why = "what its signature says isn't what an image's says"
			case errors.Is(err, signature.ErrOtherImage):
				why = "its signature is of another image"
			case errors.Is(err, signature.ErrOtherRepository):
				why = "its signature is of an image in another repository"
			default:
				why = "invalid signature payload"
			}
			continue
		}

		return SignatureCheck{
			Kind:        SigSigned,
			Fingerprint: matchedKey.Fingerprint,
		}, nil
	}

	return SignatureCheck{Kind: SigWrong, Why: why}, nil
}

type fixedBufferWriter struct {
	buf []byte
	n   int
}

func (w *fixedBufferWriter) Write(p []byte) (int, error) {
	avail := len(w.buf) - w.n
	if len(p) > avail {
		return 0, io.ErrShortWrite
	}
	copy(w.buf[w.n:], p)
	w.n += len(p)
	return len(p), nil
}

func uint64Ptr(v uint64) *uint64 {
	return &v
}

// ---------------------------------------------------------------------------
// RemoteBlob & Streaming

type RemoteBlob struct {
	StoreBlob string
	Reg       *registry.Registry
	Desc      oci.Descriptor
	File      *os.File
	Tail      []byte
	TailStart uint64
	Requests  int
	Fetched   uint64
	mu        sync.Mutex
}

func TailLen(size uint64) uint64 {
	val := size / 64
	if val < 16<<10 {
		val = 16 << 10
	}
	if val > 1<<20 {
		val = 1 << 20
	}
	if size < val {
		return size
	}
	return val
}

func OpenRemoteBlob(st *store.Store, reg *registry.Registry, desc oci.Descriptor) (*RemoteBlob, error) {
	b := &RemoteBlob{
		Reg:  reg,
		Desc: desc,
	}
	hexDig := oci.DigestHex(desc.Digest)
	if hexDig != "" && st != nil {
		p := st.BlobPath(hexDig)
		if f, err := os.Open(p); err == nil {
			b.File = f
			b.StoreBlob = p
		}
	}
	return b, nil
}

func (b *RemoteBlob) Close() error {
	if b.File != nil {
		return b.File.Close()
	}
	return nil
}

func (b *RemoteBlob) ReadAt(p []byte, offset int64) (int, error) {
	if b.File != nil {
		return b.File.ReadAt(p, offset)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	size := uint64(b.Desc.Size)
	uOffset := uint64(offset)
	if uOffset >= size || len(p) == 0 {
		return 0, io.EOF
	}

	if b.Tail == nil {
		tLen := TailLen(size)
		b.TailStart = size - tLen
		tailBuf := make([]byte, tLen)
		w := &fixedBufferWriter{buf: tailBuf}
		if err := b.fetchTo(b.TailStart, size, w); err != nil {
			return 0, err
		}
		b.Tail = tailBuf
	}

	if uOffset >= b.TailStart {
		tailSlice := b.Tail[uOffset-b.TailStart:]
		n := copy(p, tailSlice)
		return n, nil
	}

	n := len(p)
	if uint64(n) > b.TailStart-uOffset {
		n = int(b.TailStart - uOffset)
	}

	w := &fixedBufferWriter{buf: p[:n]}
	if err := b.fetchTo(uOffset, uOffset+uint64(n), w); err != nil {
		return 0, err
	}
	return n, nil
}

func (b *RemoteBlob) fetchTo(start, end uint64, out io.Writer) error {
	sentEnd := end
	if b.Tail != nil && b.TailStart < sentEnd {
		sentEnd = b.TailStart
	}
	if start < sentEnd {
		b.Requests++
		span := registry.Span{Start: start, End: &sentEnd}
		ok, err := b.Reg.GetBlob(b.Desc.Digest, span, out)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s doesn't have layer %s", b.Reg.Ref, b.Desc.Digest)
		}
		b.Fetched += (sentEnd - start)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Info

type Info struct {
	Target         Target
	ManifestDigest string
	Index          *string
	Manifest       *oci.Manifest
	Config         *oci.AppConfig
}

func FetchInfo(st *store.Store, envRegistry, imageText string, verbose bool) (Info, error) {
	target, err := Resolve(envRegistry, imageText)
	if err != nil {
		return Info{}, err
	}

	reg, err := registry.New(target.Ref, "pull", verbose)
	if err != nil {
		return Info{}, err
	}

	resolved, err := ResolveImage(reg, target.Ref, "")
	if err != nil {
		return Info{}, err
	}

	if !oci.IsConfigType(resolved.Manifest.Config.MediaType) {
		return Info{}, fmt.Errorf("%s is not a quasidock app (its config type is %s; it may be a container image)", target.Ref, resolved.Manifest.Config.MediaType)
	}

	// Read or download config
	cfgHex := oci.DigestHex(resolved.Manifest.Config.Digest)
	var cfgBytes []byte
	if st != nil && cfgHex != "" {
		if b, err := st.ReadBlob(resolved.Manifest.Config.Digest); err == nil {
			cfgBytes = b
		}
	}
	if cfgBytes == nil {
		tmpPath := filepath.Join(os.TempDir(), "quasidock-cfg-"+cfgHex+".json")
		h, err := reg.DownloadBlob(resolved.Manifest.Config, tmpPath)
		if err != nil {
			return Info{}, err
		}
		if st != nil {
			_, err = st.PutBlobFile(tmpPath, store.FileHash{Hex: h.Hex, Size: int64(h.Size)}, resolved.Manifest.Config.MediaType)
			if err != nil {
				return Info{}, err
			}
		}
		data, err := os.ReadFile(tmpPath)
		_ = os.Remove(tmpPath)
		if err != nil {
			return Info{}, err
		}
		cfgBytes = data
	}

	cfg, err := oci.ParseConfig(target.Text, resolved.Manifest.Config.MediaType, cfgBytes)
	if err != nil {
		return Info{}, err
	}
	if err := oci.ValidateLayers(target.Text, resolved.Manifest, cfg); err != nil {
		return Info{}, err
	}
	if target.AppID != nil && !strings.EqualFold(*target.AppID, cfg.ID) {
		return Info{}, fmt.Errorf("%s holds %s, not %s", target.Ref, cfg.ID, *target.AppID)
	}

	return Info{
		Target:         target,
		ManifestDigest: resolved.Digest,
		Index:          resolved.Index,
		Manifest:       resolved.Manifest,
		Config:         cfg,
	}, nil
}

func Fetch(st *store.Store, envRegistry, imageText string, verbose bool) (*oci.Image, error) {
	info, err := FetchInfo(st, envRegistry, imageText, verbose)
	if err != nil {
		return nil, err
	}

	reg, err := registry.New(info.Target.Ref, "pull", verbose)
	if err != nil {
		return nil, err
	}

	if err := DownloadBlobs(st, reg, info.Manifest.Layers, verbose); err != nil {
		return nil, err
	}

	return &oci.Image{
		ManifestDigest: info.ManifestDigest,
		Manifest:       *info.Manifest,
		Config:         *info.Config,
		Source:         info.Target.Ref.String(),
		Described:      true,
	}, nil
}

// ---------------------------------------------------------------------------
// Concurrent Blob Downloads

func DownloadBlobs(st *store.Store, reg *registry.Registry, descs []oci.Descriptor, verbose bool) error {
	var missing []oci.Descriptor
	for _, desc := range descs {
		hexDig := oci.DigestHex(desc.Digest)
		if hexDig == "" {
			return fmt.Errorf("%s: malformed digest %s", reg.Ref, desc.Digest)
		}
		if !st.HasBlob(desc.Digest) {
			missing = append(missing, desc)
		}
	}

	if len(missing) == 0 {
		return nil
	}
	if len(missing) == 1 {
		return downloadBlob(st, reg, missing[0], verbose)
	}

	workerCount := MaxBlobWorkers
	if len(missing) < workerCount {
		workerCount = len(missing)
	}

	var nextIdx atomic.Uint64
	var firstErr error
	var errMu sync.Mutex

	var wg sync.WaitGroup
	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workerReg, err := registry.New(reg.Ref, reg.Actions, verbose)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				return
			}
			workerReg.SetPullFrom(reg.PullFrom)
			workerReg.Authorization = reg.Authorization
			workerReg.Credential = reg.Credential
			workerReg.LookedUp = reg.LookedUp

			for {
				i := int(nextIdx.Add(1) - 1)
				if i >= len(missing) {
					return
				}
				err := downloadBlob(st, workerReg, missing[i], verbose)
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					nextIdx.Store(uint64(len(missing)))
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

func downloadBlob(st *store.Store, reg *registry.Registry, desc oci.Descriptor, verbose bool) error {
	hexDig := oci.DigestHex(desc.Digest)
	if hexDig == "" {
		return fmt.Errorf("%s: malformed digest %s", reg.Ref, desc.Digest)
	}
	if st.HasBlob(desc.Digest) {
		return nil
	}

	// Try delta update downloadByPlan
	if ok, err := downloadByPlan(st, reg, desc, verbose); err == nil && ok {
		return nil
	}

	if desc.Size > 1<<20 && verbose {
		fmt.Fprintf(os.Stderr, "downloading %s (%d bytes)\n", desc.Digest, desc.Size)
	}

	tmpPath := st.TmpPath("pull")
	defer func() { _ = os.Remove(tmpPath) }()

	hash, err := reg.DownloadBlob(desc, tmpPath)
	if err != nil {
		return err
	}

	_, err = st.PutBlobFile(tmpPath, store.FileHash{Hex: hash.Hex, Size: int64(hash.Size)}, desc.MediaType)
	return err
}

func downloadByPlan(st *store.Store, reg *registry.Registry, desc oci.Descriptor, verbose bool) (bool, error) {
	fmtKind, ok := format.FormatOfDescriptor(desc)
	if !ok || !fmtKind.Indexed() {
		return false, nil
	}

	pool := indexedLayers(st, desc.Digest)
	if len(pool) == 0 {
		return false, nil
	}

	// Open old blobs and readers
	var olds []*estargz.Reader
	var oldBlobs []*RemoteBlob
	defer func() {
		for _, b := range oldBlobs {
			_ = b.Close()
		}
	}()

	for _, d := range pool {
		b, err := OpenRemoteBlob(st, reg, d)
		if err != nil {
			continue
		}
		sr := io.NewSectionReader(b, 0, d.Size)
		r, err := estargz.Open(sr)
		if err != nil {
			_ = b.Close()
			continue
		}
		olds = append(olds, r)
		oldBlobs = append(oldBlobs, b)
	}
	if len(olds) == 0 {
		return false, nil
	}

	newBlob, err := OpenRemoteBlob(st, reg, desc)
	if err != nil {
		return false, err
	}
	defer newBlob.Close()

	newSR := io.NewSectionReader(newBlob, 0, desc.Size)
	newReader, err := estargz.Open(newSR)
	if err != nil {
		return false, err
	}

	plan, err := estargz.MakePlan(newReader, olds, desc.Size)
	if err != nil {
		return false, err
	}
	plan.Coalesce(int64(newBlob.TailStart))

	copied := plan.Copied()
	if copied*10 < desc.Size*MinReuseTenths {
		if verbose {
			fmt.Fprintf(os.Stderr, "  %s: store layers have %d of %d bytes; downloading whole\n",
				oci.ShortDigest(desc.Digest), copied, desc.Size)
		}
		return false, nil
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "downloading %s: %d of %d bytes, rest from layers in store\n",
			desc.Digest, desc.Size-copied, desc.Size)
	}

	tmpPath := st.TmpPath("pull")
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()

	f, err := os.Create(tmpPath)
	if err != nil {
		return false, err
	}
	defer f.Close()

	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)
	copyBuf := make([]byte, 64*1024)
	var at uint64

	for _, piece := range plan.Pieces {
		switch piece.Type {
		case estargz.PieceCopy:
			src := oldBlobs[piece.SourceIndex]
			var done int64
			for done < piece.Length {
				toRead := int64(len(copyBuf))
				if piece.Length-done < toRead {
					toRead = piece.Length - done
				}
				n, err := src.ReadAt(copyBuf[:toRead], piece.SourceOffset+done)
				if err != nil && err != io.EOF {
					return false, err
				}
				if _, err := mw.Write(copyBuf[:n]); err != nil {
					return false, err
				}
				done += int64(n)
			}
		case estargz.PieceFetch:
			if err := newBlob.fetchTo(uint64(piece.Offset), uint64(piece.Offset+piece.Length), mw); err != nil {
				return false, err
			}
		}
		at += uint64(piece.Length)
	}

	hashHex := hex.EncodeToString(hasher.Sum(nil))
	expectedHex := oci.DigestHex(desc.Digest)
	if at != uint64(desc.Size) || hashHex != expectedHex {
		if verbose {
			fmt.Fprintf(os.Stderr, "%s built from layers isn't matching digest; downloading whole\n", desc.Digest)
		}
		return false, nil
	}

	_, err = st.PutBlobFile(tmpPath, store.FileHash{Hex: hashHex, Size: int64(at)}, desc.MediaType)
	if err != nil {
		return false, err
	}
	keep = true
	return true, nil
}

func indexedLayers(st *store.Store, except string) []oci.Descriptor {
	if st == nil {
		return nil
	}
	refs, _ := st.ListRefs()
	var manifests []string
	for _, ref := range refs {
		manifests = append(manifests, ref.Manifest)
	}

	seen := make(map[string]bool)
	var found []oci.Descriptor

	for _, mDig := range manifests {
		m, err := st.ReadManifest(mDig)
		if err != nil {
			continue
		}
		for _, l := range m.Layers {
			f, ok := format.FormatOfDescriptor(l)
			if !ok || !f.Indexed() || l.Digest == except {
				continue
			}
			if seen[l.Digest] {
				continue
			}
			seen[l.Digest] = true
			if st.HasBlob(l.Digest) {
				found = append(found, l)
			}
		}
	}
	return found
}

// ---------------------------------------------------------------------------
// Push & Mounts

type Mounts struct {
	ByDigest map[string]string // digest -> repository
}

func (m Mounts) Repositories() []string {
	var repos []string
	seen := make(map[string]bool)
	for _, r := range m.ByDigest {
		if !seen[r] {
			seen[r] = true
			repos = append(repos, r)
		}
	}
	return repos
}

func MountSources(target registry.Reference, pulledFrom string, manifest oci.Manifest, config oci.AppConfig) Mounts {
	m := Mounts{ByDigest: make(map[string]string)}
	if pulledFrom != "" {
		if pulled, err := registry.ParseReference(pulledFrom); err == nil {
			if strings.EqualFold(pulled.Host, target.Host) && pulled.Repository != target.Repository {
				m.ByDigest[manifest.Config.Digest] = pulled.Repository
				for _, l := range manifest.Layers {
					m.ByDigest[l.Digest] = pulled.Repository
				}
				return m
			}
		}
	}

	namespace := ""
	if idx := strings.LastIndexByte(target.Repository, '/'); idx != -1 {
		namespace = target.Repository[:idx+1]
	}
	for _, r := range config.Runtimes {
		repo := namespace + strings.ToLower(r.ID)
		if repo != target.Repository {
			m.ByDigest[r.Layer] = repo
		}
	}
	return m
}

func UploadIfMissing(st *store.Store, reg *registry.Registry, desc oci.Descriptor, filePath string, mountFrom string, verbose bool) error {
	has, err := reg.HasBlob(desc.Digest)
	if err != nil {
		return err
	}
	if has {
		return nil
	}

	up, err := reg.StartUpload(desc.Digest, mountFrom)
	if err != nil {
		return err
	}
	if up.Kind == registry.UploadMounted {
		if desc.Size > 1<<20 && verbose {
			fmt.Fprintf(os.Stderr, "mounted %s (%d bytes) from %s\n", desc.Digest, desc.Size, mountFrom)
		}
		return nil
	}

	if mountFrom != "" && verbose {
		fmt.Fprintf(os.Stderr, "  couldn't mount %s from %s\n", oci.ShortDigest(desc.Digest), mountFrom)
	}
	if desc.Size > 1<<20 && verbose {
		fmt.Fprintf(os.Stderr, "uploading %s (%d bytes)\n", desc.Digest, desc.Size)
	}

	return reg.FinishUpload(up.Started, desc, filePath)
}

// Consent indicates whether to ask before downloading/installing.
type Consent int

const (
	ConsentNotAsked Consent = iota
	ConsentAsk
	ConsentGiven
)

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func WriteName(w io.Writer, cfg oci.AppConfig) {
	if cfg.Name != nil && *cfg.Name != "" {
		fmt.Fprintf(w, "%s %s: %s\n", cfg.ID, cfg.Version, *cfg.Name)
	} else {
		fmt.Fprintf(w, "%s %s\n", cfg.ID, cfg.Version)
	}
	if cfg.Summary != nil && *cfg.Summary != "" {
		fmt.Fprintf(w, "  %s\n", *cfg.Summary)
	}
}

func WriteContents(st *store.Store, w io.Writer, manifest oci.Manifest, cfg oci.AppConfig) {
	var total uint64 = uint64(manifest.Config.Size)
	var missing uint64 = 0

	for _, l := range manifest.Layers {
		total += uint64(l.Size)
		if st == nil || !st.HasBlob(l.Digest) {
			missing += uint64(l.Size)
		}
	}

	cfgSize := uint64(manifest.Config.Size)
	var dlStr string
	if missing == total-cfgSize {
		dlStr = formatBytes(total)
	} else if missing == 0 {
		dlStr = fmt.Sprintf("%s, all in the store already", formatBytes(total))
	} else {
		dlStr = fmt.Sprintf("%s, of which %s isn't in the store yet", formatBytes(total), formatBytes(missing))
	}
	WriteLine(w, "download", dlStr)

	var runtimes []string
	for _, r := range cfg.Runtimes {
		runtimes = append(runtimes, fmt.Sprintf("%s %s", r.ID, r.Version))
	}
	sort.Strings(runtimes)
	WriteLine(w, "runtimes", strings.Join(runtimes, ", "))

	ownLayer := oci.OwnLayer(&manifest)
	if ownLayer.Digest != "" {
		if f, ok := format.FormatOfDescriptor(ownLayer); ok && f != format.FormatGzip {
			WriteLine(w, "layer", f.String())
		}
	}

	if len(cfg.Extensions) > 0 {
		var pts []string
		for name, p := range cfg.Extensions {
			pts = append(pts, fmt.Sprintf("%s %s", name, p.Version))
		}
		sort.Strings(pts)
		WriteLine(w, "points", fmt.Sprintf("%s (installed extensions of it join its runs)", strings.Join(pts, ", ")))
	}

	if cfg.Extends != nil {
		needsStr := ""
		if len(cfg.Extends.Needs) > 0 {
			needsStr = ", after " + oci.JoinAnd(cfg.Extends.Needs)
		}
		WriteLine(w, "extends", fmt.Sprintf("%s %s%s", cfg.Extends.Point, cfg.Extends.Version, needsStr))
	}

	var cmds []string
	for k := range cfg.Exports {
		cmds = append(cmds, k)
	}
	sort.Strings(cmds)
	WriteLine(w, "commands", strings.Join(cmds, ", "))

	if len(cfg.Permissions.Services) > 0 {
		WriteLine(w, "serves", oci.DescribeServices(cfg.Permissions.Services))
	}

	if cfg.Permissions.Unsupported != nil {
		for _, sb := range []oci.Sandbox{oci.SandboxAppContainer, oci.SandboxLow, oci.SandboxSoft} {
			if why := cfg.Permissions.Unsupported.Of(sb); why != nil && *why != "" {
				WriteLine(w, "can't run", fmt.Sprintf("under %s: %s", sb, *why))
			}
		}
	}

	WriteAccess(w, oci.AccessInfoOf(cfg))
}

func WriteAccess(w io.Writer, access oci.AccessInfo) {
	WriteLine(w, "permits", oci.DescribePermissions(access.Permissions))
	if len(access.Uses) > 0 {
		WriteLine(w, "uses", oci.DescribeUses(access.Uses))
	}
	if len(access.Registry) > 0 {
		WriteLine(w, "keeps", fmt.Sprintf("%s with its data", oci.DescribeRegistry(access.Registry)))
	}
}

func WriteLine(w io.Writer, label, value string) {
	if len(value) > 0 {
		fmt.Fprintf(w, "  %-9s %s\n", label, value)
	}
}

func AgreeTo(ctx *context.Context, manifest oci.Manifest, cfg oci.AppConfig, consent Consent) error {
	var sb strings.Builder
	WriteName(&sb, cfg)
	WriteContents(ctx.Store, &sb, manifest, cfg)
	context.PrintStderr("%s", sb.String())
	if consent != ConsentAsk {
		return nil
	}

	ans, err := prompt.AskYesNo(ctx, "Install it?")
	if err != nil {
		return err
	}
	if ans == prompt.AnswerYes {
		return nil
	}
	why := ""
	if ans == prompt.AnswerNone {
		why = ": nothing answered whether to go on. Run it at a console, or add --yes"
	}
	return fmt.Errorf("%s %s wasn't installed%s", cfg.ID, cfg.Version, why)
}

func ManifestDigest(ctx *context.Context, imageText string) (string, error) {
	envReg := ""
	if ctx != nil && ctx.Env != nil {
		envReg = ctx.Env["QUASIDOCK_REGISTRY"]
	}
	target, err := Resolve(envReg, imageText)
	if err != nil {
		return "", err
	}
	reg, err := registry.New(target.Ref, "pull", ctx.Verbose)
	if err != nil {
		return "", err
	}
	tag := target.Ref.Tag
	if tag == "" {
		tag = target.Ref.Digest
	}
	res, err := reg.FetchManifestIfThere(tag)
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", fmt.Errorf("manifest not found for %s", imageText)
	}
	return res.Digest, nil
}

func SignaturePayload(ctx *context.Context, imageText string) ([]byte, error) {
	envReg := ""
	if ctx != nil && ctx.Env != nil {
		envReg = ctx.Env["QUASIDOCK_REGISTRY"]
	}
	target, err := Resolve(envReg, imageText)
	if err != nil {
		return nil, err
	}
	dig, err := ManifestDigest(ctx, imageText)
	if err != nil {
		return nil, err
	}
	repo := fmt.Sprintf("%s/%s", target.Ref.Host, target.Ref.Repository)
	return []byte(signature.MakePayload(repo, dig)), nil
}

func PushSignature(ctx *context.Context, imageText string, payload []byte, text []byte) error {
	envReg := ""
	if ctx != nil && ctx.Env != nil {
		envReg = ctx.Env["QUASIDOCK_REGISTRY"]
	}
	target, err := Resolve(envReg, imageText)
	if err != nil {
		return err
	}
	parsed, err := signature.ParsePayload(payload)
	if err != nil {
		return errors.New("that isn't what `quasidock signature payload` prints")
	}
	repo := fmt.Sprintf("%s/%s", target.Ref.Host, target.Ref.Repository)
	if !strings.EqualFold(parsed.Repository, repo) {
		return fmt.Errorf("that payload is about %s, not %s", parsed.Repository, repo)
	}
	if oci.DigestHex(parsed.Digest) == "" {
		return fmt.Errorf("that payload's digest isn't one: %s", parsed.Digest)
	}

	reg, err := registry.New(target.Ref, "pull,push", ctx.Verbose)
	if err != nil {
		return err
	}
	tag, err := signature.TagFor(parsed.Digest)
	if err != nil {
		return err
	}

	var layers []oci.Descriptor
	if found, err := reg.FetchManifestIfThere(tag); err == nil && found != nil {
		classified, err := Classify(target.Ref, found.Bytes)
		if err == nil && classified.Image != nil {
			layers = append(layers, classified.Image.Layers...)
		}
	}
	trimmed := strings.Trim(string(text), " \t\r\n")
	for _, layer := range layers {
		if layer.Annotations != nil && layer.Annotations[signature.SignatureAnnotation] == trimmed {
			context.Note("%s has that signature already", tag)
			return nil
		}
	}

	layer, err := ctx.Store.PutBlob(payload, signature.PayloadMediaType)
	if err != nil {
		return err
	}
	if layer.Annotations == nil {
		layer.Annotations = make(map[string]string)
	}
	layer.Annotations[signature.SignatureAnnotation] = trimmed
	if err := UploadIfMissing(ctx.Store, reg, layer, ctx.Store.BlobPath(oci.DigestHex(layer.Digest)), "", ctx.Verbose); err != nil {
		return err
	}
	layers = append(layers, layer)

	var diffIDs []string
	for _, l := range layers {
		diffIDs = append(diffIDs, fmt.Sprintf("%q", l.Digest))
	}
	cfgData := fmt.Sprintf(`{"architecture":"","config":{},"created":"0001-01-01T00:00:00Z","history":[{"created":"0001-01-01T00:00:00Z"}],"os":"","rootfs":{"type":"layers","diff_ids":[%s]}}`, strings.Join(diffIDs, ","))
	cfgDesc, err := ctx.Store.PutBlob([]byte(cfgData), "application/vnd.oci.image.config.v1+json")
	if err != nil {
		return err
	}
	if err := UploadIfMissing(ctx.Store, reg, cfgDesc, ctx.Store.BlobPath(oci.DigestHex(cfgDesc.Digest)), "", ctx.Verbose); err != nil {
		return err
	}

	manifest := oci.Manifest{
		Config: cfgDesc,
		Layers: layers,
	}
	mBytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := reg.PushManifest(tag, mBytes); err != nil {
		return err
	}
	context.Note("signed %s, as %s", parsed.Digest, tag)
	return nil
}

type PushOptions struct {
	Sources bool
}

func Push(ctx *context.Context, id string, imageText string, opts PushOptions) error {
	installed, err := ctx.Store.ReadRef(id)
	if err != nil || installed == nil {
		return fmt.Errorf("%s is not installed", id)
	}
	targetText := id
	if imageText != "" {
		targetText = imageText
	}
	envReg := ""
	if ctx != nil && ctx.Env != nil {
		envReg = ctx.Env["QUASIDOCK_REGISTRY"]
	}
	target, err := Resolve(envReg, targetText)
	if err != nil {
		return err
	}
	ref := target.Ref
	if target.AppID != nil && !strings.EqualFold(*target.AppID, id) {
		return fmt.Errorf("%s is the image of %s; push %s to its own, or give a full image reference", imageText, *target.AppID, id)
	}
	if ref.Digest != "" {
		return fmt.Errorf("push to a tag, not a digest: %s", target.Text)
	}
	if ref.Tag == "" {
		if registry.IsValidTag(installed.Version) {
			ref.Tag = installed.Version
		} else {
			return fmt.Errorf("%s's version %q can't be a tag; add one: %s:<tag>", id, installed.Version, target.Text)
		}
	}

	manifestBytes, err := ctx.Store.ReadBlob(installed.Manifest)
	if err != nil {
		return err
	}
	image, err := ctx.Store.ReadImage(id, installed.Manifest)
	if err != nil || image == nil {
		return fmt.Errorf("reading image for %s: %w", id, err)
	}

	for _, l := range image.Manifest.Layers {
		hex := oci.DigestHex(l.Digest)
		if ctx.Store.IsLazy(hex) && !store.Exists(ctx.Store.BlobPath(hex)) {
			return fmt.Errorf("%s is still being downloaded (`quasidock list` says how far): push it once it's in", id)
		}
	}

	mounts := MountSources(ref, installed.Source, image.Manifest, image.Config)
	reg, err := registry.New(ref, "pull,push", ctx.Verbose)
	if err != nil {
		return err
	}
	reg.PullFrom = mounts.Repositories()

	allDescs := append([]oci.Descriptor{image.Manifest.Config}, image.Manifest.Layers...)
	for _, desc := range allDescs {
		hex := oci.DigestHex(desc.Digest)
		bPath := ctx.Store.BlobPath(hex)
		mountFrom := mounts.ByDigest[desc.Digest]
		if err := UploadIfMissing(ctx.Store, reg, desc, bPath, mountFrom, ctx.Verbose); err != nil {
			return err
		}
	}

	if err := reg.PushManifest(ref.Tag, manifestBytes); err != nil {
		return err
	}
	context.Note("pushed %s %s to %s\n  manifest %s", id, installed.Version, ref.String(), installed.Manifest)
	return nil
}


