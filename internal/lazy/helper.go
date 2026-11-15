package lazy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/cloudfiles"
	"github.com/quasidock/quasidock/internal/estargz"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

type Helper struct {
	Store      *store.Store
	Hex        string
	Meta       Meta
	Root       string
	Reader     *BlobReader
	BlobPath   string
	Index      *estargz.Reader
	connection cloudfiles.Connection

	failures atomic.Uint32
	complete atomic.Bool
	stopping atomic.Bool

	errMu    sync.Mutex
	lastErr  string
	unpacked string
}

func RunHelper(st *store.Store, hexDig string, verbose bool) error {
	lockFile := LockPath(st, hexDig, "helper")
	hLock, err := store.OpenLock(lockFile, true, false)
	if err != nil {
		// Another helper is already running
		return nil
	}
	defer hLock.Release()

	meta, err := ReadMeta(st, hexDig)
	if err != nil {
		return err
	}

	tail, err := os.ReadFile(FilePath(st, hexDig, "tail"))
	if err != nil {
		return err
	}

	ref, err := registry.ParseReference(fmt.Sprintf("%s/%s", meta.Host, meta.Repository))
	if err != nil {
		return err
	}

	reader, err := NewBlobReader(ref, meta.Layer.Digest, uint64(meta.Layer.Size), tail, 3)
	if err != nil {
		return err
	}
	defer reader.Close()

	dataPath := FilePath(st, hexDig, "blob.sparse")
	mapPath := FilePath(st, hexDig, "have")
	_, err = reader.Attach(dataPath, mapPath)
	if err != nil {
		return err
	}

	deployRoot, err := st.DeployPath(meta.Layer.Digest)
	if err != nil {
		return err
	}
	h := &Helper{
		Store:    st,
		Hex:      hexDig,
		Meta:     *meta,
		Root:     deployRoot,
		Reader:   reader,
		BlobPath: st.BlobPath(hexDig),
	}

	sr := io.NewSectionReader(reader, 0, meta.Layer.Size)
	idx, err := estargz.Open(sr)
	if err != nil {
		return err
	}
	h.Index = idx
	if want := meta.Layer.Annotations[estargz.TOCJSONDigestAnnotation]; want != "" && want != idx.TOCDigest() {
		return estargz.ErrTOCMismatch
	}
	defer h.stopping.Store(true)

	// Setup cloud files sync root if not already done
	if !cloudfiles.IsSyncRoot(deployRoot) {
		_ = os.MkdirAll(deployRoot, 0755)
		if err := cloudfiles.Register(deployRoot, meta.Layer.Digest, false); err != nil {
			if errors.Is(err, cloudfiles.ErrUnsupported) {
				_ = WriteState(st, hexDig, State{Phase: "unsupported", Size: uint64(meta.Layer.Size), PID: win32.GetCurrentProcessId()})
				return err
			}
			return err
		}

		// Populate placeholders
		if err := h.createAllPlaceholders(); err != nil {
			_ = cloudfiles.Unregister(deployRoot)
			return err
		}
	}

	h.connection, err = cloudfiles.Connect(deployRoot, h.fetch)
	if err != nil {
		return err
	}
	defer h.connection.Disconnect()
	go func() {
		for _, entry := range h.Index.Prioritized() {
			if entry.Type != "reg" || !estargz.IsLayerFile(entry.Name) {
				continue
			}
			status, err := cloudfiles.Hydrate(filepath.Join(h.Root, filepath.FromSlash(entry.Name)))
			if err == nil && status >= 0 {
				fmt.Fprintf(os.Stderr, "hydrated %s\n", entry.Name)
			}
		}
	}()

	// Signal ready event
	readyEvName := EventName(st.Root, hexDig, "ready")
	if ev, err := win32.OpenEvent(readyEvName, win32.EVENT_MODIFY_STATE); err == nil {
		_ = win32.SetEvent(ev)
		_ = win32.CloseHandle(ev)
	}

	_ = WriteState(st, hexDig, h.state("downloading"))

	// Start background download
	downloadDone := make(chan error, 1)
	go func() {
		// Wait until pull lock is released
		pullLockPath := LockPath(st, hexDig, "pull")
		for {
			if h.stopping.Load() {
				return
			}
			gate := os.Getenv("QUASIDOCK_TEST_LAZY_GATE")
			if !store.IsLocked(pullLockPath) && (gate == "" || !store.Exists(gate)) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		downloadDone <- reader.DownloadAll()
	}()

	// Stop event listener
	stopEvName := EventName(st.Root, hexDig, "stop")
	stopEv, _ := win32.CreateEvent(stopEvName, true, false)
	if stopEv != 0 {
		defer win32.CloseHandle(stopEv)
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case err := <-downloadDone:
			if err != nil {
				h.setError("download failed: %v", err)
				_ = WriteState(st, hexDig, h.state("paused"))
				return err
			}
			h.complete.Store(true)
			_ = WriteState(st, hexDig, h.state("unpacking"))
			return h.finish()
		case <-ticker.C:
			if stopEv != 0 && win32.WaitForSingleObject(stopEv, 0) == win32.WAIT_OBJECT_0 {
				_ = WriteState(st, hexDig, h.state("stopped"))
				return nil
			}
			_ = WriteState(st, hexDig, h.state("downloading"))
		}
	}
}

