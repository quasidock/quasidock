package written

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/zigsaw-io/zigsaw/internal/acl"
	"github.com/zigsaw-io/zigsaw/internal/store"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

const walkLimit = 50000

func IsProgram(name string) bool {
	exts := []string{".exe", ".com", ".scr"}
	for _, ext := range exts {
		if strings.HasSuffix(strings.ToLower(name), ext) {
			return true
		}
	}
	return false
}

type Watch struct {
	Path       string
	Handle     win32.HANDLE
	Overlapped win32.OVERLAPPED
	Buffer     [64 * 1024]byte
	Missed     bool
}

type Watcher struct {
	Port    win32.HANDLE
	Watches []*Watch
	Since   time.Time
	mu      sync.Mutex
	Found   []string
	done    chan struct{}
}

func Start(dirs []string) *Watcher {
	if len(dirs) == 0 {
		return nil
	}
	w, err := startWatcher(dirs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: can't watch for the programs this run writes (%v): they'll start at low integrity\n", err)
		return nil
	}
	return w
}

func startWatcher(dirs []string) (*Watcher, error) {
	port, err := win32.CreateIoCompletionPort(win32.INVALID_HANDLE_VALUE, 0, 0, 1)
	if err != nil {
		return nil, err
	}

	var watches []*Watch
	for _, dir := range dirs {
		h, err := win32.CreateFile(
			dir,
			win32.FILE_LIST_DIRECTORY,
			win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE|win32.FILE_SHARE_DELETE,
			nil,
			win32.OPEN_EXISTING,
			win32.FILE_FLAG_BACKUP_SEMANTICS|win32.FILE_FLAG_OVERLAPPED,
			0,
		)
		if err != nil {
			continue
		}
		w := &Watch{Path: dir, Handle: h}
		_, err = win32.CreateIoCompletionPort(h, port, uintptr(unsafe.Pointer(w)), 0)
		if err != nil || !issue(w) {
			win32.CloseHandle(h)
			continue
		}
		watches = append(watches, w)
	}

	self := &Watcher{
		Port:    port,
		Watches: watches,
		Since:   time.Now(),
		done:    make(chan struct{}),
	}
	go self.collect()
	return self, nil
}

func issue(w *Watch) bool {
	w.Overlapped = win32.OVERLAPPED{}
	filter := win32.FILE_NOTIFY_CHANGE_FILE_NAME | win32.FILE_NOTIFY_CHANGE_LAST_WRITE
	err := win32.ReadDirectoryChanges(
		w.Handle,
		unsafe.Pointer(&w.Buffer[0]),
		win32.DWORD(len(w.Buffer)),
		true,
		filter,
		nil,
		&w.Overlapped,
	)
	return err == nil
}

func (self *Watcher) collect() {
	defer close(self.done)
	for {
		var bytesTransferred win32.DWORD
		var key uintptr
		var overlapped *win32.OVERLAPPED
		ok := win32.GetQueuedCompletionStatus(self.Port, &bytesTransferred, &key, &overlapped, win32.INFINITE)
		if key == 0 {
			return
		}
		w := (*Watch)(unsafe.Pointer(key))
		if !ok {
			continue
		}
		if bytesTransferred == 0 {
			w.Missed = true
		} else {
			self.gather(w, bytesTransferred)
		}
		if !issue(w) {
			w.Missed = true
		}
	}
}

func (self *Watcher) gather(w *Watch, bytes win32.DWORD) {
	self.mu.Lock()
	defer self.mu.Unlock()

	var offset win32.DWORD = 0
	for offset+win32.DWORD(unsafe.Sizeof(win32.FILE_NOTIFY_INFORMATION{})) <= bytes {
		info := (*win32.FILE_NOTIFY_INFORMATION)(unsafe.Pointer(&w.Buffer[offset]))
		nameLen := info.FileNameLength / 2
		namePtr := (*[1 << 16]uint16)(unsafe.Pointer(&info.FileName[0]))[:nameLen:nameLen]
		name := syscall.UTF16ToString(namePtr)

		switch info.Action {
		case win32.FILE_ACTION_ADDED, win32.FILE_ACTION_MODIFIED, win32.FILE_ACTION_RENAMED_NEW_NAME:
			if IsProgram(name) {
				self.Found = append(self.Found, filepath.Join(w.Path, name))
			}
		}

		if info.NextEntryOffset == 0 {
			break
		}
		offset += info.NextEntryOffset
	}
}

