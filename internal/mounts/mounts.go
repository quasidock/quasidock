// Package mounts implements data-relative directory mounts without a private
// filesystem namespace. It never follows reparse points during deletion.
package mounts

import (
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/quasidock/quasidock/internal/win32"
)

type Spec struct {
	Type   string `json:"Type"`
	Source string `json:"Source"`
	Target string `json:"Target"`
}

type Mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name,omitempty"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

func validComponent(s string) bool {
	if s == "" || s == "." || s == ".." || strings.TrimRight(s, " .") != s || strings.ContainsAny(s, `<>:"/\|?*`) {
		return false
	}
	for _, r := range s {
		if r < 32 {
			return false
		}
	}
	stem := strings.ToUpper(strings.SplitN(s, ".", 2)[0])
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return false
	}
	return !(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9')
}

func ValidateName(name string) error {
	if len(name) > 128 || !validComponent(name) {
		return fmt.Errorf("invalid volume name %q", name)
	}
	for i, ch := range []byte(name) {
		alnum := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
		if !alnum && (i == 0 || ch != '.' && ch != '_' && ch != '-') {
			return fmt.Errorf("invalid volume name %q", name)
		}
	}
	return nil
}

func NormalizeTarget(target string) (string, error) {
	t := strings.ReplaceAll(target, `\`, "/")
	if t == "${data}" {
		return t, nil
	}
	if !strings.HasPrefix(t, "${data}/") {
		return "", fmt.Errorf("mount target must be ${data} or a subpath: %q", target)
	}
	for _, part := range strings.Split(strings.TrimPrefix(t, "${data}/"), "/") {
		if !validComponent(part) || strings.ContainsAny(part, "${}") {
			return "", fmt.Errorf("invalid mount target %q", target)
		}
	}
	return t, nil
}

func Parse(value string, short bool) (Spec, error) {
	var s Spec
	if short {
		i := strings.Index(value, ":${data}")
		if i < 1 {
			return s, errors.New("volume syntax is SOURCE:${data}[/PATH][:rw]")
		}
		s.Source, s.Target = value[:i], value[i+1:]
		if strings.HasSuffix(s.Target, ":rw") {
			s.Target = strings.TrimSuffix(s.Target, ":rw")
		}
		if strings.HasSuffix(s.Target, ":ro") {
			return s, errors.New("read-only mounts are not supported yet")
		}
		s.Type = "volume"
		if filepath.IsAbs(s.Source) || strings.HasPrefix(s.Source, ".") || strings.ContainsAny(s.Source, `\/:`) {
			s.Type = "bind"
		}
	} else {
		fields, err := csv.NewReader(strings.NewReader(value)).Read()
		if err != nil {
			return s, fmt.Errorf("invalid --mount: %w", err)
		}
		seen := map[string]bool{}
		for _, field := range fields {
			key, val, ok := strings.Cut(field, "=")
			if seen[key] {
				return s, fmt.Errorf("duplicate mount field %q", key)
			}
			seen[key] = true
			if !ok || val == "" {
				return s, fmt.Errorf("mount field %q needs a value (read-only mounts are not supported)", key)
			}
			switch key {
			case "type":
				s.Type = val
			case "source":
				s.Source = val
			case "target":
				s.Target = val
			default:
				return s, fmt.Errorf("unsupported mount field %q", key)
			}
		}
	}
	if s.Type != "volume" && s.Type != "bind" {
		return s, errors.New("mount type must be volume or bind")
	}
	if s.Source == "" || strings.ContainsRune(s.Source, 0) {
		return s, errors.New("mount source is required")
	}
	if s.Type == "volume" {
		if err := ValidateName(s.Source); err != nil {
			return s, err
		}
	}
	var err error
	s.Target, err = NormalizeTarget(s.Target)
	return s, err
}

func Within(path, dir string) bool {
	p, d := strings.ToLower(filepath.Clean(path)), strings.ToLower(filepath.Clean(dir))
	return p == d || strings.HasPrefix(p, strings.TrimRight(d, `\`)+`\`)
}

func ValidateTargets(specs []Spec) error {
	for i, s := range specs {
		if _, err := NormalizeTarget(s.Target); err != nil {
			return err
		}
		for _, other := range specs[:i] {
			a, b := strings.ToLower(s.Target), strings.ToLower(other.Target)
			if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
				return fmt.Errorf("overlapping mount targets %q and %q", other.Target, s.Target)
			}
		}
	}
	return nil
}

func Destination(data, target string) string {
	return filepath.Join(data, strings.TrimPrefix(strings.TrimPrefix(target, "${data}"), "/"))
}

// CanonicalDir resolves source aliases once, before a supervisor changes cwd.
func CanonicalDir(path string) (string, error) {
	p, err := CanonicalPath(path)
	if err != nil {
		return "", fmt.Errorf("mount source %q: %w", path, err)
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("mount source %q must be an existing directory", path)
	}
	if err := win32.RequireLocalNTFS(p); err != nil {
		return "", err
	}
	return filepath.Clean(p), nil
}

// CanonicalPath resolves the opened object, including junctions in ancestors.
// Walking ancestor paths also requires permissions the caller need not have.
func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	h, err := win32.CreateFile(abs, 0, win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE|win32.FILE_SHARE_DELETE, nil, win32.OPEN_EXISTING, win32.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", abs, err)
	}
	defer win32.CloseHandle(h)
	p, err := win32.GetFinalPathNameByHandle(h, 0)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", abs, err)
	}
	if strings.HasPrefix(p, `\\?\UNC\`) {
		p = `\\` + strings.TrimPrefix(p, `\\?\UNC\`)
	} else {
		p = strings.TrimPrefix(p, `\\?\`)
	}
	return filepath.Clean(p), nil
}

func IsReparse(path string) (bool, error) {
	attrs, err := win32.GetFileAttributes(path)
	return attrs&win32.FILE_ATTRIBUTE_REPARSE_POINT != 0, err
}

// Detach removes a retained mount link before permissions are applied to the
// private data tree again. It never follows the link or a reparse ancestor.
func Detach(data string, m Mount) error {
	if !Within(m.Destination, data) {
		return errors.New("mount destination escapes container data")
	}
	parts, err := filepath.Rel(filepath.Dir(data), m.Destination)
	if err != nil {
		return err
	}
	p := filepath.Dir(data)
	for _, part := range strings.Split(parts, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		reparse, err := IsReparse(p)
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		if err != nil {
			return err
		}
		if reparse {
			if !strings.EqualFold(p, filepath.Clean(m.Destination)) {
				return fmt.Errorf("mount destination traverses reparse point %s", p)
			}
			return win32.RemoveDirectory(p)
		}
	}
	return nil
}

// Attach creates an NTFS directory junction, refusing aliases in its ancestry.
func Attach(data string, m Mount) error {
	if !Within(m.Destination, data) {
		return errors.New("mount destination escapes container data")
	}
	if err := win32.RequireLocalNTFS(filepath.Dir(data)); err != nil {
		return err
	}
	parts, err := filepath.Rel(filepath.Dir(data), m.Destination)
	if err != nil {
		return err
	}
	p := filepath.Dir(data)
	for _, part := range strings.Split(parts, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		reparse, err := IsReparse(p)
		if err != nil {
			return err
		}
		if reparse {
			return fmt.Errorf("mount destination traverses reparse point %s", p)
		}
	}
	entries, err := os.ReadDir(m.Destination)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("mount destination is not empty: %s", m.Destination)
	}
	h, err := win32.CreateFile(m.Destination, win32.GENERIC_WRITE, win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE|win32.FILE_SHARE_DELETE, nil, win32.OPEN_EXISTING, win32.FILE_FLAG_BACKUP_SEMANTICS|win32.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer win32.CloseHandle(h)
	sub := syscall.StringToUTF16(`\??\` + m.Source)
	print := syscall.StringToUTF16(m.Source)
	size := 16 + 2*(len(sub)+len(print))
	buf := make([]byte, size)
	binary.LittleEndian.PutUint32(buf[0:4], win32.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buf[4:6], uint16(size-8))
	binary.LittleEndian.PutUint16(buf[10:12], uint16((len(sub)-1)*2))
	binary.LittleEndian.PutUint16(buf[12:14], uint16(len(sub)*2))
	binary.LittleEndian.PutUint16(buf[14:16], uint16((len(print)-1)*2))
	off := 16
	for _, v := range sub {
		binary.LittleEndian.PutUint16(buf[off:], v)
		off += 2
	}
	for _, v := range print {
		binary.LittleEndian.PutUint16(buf[off:], v)
		off += 2
	}
	var n win32.DWORD
	if err := win32.DeviceIoControl(h, win32.FSCTL_SET_REPARSE_POINT, buf, nil, &n, nil); err != nil {
		return fmt.Errorf("creating directory junction: %w", err)
	}
	return nil
}

// RemoveTree deletes links themselves, including unknown or replaced junctions.
// It must only be called on an already validated container/volume directory.
func RemoveTree(path string) error {
	attrs, err := win32.GetFileAttributes(path)
	if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return err
	}
	if attrs&win32.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return os.Remove(path)
	}
	if attrs&win32.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return win32.RemoveDirectory(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := RemoveTree(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func TreeSize(path string) uint64 {
	attrs, err := win32.GetFileAttributes(path)
	if err != nil || attrs&win32.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return 0
	}
	if attrs&win32.FILE_ATTRIBUTE_DIRECTORY == 0 {
		if fi, e := os.Stat(path); e == nil {
			return uint64(fi.Size())
		}
		return 0
	}
	entries, _ := os.ReadDir(path)
	var size uint64
	for _, entry := range entries {
		size += TreeSize(filepath.Join(path, entry.Name()))
	}
	return size
}
