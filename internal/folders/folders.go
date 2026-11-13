package folders

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/zigsaw-io/zigsaw/internal/environment"
	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/override"
	"github.com/zigsaw-io/zigsaw/internal/regkey"
	"github.com/zigsaw-io/zigsaw/internal/store"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

type Grant struct {
	Folder   oci.UserFolder
	ReadOnly bool
}

type HostPath struct {
	Path     string
	ReadOnly bool
}

type Place int

const (
	PlaceMissing Place = iota
	PlaceLink
	PlaceEmptyDir
	PlaceOther
)

const ntPrefix = `\??\`
const shellFoldersKey = `Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`

type folderInfo struct {
	Name  string
	ID    win32.GUID
	Value string
}

func getFolderInfo(folder oci.UserFolder) folderInfo {
	switch folder {
	case oci.UserFolderDesktop:
		return folderInfo{
			Name:  "Desktop",
			Value: "Desktop",
			ID: win32.GUID{
				Data1: 0xB4BFCC3A, Data2: 0xDB2C, Data3: 0x424C,
				Data4: [8]byte{0xB0, 0x29, 0x7F, 0xE9, 0x9A, 0x87, 0xC6, 0x41},
			},
		}
	case oci.UserFolderDocuments:
		return folderInfo{
			Name:  "Documents",
			Value: "Personal",
			ID: win32.GUID{
				Data1: 0xFDD39AD0, Data2: 0x238F, Data3: 0x46AF,
				Data4: [8]byte{0xAD, 0xB4, 0x6C, 0x85, 0x48, 0x03, 0x69, 0xC7},
			},
		}
	case oci.UserFolderDownloads:
		return folderInfo{
			Name:  "Downloads",
			Value: "{374DE290-123F-4565-9164-39C4925E467B}",
			ID: win32.GUID{
				Data1: 0x374DE290, Data2: 0x123F, Data3: 0x4565,
				Data4: [8]byte{0x91, 0x64, 0x39, 0xC4, 0x92, 0x5E, 0x46, 0x7B},
			},
		}
	case oci.UserFolderMusic:
		return folderInfo{
			Name:  "Music",
			Value: "My Music",
			ID: win32.GUID{
				Data1: 0x4BD8D571, Data2: 0x6D19, Data3: 0x48D3,
				Data4: [8]byte{0xBE, 0x97, 0x42, 0x22, 0x20, 0x08, 0x0E, 0x43},
			},
		}
	case oci.UserFolderPictures:
		return folderInfo{
			Name:  "Pictures",
			Value: "My Pictures",
			ID: win32.GUID{
				Data1: 0x33E28130, Data2: 0x4E1E, Data3: 0x4676,
				Data4: [8]byte{0x83, 0x5A, 0x98, 0x39, 0x5C, 0x3B, 0xC3, 0xBB},
			},
		}
	case oci.UserFolderVideos:
		return folderInfo{
			Name:  "Videos",
			Value: "My Video",
			ID: win32.GUID{
				Data1: 0x18989B1D, Data2: 0x99B5, Data3: 0x455B,
				Data4: [8]byte{0x84, 0x1C, 0xAB, 0x7C, 0x74, 0xE4, 0xDD, 0xFC},
			},
		}
	default:
		return folderInfo{Name: string(folder), Value: string(folder)}
	}
}

var allUserFolders = []oci.UserFolder{
	oci.UserFolderDesktop,
	oci.UserFolderDocuments,
	oci.UserFolderDownloads,
	oci.UserFolderMusic,
	oci.UserFolderPictures,
	oci.UserFolderVideos,
}

func Apply(st *store.Store, dataDir string, env []environment.Var, granted []Grant, verbose bool) ([]HostPath, error) {
	var out []HostPath
	var leftOut []string

	for _, folder := range allUserFolders {
		err := applyOne(st, dataDir, env, folder, granted, &out, &leftOut, verbose)
		if err != nil {
			return nil, err
		}
	}

	if len(leftOut) > 0 {
		fmt.Fprintf(os.Stderr, "note: left out your %s: this store, %s, isn't your default one, so its apps don't get your real folders. `zigsaw override --global --real-folders` lets them\n",
			strings.Join(leftOut, ", "), st.Root)
	}
	return out, nil
}

func applyOne(st *store.Store, dataDir string, env []environment.Var, folder oci.UserFolder, granted []Grant, out *[]HostPath, leftOut *[]string, verbose bool) error {
	hook := os.Getenv("ZIGSAW_USER_FOLDERS")
	appP := AppPath(folder, env, hook)
	inData := IsInside(appP, dataDir)

	var wanted *Grant
	for i := range granted {
		if granted[i].Folder == folder {
			if wanted == nil || !granted[i].ReadOnly {
				wanted = &granted[i]
			}
		}
	}

	if wanted == nil {
		if inData {
			_ = Unlink(appP)
		}
		return nil
	}

	allowed, err := MayUseFolders(st)
	if err != nil {
		return err
	}
	if !allowed {
		*leftOut = append(*leftOut, getFolderInfo(folder).Name)
		if inData {
			_ = Unlink(appP)
		}
		return nil
	}

	host, err := HostPathFrom(folder, hook)
	if err != nil || host == "" {
		if verbose {
			fmt.Fprintf(os.Stderr, "folder   %s: you have no such folder\n", folder)
		}
		if inData {
			_ = Unlink(appP)
		}
		return nil
	}

	if IsInside(dataDir, host) {
		fmt.Fprintf(os.Stderr, "note: the app's data directory is in your %s folder, %s, which zigsaw won't give it\n", folder, host)
		if inData {
			_ = Unlink(appP)
		}
		return nil
	}

	*out = append(*out, HostPath{Path: host, ReadOnly: wanted.ReadOnly})
	if verbose {
		fmt.Fprintf(os.Stderr, "folder   %s: %s, at %s\n", folder, host, appP)
	}

	if SamePath(appP, host) {
		return nil
	}
	if !inData {
		fmt.Fprintf(os.Stderr, "note: the app looks for your %s folder at %s, which zigsaw can't link to %s\n", folder, appP, host)
		return nil
	}
	if strings.HasPrefix(host, `\\`) {
		fmt.Fprintf(os.Stderr, "note: %s is on the network, and zigsaw can't link %s to it\n", host, appP)
		return nil
	}

	return Link(appP, host, folder)
}

func HostPathOf(st *store.Store, folder oci.UserFolder) (string, error) {
	allowed, err := MayUseFolders(st)
	if err != nil || !allowed {
		return "", err
	}
	return HostPathFrom(folder, os.Getenv("ZIGSAW_USER_FOLDERS"))
}

func AllHostPaths(st *store.Store) []string {
	var out []string
	hook := os.Getenv("ZIGSAW_USER_FOLDERS")
	profile := hook
	if profile == "" {
		profile = os.Getenv("USERPROFILE")
	}
	if profile != "" {
		out = append(out, profile)
	}
	for _, folder := range allUserFolders {
		if p, err := HostPathFrom(folder, hook); err == nil && p != "" {
			out = append(out, p)
		}
	}
	return out
}

func MayUseFolders(st *store.Store) (bool, error) {
	if os.Getenv("ZIGSAW_USER_FOLDERS") != "" {
		return true, nil
	}
	ov, err := override.Load(st, "")
	if err != nil {
		return false, err
	}
	return RealFoldersAllowed(IsDefaultStore(st), ov.RealFolders), nil
}

func RealFoldersAllowed(defaultStore bool, saved *bool) bool {
	if saved != nil {
		return *saved
	}
	return defaultStore
}

func IsDefaultStore(st *store.Store) bool {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return false
	}
	defaultDir := filepath.Join(local, "zigsaw")
	if SamePath(st.Root, defaultDir) {
		return true
	}
	a, err1 := filepath.EvalSymlinks(st.Root)
	b, err2 := filepath.EvalSymlinks(defaultDir)
	if err1 == nil && err2 == nil && SamePath(a, b) {
		return true
	}
	return false
}

func HostPathFrom(folder oci.UserFolder, hook string) (string, error) {
	info := getFolderInfo(folder)
	if hook != "" {
		p := filepath.Join(hook, info.Name)
		fi, err := os.Stat(p)
		if err == nil && fi.IsDir() {
			return p, nil
		}
		return "", nil
	}
	p, err := win32.SHGetKnownFolderPath(&info.ID, 0, 0)
	if err != nil {
		return "", nil
	}
	return p, nil
}

func AppPath(folder oci.UserFolder, env []environment.Var, hook string) string {
	info := getFolderInfo(folder)
	def := fmt.Sprintf("%%USERPROFILE%%\\%s", info.Name)
	tmpl := def
	if hook == "" {
		if val, err := readShellFolder(info.Value); err == nil && val != "" {
			tmpl = val
		}
	}
	return Expand(tmpl, env)
}

func readShellFolder(name string) (string, error) {
	key, err := regkey.Open(win32.HKEY_CURRENT_USER, shellFoldersKey, win32.KEY_READ)
	if err != nil {
		return "", err
	}
	defer key.Close()
	val, err := key.GetString(name)
	if err != nil || val == nil {
		return "", err
	}
	return *val, nil
}

func Expand(template string, env []environment.Var) string {
	var out strings.Builder
	rest := template
	for {
		start := strings.IndexByte(rest, '%')
		if start == -1 {
			break
		}
		end := strings.IndexByte(rest[start+1:], '%')
		if end == -1 {
			break
		}
		end += start + 1

		out.WriteString(rest[:start])
		name := rest[start+1 : end]

		var foundVal *string
		for _, kv := range env {
			if strings.EqualFold(kv.Name, name) {
				v := kv.Value
				foundVal = &v
				break
			}
		}

		if foundVal != nil {
			out.WriteString(*foundVal)
			rest = rest[end+1:]
		} else {
			out.WriteString(rest[start:end])
			rest = rest[end:]
		}
	}
	out.WriteString(rest)
	return out.String()
}

// ---------------------------------------------------------------------------
// Junctions

func Inspect(path string) (Place, string, error) {
	attrs, err := win32.GetFileAttributes(path)
	if attrs == win32.INVALID_FILE_ATTRIBUTES {
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) {
			return PlaceMissing, "", nil
		}
		return PlaceMissing, "", fmt.Errorf("reading %s: %w", path, err)
	}

	if attrs&win32.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return PlaceOther, "", nil
	}

	if attrs&win32.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		target, err := JunctionTarget(path)
		if err == nil {
			return PlaceLink, target, nil
		}
		return PlaceLink, "", nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return PlaceOther, "", err
	}
	if len(entries) == 0 {
		return PlaceEmptyDir, "", nil
	}
	return PlaceOther, "", nil
}

func Link(at, target string, folder oci.UserFolder) error {
	place, oldTarget, err := Inspect(at)
	if err != nil && place != PlaceMissing {
		return err
	}

	switch place {
	case PlaceMissing:
	case PlaceLink:
		if SamePath(oldTarget, target) {
			return nil
		}
		if err := RemoveLink(at); err != nil {
			return err
		}
	case PlaceEmptyDir:
		if err := RemoveLink(at); err != nil {
			return err
		}
	case PlaceOther:
		fmt.Fprintf(os.Stderr, "note: kept %s, which has the app's own files; move them out, and its %s folder will be yours\n", at, folder)
		return nil
	}

	if parent := filepath.Dir(at); parent != "" {
		_ = os.MkdirAll(parent, 0755)
	}
	return CreateJunction(at, target)
}

func Unlink(at string) error {
	place, _, err := Inspect(at)
	if err != nil {
		return err
	}
	if place == PlaceLink {
		return RemoveLink(at)
	}
	return nil
}

func RemoveLink(path string) error {
	return win32.RemoveDirectory(path)
}

func CreateJunction(at, target string) error {
	if err := os.Mkdir(at, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}

	h, err := win32.CreateFile(
		at,
		win32.GENERIC_WRITE,
		0,
		nil,
		win32.OPEN_EXISTING,
		win32.FILE_FLAG_BACKUP_SEMANTICS|win32.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		_ = win32.RemoveDirectory(at)
		return fmt.Errorf("CreateFile %s: %w", at, err)
	}
	defer win32.CloseHandle(h)

	buf, err := junctionData(target)
	if err != nil {
		return err
	}

	var returned win32.DWORD
	if err := win32.DeviceIoControl(h, win32.FSCTL_SET_REPARSE_POINT, unsafe.Pointer(&buf[0]), win32.DWORD(len(buf)), nil, 0, &returned, nil); err != nil {
		return fmt.Errorf("making %s a link to %s: %w", at, target, err)
	}
	return nil
}

func junctionData(target string) ([]byte, error) {
	substitute := syscall.StringToUTF16(ntPrefix + target) // includes trailing 0
	printName := syscall.StringToUTF16(target)             // includes trailing 0

	subLen := len(substitute) - 1 // character count without null
	printLen := len(printName) - 1

	namesSize := (len(substitute) + len(printName)) * 2
	headerSize := int(unsafe.Sizeof(win32.MOUNT_POINT_REPARSE_HEADER{}))
	dataLength := (headerSize - 8) + namesSize

	if dataLength > win32.MAXIMUM_REPARSE_DATA_BUFFER_SIZE-8 {
		return nil, fmt.Errorf("%s is too long a path to link to", target)
	}

	totalSize := headerSize + namesSize
	buf := make([]byte, totalSize)

	header := (*win32.MOUNT_POINT_REPARSE_HEADER)(unsafe.Pointer(&buf[0]))
	header.ReparseTag = win32.IO_REPARSE_TAG_MOUNT_POINT
	header.ReparseDataLength = uint16(dataLength)
	header.SubstituteNameOffset = 0
	header.SubstituteNameLength = uint16(subLen * 2)
	header.PrintNameOffset = uint16(len(substitute) * 2)
	header.PrintNameLength = uint16(printLen * 2)

	subBytes := unsafe.Slice((*byte)(unsafe.Pointer(&substitute[0])), len(substitute)*2)
	printBytes := unsafe.Slice((*byte)(unsafe.Pointer(&printName[0])), len(printName)*2)

	copy(buf[headerSize:], subBytes)
	copy(buf[headerSize+int(header.PrintNameOffset):], printBytes)

	return buf, nil
}

func JunctionTarget(path string) (string, error) {
	h, err := win32.CreateFile(
		path,
		win32.FILE_READ_ATTRIBUTES,
		win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE|win32.FILE_SHARE_DELETE,
		nil,
		win32.OPEN_EXISTING,
		win32.FILE_FLAG_BACKUP_SEMANTICS|win32.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return "", err
	}
	defer win32.CloseHandle(h)

	buf := make([]byte, win32.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var returned win32.DWORD
	if err := win32.DeviceIoControl(h, win32.FSCTL_GET_REPARSE_POINT, nil, 0, unsafe.Pointer(&buf[0]), win32.DWORD(len(buf)), &returned, nil); err != nil {
		return "", fmt.Errorf("reading link: %w", err)
	}
	return ParseJunction(buf[:returned])
}

func ParseJunction(data []byte) (string, error) {
	headerSize := int(unsafe.Sizeof(win32.MOUNT_POINT_REPARSE_HEADER{}))
	if len(data) < headerSize {
		return "", errors.New("reparse data too short")
	}

	header := (*win32.MOUNT_POINT_REPARSE_HEADER)(unsafe.Pointer(&data[0]))
	if header.ReparseTag != win32.IO_REPARSE_TAG_MOUNT_POINT {
		return "", errors.New("not a mount point")
	}

	names := data[headerSize:]
	start := int(header.SubstituteNameOffset)
	length := int(header.SubstituteNameLength)

	if start+length > len(names) || start%2 != 0 || length%2 != 0 {
		return "", errors.New("invalid substitute name range")
	}

	u16 := make([]uint16, length/2)
	for i := 0; i < len(u16); i++ {
		u16[i] = binary.LittleEndian.Uint16(names[start+i*2 : start+(i+1)*2])
	}
	substitute := syscall.UTF16ToString(u16)
	if strings.HasPrefix(substitute, ntPrefix) {
		return substitute[len(ntPrefix):], nil
	}
	return substitute, nil
}

// ---------------------------------------------------------------------------
// Paths

func IsInside(path, dir string) bool {
	d := strings.TrimRight(dir, `\/`)
	if len(path) <= len(d)+1 || !EqlPaths(path[:len(d)], d) {
		return false
	}
	return path[len(d)] == '\\' || path[len(d)] == '/'
}

func SamePath(a, b string) bool {
	x := strings.TrimRight(a, `\/`)
	y := strings.TrimRight(b, `\/`)
	return len(x) == len(y) && EqlPaths(x, y)
}

func EqlPaths(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x := a[i]
		y := b[i]
		sx := x == '/' || x == '\\'
		sy := y == '/' || y == '\\'
		if sx && sy {
			continue
		}
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
