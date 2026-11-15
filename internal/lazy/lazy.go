package lazy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/quasidock/quasidock/internal/cloudfiles"
	"github.com/quasidock/quasidock/internal/estargz"
	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

const (
	ExitUnsupported = 3
	DefaultMinSize  = 32 << 20 // 32 MiB
)

type Meta struct {
	Host       string         `json:"host"`
	Repository string         `json:"repository"`
	Layer      oci.Descriptor `json:"layer"`
}

type State struct {
	Phase      string  `json:"phase"`
	Downloaded uint64  `json:"downloaded"`
	Size       uint64  `json:"size"`
	PID        uint32  `json:"pid"`
	Failures   uint32  `json:"failures"`
	Error      *string `json:"error,omitempty"`
	Requests   uint64  `json:"requests"`
	OnDemand   uint64  `json:"on_demand"`
}

func DirPath(st *store.Store, hexDig string) string {
	return filepath.Join(st.Root, "lazy", hexDig)
}

func FilePath(st *store.Store, hexDig, name string) string {
	return filepath.Join(st.Root, "lazy", hexDig, name)
}

func LockPath(st *store.Store, hexDig, which string) string {
	return filepath.Join(st.Root, "lazy", fmt.Sprintf("%s.%s.lock", hexDig, which))
}

func IsLazy(st *store.Store, hexDig string) bool {
	info, err := os.Stat(DirPath(st, hexDig))
	return err == nil && info.IsDir()
}

