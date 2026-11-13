package lazy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zigsaw-io/zigsaw/internal/cloudfiles"
	"github.com/zigsaw-io/zigsaw/internal/estargz"
	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/registry"
	"github.com/zigsaw-io/zigsaw/internal/store"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

type Helper struct {
	Store    *store.Store
	Hex      string
	Meta     Meta
	Root     string
	Reader   *BlobReader
	BlobPath string
	Index    *estargz.Reader

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
			if !store.IsLocked(pullLockPath) {
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

	// Move blob into store
	stat, _ := f.Stat()
	_ = f.Close()
	_, err = h.Store.PutBlobFile(
		FilePath(h.Store, h.Hex, "blob.sparse"),
		store.FileHash{Hex: calcHex, Size: stat.Size()},
		h.Meta.Layer.MediaType,
	)
	if err != nil {
		return err
	}

	// Unpack to tmp folder
	tmpUnpack := h.Store.TmpPath("lazy-unpack")
	_ = os.MkdirAll(tmpUnpack, 0755)
	if err := h.Index.Extract(tmpUnpack, 4); err != nil {
		_ = store.DeleteTree(tmpUnpack)
		return err
	}

	// Wait for deployment lock to be free
	for {
		if !h.Store.DeploymentInUse(h.Meta.Layer.Digest) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	_ = cloudfiles.Unregister(h.Root)
	_ = store.DeleteTree(h.Root)
	if err := store.RenameRetrying(tmpUnpack, h.Root, 5000); err != nil {
		return err
	}

	// Cleanup lazy folder
	_ = store.DeleteTree(DirPath(h.Store, h.Hex))
	return nil
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