func (self *Watcher) Stop() ([]string, error) {
	win32.PostQueuedCompletionStatus(self.Port, 0, 0, nil)
	<-self.done

	for _, w := range self.Watches {
		win32.CancelIoEx(w.Handle, nil)
		win32.CloseHandle(w.Handle)
	}
	win32.CloseHandle(self.Port)

	self.mu.Lock()
	candidates := append([]string(nil), self.Found...)
	self.mu.Unlock()

	for _, w := range self.Watches {
		if w.Missed {
			walkDir(w.Path, self.Since, &candidates)
		}
	}

	var programs []string
nextCandidate:
	for _, p := range candidates {
		for _, q := range programs {
			if strings.EqualFold(p, q) {
				continue nextCandidate
			}
		}
		attrs, err := win32.GetFileAttributes(p)
		if err != nil || attrs == win32.INVALID_FILE_ATTRIBUTES || (attrs&win32.FILE_ATTRIBUTE_DIRECTORY != 0) {
			continue
		}
		label, err := acl.EffectiveLabel(p)
		if err != nil || label == nil {
			continue
		}
		if *label <= win32.SECURITY_MANDATORY_LOW_RID {
			programs = append(programs, p)
		}
	}

	return programs, nil
}

func walkDir(dir string, since time.Time, list *[]string) {
	seen := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		seen++
		if seen > walkLimit {
			fmt.Fprintf(os.Stderr, "warning: didn't look through all of %s for the programs this run wrote; those it missed start at low integrity\n", dir)
			return fs.SkipAll
		}
		if d.IsDir() || !IsProgram(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().Add(time.Second).Before(since) {
			return nil
		}
		*list = append(*list, p)
		return nil
	})
}

func Finish(st *store.Store, id string, w *Watcher) {
	programs, err := w.Stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: can't record the programs this run wrote (%v): they'll start at low integrity\n", err)
		return
	}
	if err := Record(st, id, programs); err != nil {
		fmt.Fprintf(os.Stderr, "warning: can't record the programs this run wrote (%v): they'll start at low integrity\n", err)
	}
}

func Record(st *store.Store, id string, programs []string) error {
	if len(programs) == 0 {
		return nil
	}
	lock, err := st.LockGrantChanges()
	if err != nil {
		return err
	}
	defer lock.Release()

	all, _ := st.ReadPrograms(id)
	before := len(all)

nextProgram:
	for _, p := range programs {
		for _, q := range all {
			if strings.EqualFold(p, q) {
				continue nextProgram
			}
		}
		all = append(all, p)
	}

	if err := st.WritePrograms(id, all); err != nil {
		return err
	}

	newCount := len(all) - before
	if newCount > 0 {
		plural := "s"
		if newCount == 1 {
			plural = ""
		}
		fmt.Printf("%d program%s this run wrote will start at medium integrity, not low, once the app's runs have ended\n", newCount, plural)
	}
	return nil
}

func Relabel(st *store.Store, id string) error {
	programs, err := st.ReadPrograms(id)
	if err != nil || len(programs) == 0 {
		return nil
	}
	lock, err := st.LockGrantChanges()
	if err != nil {
		return err
	}
	defer lock.Release()

	for _, p := range programs {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		label, err := acl.EffectiveLabel(p)
		if err == nil && label != nil && *label <= win32.SECURITY_MANDATORY_LOW_RID {
			continue
		}
		if err := acl.LabelProgram(p, acl.IntegrityLow); err != nil {
			continue
		}
	}
	return nil
}

func Settle(st *store.Store, id string) error {
	recorded, err := st.ReadPrograms(id)
	if err != nil || len(recorded) == 0 {
		return nil
	}

	var kept []string
	for _, p := range recorded {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		kept = append(kept, p)
		label, err := acl.EffectiveLabel(p)
		if err != nil || label == nil || *label > win32.SECURITY_MANDATORY_LOW_RID {
			continue
		}
		_ = acl.LabelProgram(p, acl.IntegrityMedium)
	}

	return st.WritePrograms(id, kept)
}