func ReadState(st *store.Store, hexDig string) (*State, error) {
	data, err := os.ReadFile(FilePath(st, hexDig, "state.json"))
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func WriteState(st *store.Store, hexDig string, state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := FilePath(st, hexDig, "state.json.tmp")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return store.RenameRetrying(tmp, FilePath(st, hexDig, "state.json"), 5000)
}

func ReadMeta(st *store.Store, hexDig string) (*Meta, error) {
	data, err := os.ReadFile(FilePath(st, hexDig, "layer.json"))
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func Describe(st *store.Store, manifest oci.Manifest) *string {
	for _, l := range manifest.Layers {
		hex := oci.DigestHex(l.Digest)
		if hex == "" || !st.IsLazy(hex) {
			continue
		}
		s, err := ReadState(st, hex)
		if err != nil || s == nil {
			str := "(fetching)"
			return &str
		}
		pct := uint64(0)
		if s.Size > 0 {
			pct = s.Downloaded * 100 / s.Size
		}
		live := store.IsLocked(LockPath(st, hex, "helper"))
		if s.Phase == "unpacking" || s.Phase == "swapping" {
			str := "(unpacking)"
			return &str
		}
		if !live || s.Phase == "paused" {
			errStr := ""
			if s.Error != nil && *s.Error != "" {
				errStr = ": " + *s.Error
			}
			str := fmt.Sprintf("(paused at %d%%%s; runs go on)", pct, errStr)
			return &str
		}
		str := fmt.Sprintf("(downloading %d%%)", pct)
		return &str
	}
	return nil
}

func minSize() uint64 {
	if val := os.Getenv("QUASIDOCK_TEST_LAZY_MIN"); val != "" {
		if n, err := strconv.ParseUint(val, 10, 64); err == nil {
			return n
		}
	}
	return DefaultMinSize
}

func Eligible(desc oci.Descriptor) bool {
	f, ok := format.FormatOfDescriptor(desc)
	if !ok || !f.Indexed() {
		return false
	}
	return uint64(desc.Size) >= minSize()
}

type PreparedKind int

const (
	PreparedLazy PreparedKind = iota
	PreparedWhole
)

var pullHolds []*store.Lock

func Prepare(st *store.Store, reg *registry.Registry, desc oci.Descriptor, verbose bool) (PreparedKind, error) {
	if os.Getenv("QUASIDOCK_TEST_LAZY_REFUSE") == "1" {
		fmt.Fprintln(os.Stderr, "  lazy     store volume can't have Cloud Files placeholders, so it's downloaded whole")
		return PreparedWhole, nil
	}
	hexDig := oci.DigestHex(desc.Digest)
	short := oci.ShortDigest(desc.Digest)
	deployPath, err := st.DeployPath(desc.Digest)
	if err != nil {
		return PreparedWhole, err
	}

	if _, err := os.Stat(deployPath); err == nil {
		if IsLazy(st, hexDig) {
			return PreparedLazy, nil
		}
		return PreparedWhole, nil
	}

	blob, err := remote.OpenRemoteBlob(st, reg, desc)
	if err != nil {
		return PreparedWhole, err
	}
	defer blob.Close()

	sr := io.NewSectionReader(blob, 0, desc.Size)
	reader, err := estargz.Open(sr)
	if err != nil {
		if errors.Is(err, registry.ErrRangeIgnored) {
			if verbose {
				fmt.Fprintf(os.Stderr, "  layer %s: %s sends blobs only whole, so it's downloaded whole\n", short, reg.Ref)
			}
			return PreparedWhole, nil
		}
		return PreparedWhole, err
	}

	if err := format.CheckDeployable(desc, reader); err != nil {
		return PreparedWhole, err
	}
	if want := desc.Annotations[estargz.TOCJSONDigestAnnotation]; want != "" && want != reader.TOCDigest() {
		return PreparedWhole, estargz.ErrTOCMismatch
	}

	// Verify all chunks have digests in TOC
	for _, f := range reader.AllEntries() {
		if f.Type == "reg" && estargz.IsLayerFile(f.Name) {
			if f.Digest == "" {
				if verbose {
					fmt.Fprintf(os.Stderr, "  layer %s: TOC doesn't give every chunk's digest, downloading whole\n", short)
				}
				return PreparedWhole, nil
			}
		}
	}

	dir := DirPath(st, hexDig)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return PreparedWhole, err
	}

	keep := false
	defer func() {
		if !keep {
			_ = store.DeleteTree(dir)
		}
	}()

	if err := os.WriteFile(FilePath(st, hexDig, "tail"), blob.Tail, 0644); err != nil {
		return PreparedWhole, err
	}

	metaBytes, err := json.Marshal(Meta{
		Host:       reg.Ref.Host,
		Repository: reg.Ref.Repository,
		Layer:      desc,
	})
	if err != nil {
		return PreparedWhole, err
	}
	if err := os.WriteFile(FilePath(st, hexDig, "layer.json"), metaBytes, 0644); err != nil {
		return PreparedWhole, err
	}

	_ = WriteState(st, hexDig, State{Phase: "starting", Size: uint64(desc.Size)})

	// Acquire shared pull lock
	pLock, err := store.OpenLock(LockPath(st, hexDig, "pull"), false, true)
	if err == nil {
		pullHolds = append(pullHolds, pLock)
	}

	started, err := StartHelper(st, hexDig, verbose)
	if err != nil {
		return PreparedWhole, err
	}
	if started.Unsupported != "" {
		if verbose {
			fmt.Fprintf(os.Stderr, "  layer %s: %s, so it's downloaded whole\n", short, started.Unsupported)
		}
		return PreparedWhole, nil
	}

	keep = true
	fmt.Fprintf(os.Stderr, "  lazy     layer %s is downloading in the background\n", short)
	return PreparedLazy, nil
}

func HelperAlive(st *store.Store, hexDig string) bool {
	return store.IsLocked(LockPath(st, hexDig, "helper"))
}

func EnsureServed(st *store.Store, layers []oci.Descriptor, verbose bool) error {
	for _, l := range layers {
		hexDig := oci.DigestHex(l.Digest)
		if hexDig == "" {
			continue
		}
		if !IsLazy(st, hexDig) {
			continue
		}
		if HelperAlive(st, hexDig) {
			continue
		}
		started, err := StartHelper(st, hexDig, verbose)
		if err != nil {
			return err
		}
		if started.Unsupported != "" {
			return fmt.Errorf("layer %s is still being fetched, and can't be: %s", oci.ShortDigest(l.Digest), started.Unsupported)
		}
	}
	return nil
}

type Started struct {
	Serving     bool
	Unsupported string
}

func EventName(storeRoot, hexDig, which string) string {
	lower := strings.ToLower(strings.TrimRight(storeRoot, "\\/"))
	h := sha256.Sum256([]byte(lower))
	hHex := hex.EncodeToString(h[:8])
	prefix := hexDig
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	return fmt.Sprintf("Local\\quasidock-%s-%s-%s", hHex, prefix, which)
}

func StartHelper(st *store.Store, hexDig string, verbose bool) (Started, error) {
	// One starter at a time
	startLock, err := store.OpenLock(LockPath(st, hexDig, "start"), true, true)
	if err != nil {
		return Started{}, err
	}
	defer startLock.Release()

	if !IsLazy(st, hexDig) || HelperAlive(st, hexDig) {
		return Started{Serving: true}, nil
	}

	readyEvName := EventName(st.Root, hexDig, "ready")
	readyEv, err := win32.CreateEvent(readyEvName, true, false)
	if err != nil {
		return Started{}, err
	}
	defer win32.CloseHandle(readyEv)

	exe, err := helperExe(st)
	if err != nil {
		return Started{}, err
	}

	logPath := FilePath(st, hexDig, "log")
	logHandle, err := win32.CreateFile(logPath, win32.GENERIC_WRITE, win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE|win32.FILE_SHARE_DELETE, nil, win32.OPEN_ALWAYS, win32.FILE_ATTRIBUTE_NORMAL, 0)
	var logFile *os.File
	if err == nil {
		logFile = os.NewFile(logHandle, logPath)
		_, _ = logFile.Seek(0, io.SeekEnd)
		defer logFile.Close()
	}
	newCommand := func(breakaway bool) *exec.Cmd {
		cmd := exec.Command(exe, "lazy-helper", st.Root, hexDig)
		flags := win32.CREATE_NEW_PROCESS_GROUP | win32.DETACHED_PROCESS
		if breakaway {
			flags |= win32.CREATE_BREAKAWAY_FROM_JOB
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: uint32(flags)}
		if logFile != nil {
			cmd.Stderr = logFile
		}
		return cmd
	}
	cmd := newCommand(true)
	if err := cmd.Start(); err != nil {
		if !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return Started{}, fmt.Errorf("starting lazy-helper: %w", err)
		}
		cmd = newCommand(false)
		if err := cmd.Start(); err != nil {
			return Started{}, fmt.Errorf("starting lazy-helper: %w", err)
		}
		fmt.Fprintln(os.Stderr, "note: quasidock is in a job that keeps what it starts; the lazy download lasts as long as that job")
	}

	handles := []win32.HANDLE{readyEv, win32.HANDLE(cmd.Process.Pid)}
	_ = handles

	// Wait up to 120s for ready event
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		res := win32.WaitForSingleObject(readyEv, 500)
		if res == win32.WAIT_OBJECT_0 {
			return Started{Serving: true}, nil
		}

		// Check if helper process exited prematurely
		if !HelperAlive(st, hexDig) {
			s, _ := ReadState(st, hexDig)
			if s != nil && s.Phase == "unsupported" {
				return Started{Unsupported: fmt.Sprintf("store volume can't have Cloud Files placeholders (%s)", volumeOf(st.Root))}, nil
			}
			if !IsLazy(st, hexDig) {
				return Started{Serving: true}, nil
			}
			lastLog := LastLogLine(st, hexDig)
			if lastLog != "" && lastLog != "no log" {
				return Started{}, fmt.Errorf("fetching layer %s didn't start: %s", hexDig[:12], lastLog)
			}
		}
	}

	return Started{}, fmt.Errorf("fetching layer %s didn't start within two minutes: %s", hexDig[:12], LastLogLine(st, hexDig))
}