func (h *Helper) fetch(info *cloudfiles.CallbackInfo, params *cloudfiles.FetchDataParams) {
	identity := cloudfiles.IdentityOf(info)
	entries := h.Index.AllEntries()
	if identity == nil || int(*identity) >= len(entries) {
		cloudfiles.Transfer(info, cloudfiles.StatusValidationFailed, nil, uint64(params.RequiredOffset), uint64(params.RequiredLength))
		return
	}
	entry := entries[*identity]
	data, err := func() ([]byte, error) {
		if os.Getenv("QUASIDOCK_TEST_LAZY_FAIL") == entry.Name {
			return nil, errors.New("test fetch failure")
		}
		for at := int64(0); at < entry.Size; {
			chunk, ok := h.Index.Raw().ChunkEntryForOffset(entry.Name, at)
			if !ok {
				return nil, errors.New("missing chunk in layer TOC")
			}
			h.Reader.Prefetch(uint64(chunk.Offset), uint64(chunk.NextOffset()))
			next := chunk.ChunkOffset + chunk.ChunkSize
			if next <= at {
				break
			}
			at = next
		}
		file, err := h.Index.OpenFile(entry.Name)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(file)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(data)
		if "sha256:"+hex.EncodeToString(digest[:]) != entry.Digest {
			return nil, errors.New("file digest does not match layer TOC")
		}
		return data, nil
	}()
	if err != nil {
		h.failures.Add(1)
		h.setError("%s: %v", entry.Name, err)
		fmt.Fprintf(os.Stderr, "fetch failed: %s: %v\n", entry.Name, err)
		cloudfiles.Transfer(info, cloudfiles.StatusUnsuccessful, nil, uint64(params.RequiredOffset), uint64(params.RequiredLength))
		return
	}
	if status := cloudfiles.Transfer(info, cloudfiles.StatusOk, data, 0, uint64(len(data))); status < 0 {
		fmt.Fprintf(os.Stderr, "fetch failed: %s: transfer HRESULT 0x%08x\n", entry.Name, uint32(status))
		return
	}
	fmt.Fprintf(os.Stderr, "fetched %s: %d bytes, for %s\n", entry.Name, len(data), filepath.Base(cloudfiles.ProcessImage(info)))
}

func (h *Helper) createAllPlaceholders() error {
	entries := h.Index.AllEntries()
	var children []cloudfiles.Placeholder

	for i, e := range entries {
		if !estargz.IsLayerFile(e.Name) {
			continue
		}
		isDir := (e.Type == "dir")
		children = append(children, cloudfiles.Placeholder{
			Name:     e.Name,
			Dir:      isDir,
			Size:     uint64(e.Size),
			Identity: uint32(i),
		})
	}

	_, err := cloudfiles.CreatePlaceholders(h.Root, children)
	return err
}

func (h *Helper) finish() error {
	// Verify whole sparse file matches digest
	f, err := os.Open(FilePath(h.Store, h.Hex, "blob.sparse"))
	if err != nil {
		return err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return err
	}
	calcHex := hex.EncodeToString(hasher.Sum(nil))
	expectedHex := oci.DigestHex(h.Meta.Layer.Digest)
	if calcHex != expectedHex {
		return fmt.Errorf("downloaded blob digest mismatch: got %s, want %s", calcHex, expectedHex)
	}
	fmt.Fprintf(os.Stderr, "downloaded and checked the whole blob: %d bytes by the download, %d for reads, %d an earlier helper got\n", h.Reader.Downloaded.Load(), h.Reader.OnDemand.Load(), h.Reader.Earlier)

	// Keep the sparse file open while callbacks and extraction still use it.
	stat, _ := f.Stat()
	_ = f.Close()

	// Unpack to tmp folder
	tmpUnpack := h.Store.TmpPath("lazy-unpack")
	_ = os.MkdirAll(tmpUnpack, 0755)
	if err := h.Index.Extract(tmpUnpack, 4); err != nil {
		_ = store.DeleteTree(tmpUnpack)
		return err
	}

	// Hold the deployment lock throughout replacement so no new run can
	// enter between the last reader exiting and the swap.
	deployment, err := store.OpenLock(store.LockPathFor(h.Root), true, true)
	if err != nil {
		return err
	}
	defer deployment.Release()

	h.connection.Disconnect()
	h.Reader.Close()
	if _, err := h.Store.PutBlobFile(FilePath(h.Store, h.Hex, "blob.sparse"), store.FileHash{Hex: calcHex, Size: stat.Size()}, h.Meta.Layer.MediaType); err != nil {
		return err
	}
	_ = cloudfiles.Unregister(h.Root)
	_ = acl.Unprotect(h.Root)
	if err := store.DeleteTree(h.Root); err != nil {
		return err
	}
	if err := store.RenameRetrying(tmpUnpack, h.Root, 5000); err != nil {
		return err
	}
	if err := acl.Protect(h.Root); err != nil {
		return err
	}

	// Cleanup lazy folder
	return store.DeleteTree(DirPath(h.Store, h.Hex))
}

func (h *Helper) setError(formatStr string, args ...any) {
	h.errMu.Lock()
	defer h.errMu.Unlock()
	h.lastErr = fmt.Sprintf(formatStr, args...)
}

func (h *Helper) state(phase string) State {
	h.errMu.Lock()
	errCopy := h.lastErr
	h.errMu.Unlock()

	var errPtr *string
	if errCopy != "" {
		errPtr = &errCopy
	}

	dl := h.Reader.HaveBytes()
	if h.complete.Load() {
		dl = uint64(h.Meta.Layer.Size)
	}

	return State{
		Phase:      phase,
		Downloaded: dl,
		Size:       uint64(h.Meta.Layer.Size),
		PID:        win32.GetCurrentProcessId(),
		Failures:   h.failures.Load(),
		Error:      errPtr,
		Requests:   h.Reader.Requests.Load(),
		OnDemand:   h.Reader.OnDemand.Load(),
	}
}