func volumeOf(path string) string {
	if len(path) >= 2 && path[1] == ':' {
		return path[:2]
	}
	return path
}

func LastLogLine(st *store.Store, hexDig string) string {
	data, err := os.ReadFile(FilePath(st, hexDig, "log"))
	if err != nil || len(data) == 0 {
		return "no log"
	}
	trimmed := strings.TrimRight(string(data), "\r\n ")
	if idx := strings.LastIndexByte(trimmed, '\n'); idx != -1 {
		return trimmed[idx+1:]
	}
	return trimmed
}

func FailedFetches(st *store.Store, hexDig string, skip int) ([]string, error) {
	data, err := os.ReadFile(FilePath(st, hexDig, "log"))
	if err != nil {
		return nil, nil
	}
	var found []string
	seen := 0
	lines := strings.Split(string(data), "\n")
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if strings.HasPrefix(line, "fetch failed: ") {
			seen++
			if seen > skip {
				found = append(found, strings.TrimPrefix(line, "fetch failed: "))
			}
		}
	}
	return found, nil
}

func Stop(st *store.Store, hexDig string) {
	dir := DirPath(st, hexDig)
	if !IsLazy(st, hexDig) {
		return
	}

	lock := LockPath(st, hexDig, "helper")
	if store.IsLocked(lock) {
		stopEvName := EventName(st.Root, hexDig, "stop")
		if ev, err := win32.OpenEvent(stopEvName, win32.EVENT_MODIFY_STATE); err == nil {
			_ = win32.SetEvent(ev)
			_ = win32.CloseHandle(ev)
		}

		waited := 0
		for waited < 15000 {
			if !store.IsLocked(lock) {
				break
			}
			time.Sleep(50 * time.Millisecond)
			waited += 50
		}

		if store.IsLocked(lock) {
			if s, _ := ReadState(st, hexDig); s != nil && s.PID != 0 {
				if p, err := win32.OpenProcess(win32.PROCESS_TERMINATE|win32.SYNCHRONIZE, false, s.PID); err == nil {
					_ = win32.TerminateProcess(p, 1)
					_ = win32.WaitForSingleObject(p, 5000)
					_ = win32.CloseHandle(p)
				}
			}
		}
	}
	_ = store.DeleteTree(dir)
}

func helperExe(st *store.Store) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return "", err
	}

	h := sha256.Sum256(data)
	hStr := hex.EncodeToString(h[:8])
	dest := filepath.Join(st.Root, "lazy", fmt.Sprintf("quasidock-%s.exe", hStr))

	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	tmp := st.TmpPath("helper")
	if err := os.WriteFile(tmp, data, 0755); err != nil {
		return "", err
	}

	if err := store.RenameRetrying(tmp, dest, 5000); err != nil {
		_ = os.Remove(tmp)
		if _, statErr := os.Stat(dest); statErr == nil {
			return dest, nil
		}
		return "", err
	}
	return dest, nil
}

func Prune(st *store.Store, currentExeName string) (int, error) {
	used, err := st.UsedLayers(true)
	if err != nil {
		return 0, err
	}
	lazyDir := filepath.Join(st.Root, "lazy")
	entries, err := os.ReadDir(lazyDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	deleted := 0
	for _, entry := range entries {
		name := entry.Name()
		p := filepath.Join(lazyDir, name)

		if oci.IsSha256Hex(name) {
			if _, needed := used[name]; needed || st.DeploymentInUse("sha256:"+name) {
				continue
			}
			Stop(st, name)
			deployPath, err := st.DeployPath("sha256:" + name)
			if err == nil {
				if _, err := os.Stat(deployPath); err == nil {
					_ = cloudfiles.Unregister(deployPath)
					_, _ = st.DeleteDeployment("sha256:" + name)
				}
			}
			if HelperAlive(st, name) {
				continue
			}
			if err := store.DeleteTree(p); err == nil {
				deleted++
			}
		} else if strings.HasSuffix(name, ".lock") {
			dot := strings.IndexByte(name, '.')
			if dot != -1 {
				dirP := filepath.Join(lazyDir, name[:dot])
				if _, err := os.Stat(dirP); err == nil {
					continue
				}
				if store.IsLocked(p) {
					continue
				}
				if os.Remove(p) == nil {
					deleted++
				}
			}
		} else if strings.HasPrefix(name, "quasidock-") && strings.HasSuffix(name, ".exe") {
			if currentExeName != "" && name == currentExeName {
				continue
			}
			if os.Remove(p) == nil {
				deleted++
			}
		}
	}
	return deleted, nil
}
