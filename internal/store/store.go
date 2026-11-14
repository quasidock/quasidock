package store

import (
	"crypto/rand"
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
	"time"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/win32"
)

var (
	ErrWouldBlock   = errors.New("lock would block")
	ErrCorruptBlob  = errors.New("blob is corrupt")
	ErrMissingBlob  = errors.New("blob is missing from the store")
	ErrMalformedDig = errors.New("malformed digest")
)

type Deletion int

const (
	DeletionDeleted Deletion = iota
	DeletionInUse
)

type FileHash struct {
	Hex  string
	Size int64
}

// Exists reports whether the file or directory at path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type Store struct {
	Root      string
	Verbose   bool
	LedgerKey string
}

var subdirs = []string{
	"blobs/sha256",
	"refs",
	"deploy",
	"lazy",
	"data",
	"grants",
	"ports",
	"relay",
	"host",
	"certs",
	"cache/downloads",
	"cache/images",
	"cache/builds",
	"cache/tools",
	"cache/work",
	"tmp",
}

// Open creates or opens the local image store at root (or %QUASIDOCK_HOME% / %LOCALAPPDATA%\quasidock).
func Open(root string, verbose bool, ledgerKey string) (*Store, error) {
	if root == "" {
		if home := os.Getenv("QUASIDOCK_HOME"); home != "" {
			root = home
		} else if local := os.Getenv("LOCALAPPDATA"); local != "" {
			root = filepath.Join(local, "quasidock")
		} else {
			return nil, errors.New("neither QUASIDOCK_HOME nor LOCALAPPDATA is set")
		}
	}

	if ledgerKey == "" {
		if k := os.Getenv("QUASIDOCK_GRANTS_KEY"); k != "" {
			ledgerKey = k
		} else {
			ledgerKey = `Software\Quasidock\Grants`
		}
	}

	s := &Store{
		Root:      filepath.Clean(root),
		Verbose:   verbose,
		LedgerKey: ledgerKey,
	}

	for _, sub := range subdirs {
		if err := os.MkdirAll(s.Path(sub), 0o755); err != nil {
			return nil, err
		}
	}

	return s, nil
}

func (s *Store) FullRoot() string {
	eval, err := filepath.EvalSymlinks(s.Root)
	if err == nil {
		return eval
	}
	return s.Root
}

func (s *Store) Path(parts ...string) string {
	all := append([]string{s.Root}, parts...)
	return filepath.Join(all...)
}

func (s *Store) TmpPath(prefix string) string {
	var random [8]byte
	_, _ = rand.Read(random[:])
	name := fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(random[:]))
	return s.Path("tmp", name)
}

func (s *Store) MakeTmpDir(prefix string) (string, error) {
	dir := s.TmpPath(prefix)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Discard moves p to tmp/trash-* and deletes it.
func (s *Store) Discard(p string) error {
	trash := s.TmpPath("trash")
	if err := os.Rename(p, trash); err != nil {
		return err
	}
	return os.RemoveAll(trash)
}

// DeleteTree deletes an entire directory tree.
func DeleteTree(path string) error {
	return os.RemoveAll(path)
}

type RunDir struct {
	Path string
	Lock *Lock
}

func (d *RunDir) Delete() {
	if d == nil {
		return
	}
	_ = os.RemoveAll(d.Path)
	if d.Lock != nil {
		d.Lock.Release()
	}
	_ = os.Remove(LockPathFor(d.Path))
}

func (s *Store) MakeRunDir() (*RunDir, error) {
	dir := s.TmpPath("run")
	inUse, err := OpenLock(LockPathFor(dir), true, true)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		inUse.Release()
		_ = os.Remove(LockPathFor(dir))
		return nil, err
	}
	return &RunDir{Path: dir, Lock: inUse}, nil
}

// ---------------------------------------------------------------------------
// Advisory File Locks via Win32 LockFileEx / UnlockFileEx

type Lock struct {
	h win32.HANDLE
	p string
}

func (l *Lock) Release() {
	if l.h != 0 && l.h != win32.INVALID_HANDLE_VALUE {
		_ = win32.CloseHandle(l.h)
		l.h = 0
	}
}

func LockPathFor(p string) string {
	return p + ".lock"
}

func OpenLock(p string, exclusive bool, wait bool) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}

	// Open with read/write access and full sharing so other processes can also open it to lock
	access := win32.GENERIC_READ | win32.GENERIC_WRITE
	share := win32.FILE_SHARE_READ | win32.FILE_SHARE_WRITE | win32.FILE_SHARE_DELETE
	h, err := win32.CreateFile(p, access, share, nil, win32.OPEN_ALWAYS, win32.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}

	var flags win32.DWORD
	if exclusive {
		flags |= win32.LOCKFILE_EXCLUSIVE_LOCK
	}
	if !wait {
		flags |= win32.LOCKFILE_FAIL_IMMEDIATELY
	}

	ov := win32.Overlapped{}
	if err := win32.LockFileEx(h, flags, 0, 0xFFFFFFFF, 0xFFFFFFFF, &ov); err != nil {
		_ = win32.CloseHandle(h)
		if !wait {
			return nil, ErrWouldBlock
		}
		return nil, err
	}

	return &Lock{h: h, p: p}, nil
}

func TryLock(p string, exclusive bool) (*Lock, error) {
	l, err := OpenLock(p, exclusive, false)
	if errors.Is(err, ErrWouldBlock) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

func IsLocked(p string) bool {
	if _, err := os.Stat(p); err != nil {
		return false
	}
	l, err := TryLock(p, true)
	if err != nil {
		return true
	}
	if l == nil {
		return true
	}
	l.Release()
	return false
}

func (s *Store) Lock(exclusive bool) (*Lock, error) {
	p := s.Path("lock")
	l, err := TryLock(p, exclusive)
	if err != nil {
		return nil, err
	}
	if l != nil {
		return l, nil
	}
	return OpenLock(p, exclusive, true)
}

func (s *Store) LockGrantChanges() (*Lock, error) {
	p := s.Path("grants.lock")
	l, err := TryLock(p, true)
	if err != nil {
		return nil, err
	}
	if l != nil {
		return l, nil
	}
	return OpenLock(p, true, true)
}

// ---------------------------------------------------------------------------
// Blobs

func (s *Store) BlobPath(hexDigest string) string {
	return s.Path("blobs", "sha256", hexDigest)
}

func (s *Store) HasBlob(hexDigest string) bool {
	_, err := os.Stat(s.BlobPath(hexDigest))
	return err == nil
}

func (s *Store) PutBlob(data []byte, mediaType string) (oci.Descriptor, error) {
	sum := sha256.Sum256(data)
	hexDig := hex.EncodeToString(sum[:])
	dest := s.BlobPath(hexDig)

	if !s.HasBlob(hexDig) {
		tmp := s.TmpPath("blob")
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return oci.Descriptor{}, err
		}
		if err := os.Rename(tmp, dest); err != nil {
			_ = os.Remove(tmp)
			if !s.HasBlob(hexDig) {
				return oci.Descriptor{}, err
			}
		}
	}

	return oci.Descriptor{
		MediaType: mediaType,
		Digest:    "sha256:" + hexDig,
		Size:      int64(len(data)),
	}, nil
}

func (s *Store) PutBlobFile(src string, hash FileHash, mediaType string) (oci.Descriptor, error) {
	dest := s.BlobPath(hash.Hex)
	if s.HasBlob(hash.Hex) {
		_ = os.Remove(src)
	} else {
		if err := os.Rename(src, dest); err != nil {
			_ = os.Remove(src)
			if !s.HasBlob(hash.Hex) {
				return oci.Descriptor{}, err
			}
		}
	}

	return oci.Descriptor{
		MediaType: mediaType,
		Digest:    "sha256:" + hash.Hex,
		Size:      hash.Size,
	}, nil
}

func (s *Store) ReadBlob(digest string) ([]byte, error) {
	hexDig := oci.DigestHex(digest)
	if hexDig == "" {
		return nil, fmt.Errorf("%w: %s", ErrMalformedDig, digest)
	}
	p := s.BlobPath(hexDig)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrMissingBlob, digest)
		}
		return nil, err
	}

	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hexDig {
		return nil, fmt.Errorf("%w: %s", ErrCorruptBlob, digest)
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// Manifests & Images

func (s *Store) ReadManifest(manifestDigest string) (*oci.Manifest, error) {
	data, err := s.ReadBlob(manifestDigest)
	if err != nil {
		return nil, err
	}
	var m oci.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest %s cannot be parsed: %w", manifestDigest, err)
	}
	return &m, nil
}

type LoadedImage struct {
	Manifest oci.Manifest
	Config   oci.AppConfig
}

type Image struct {
	Ref         Ref
	Manifest    oci.Manifest
	Config      oci.AppConfig
	DeployDir   string
	RuntimeDirs []string
	Locks       []*Lock
}

func (img *Image) Release() {
	for _, lk := range img.Locks {
		if lk != nil {
			lk.Release()
		}
	}
	img.Locks = nil
}

func (s *Store) UseLayers(layers []oci.Descriptor) ([]string, []*Lock, error) {
	dirs := make([]string, len(layers))
	locks := make([]*Lock, 0, len(layers))
	for i, l := range layers {
		depPath, err := s.DeployPath(l.Digest)
		if err != nil {
			for _, lk := range locks {
				lk.Release()
			}
			return nil, nil, err
		}
		lockPath := LockPathFor(depPath)
		lk, err := OpenLock(lockPath, false, true)
		if err != nil {
			for _, lk2 := range locks {
				lk2.Release()
			}
			return nil, nil, err
		}
		locks = append(locks, lk)
		dir, err := s.Deploy(l)
		if err != nil {
			for _, lk2 := range locks {
				lk2.Release()
			}
			return nil, nil, err
		}
		dirs[i] = dir
	}
	return dirs, locks, nil
}

func (s *Store) LoadImage(id string) (*Image, error) {
	ref, err := s.ReadRef(id)
	if err != nil {
		return nil, err
	}
	if ref == nil {
		return nil, fmt.Errorf("%s is not installed (see `quasidock list`)", id)
	}
	img, err := s.ReadImage(id, ref.Manifest)
	if err != nil {
		return nil, err
	}
	if len(img.Manifest.Layers) == 0 {
		return nil, fmt.Errorf("image %s has no layers", id)
	}
	dirs, locks, err := s.UseLayers(img.Manifest.Layers)
	if err != nil {
		return nil, err
	}
	return &Image{
		Ref:         *ref,
		Manifest:    img.Manifest,
		Config:      img.Config,
		DeployDir:   dirs[len(dirs)-1],
		RuntimeDirs: dirs[:len(dirs)-1],
		Locks:       locks,
	}, nil
}

func (s *Store) DeleteUnusedDeployments(manifestDigest string) (bool, error) {
	used, err := s.UsedLayers(true)
	if err != nil {
		return false, err
	}
	m, err := s.ReadManifest(manifestDigest)
	if err != nil {
		return false, err
	}
	kept := false
	for _, l := range m.Layers {
		hex := oci.DigestHex(l.Digest)
		if hex == "" {
			continue
		}
		if _, ok := used[hex]; ok {
			continue
		}
		del, err := s.DeleteDeployment(l.Digest)
		if err != nil {
			return false, err
		}
		if del == DeletionInUse {
			kept = true
		}
	}
	return kept, nil
}

func (s *Store) ReadImage(what, manifestDigest string) (*LoadedImage, error) {
	manifest, err := s.ReadManifest(manifestDigest)
	if err != nil {
		return nil, err
	}
	configBytes, err := s.ReadBlob(manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	config, err := oci.ParseConfig(what, manifest.Config.MediaType, configBytes)
	if err != nil {
		return nil, err
	}
	if err := oci.ValidateLayers(what, manifest, config); err != nil {
		return nil, err
	}
	return &LoadedImage{
		Manifest: *manifest,
		Config:   *config,
	}, nil
}

func (s *Store) ReadCompleteImage(manifestDigest string) (*LoadedImage, error) {
	hexDig := oci.DigestHex(manifestDigest)
	if hexDig == "" || !s.HasBlob(hexDig) {
		return nil, nil
	}
	m, err := s.ReadManifest(manifestDigest)
	if err != nil {
		return nil, nil
	}
	cHex := oci.DigestHex(m.Config.Digest)
	if cHex == "" || !s.HasBlob(cHex) {
		return nil, nil
	}
	for _, l := range m.Layers {
		lHex := oci.DigestHex(l.Digest)
		if lHex == "" || !s.HasBlob(lHex) {
			return nil, nil
		}
	}
	return s.ReadImage(manifestDigest, manifestDigest)
}

func (s *Store) IsLazy(hexDig string) bool {
	info, err := os.Stat(filepath.Join(s.Root, "lazy", hexDig))
	return err == nil && info.IsDir()
}

func (s *Store) ReadUsableImage(manifestDigest string) (*LoadedImage, error) {
	hexDig := oci.DigestHex(manifestDigest)
	if hexDig == "" || !s.HasBlob(hexDig) {
		return nil, nil
	}
	m, err := s.ReadManifest(manifestDigest)
	if err != nil {
		return nil, nil
	}
	cHex := oci.DigestHex(m.Config.Digest)
	if cHex == "" || !s.HasBlob(cHex) {
		return nil, nil
	}
	for _, l := range m.Layers {
		lHex := oci.DigestHex(l.Digest)
		if lHex == "" || (!s.HasBlob(lHex) && !s.IsLazy(lHex)) {
			return nil, nil
		}
	}
	return s.ReadImage(manifestDigest, manifestDigest)
}

// ---------------------------------------------------------------------------
// Refs: Installed apps

type Ref struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Manifest string `json:"manifest"`
	Source   string `json:"source,omitempty"`
}

func (s *Store) refPath(id string) string {
	return s.Path("refs", id+".json")
}

func (s *Store) ReadRef(id string) (*Ref, error) {
	if !oci.IsValidID(id) {
		return nil, nil
	}
	data, err := os.ReadFile(s.refPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var r Ref
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) WriteRef(ref Ref) error {
	data, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.TmpPath("ref")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.refPath(ref.ID))
}

func (s *Store) DeleteRef(id string) error {
	err := os.Remove(s.refPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) ListRefs() ([]Ref, error) {
	entries, err := os.ReadDir(s.Path("refs"))
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		r, err := s.ReadRef(id)
		if err == nil && r != nil {
			refs = append(refs, *r)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].ID < refs[j].ID
	})
	return refs, nil
}

// ---------------------------------------------------------------------------
// Build cache

func (s *Store) ReadBuildCache(inputsHex string) (string, error) {
	p := s.Path("cache", "builds", inputsHex)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	digest := strings.TrimSpace(string(data))
	if oci.DigestHex(digest) != "" {
		return digest, nil
	}
	return "", nil
}

func (s *Store) WriteBuildCache(inputsHex, manifestDigest string) error {
	tmp := s.TmpPath("build-cache")
	if err := os.WriteFile(tmp, []byte(manifestDigest), 0644); err != nil {
		return err
	}
	dest := s.Path("cache", "builds", inputsHex)
	_ = os.MkdirAll(filepath.Dir(dest), 0755)
	return RenameRetrying(tmp, dest, 5000)
}

// ---------------------------------------------------------------------------
// Deployments

func (s *Store) DeployPath(layerDigest string) (string, error) {
	hexDig := oci.DigestHex(layerDigest)
	if hexDig == "" {
		return "", fmt.Errorf("%w: %s", ErrMalformedDig, layerDigest)
	}
	return s.Path("deploy", hexDig), nil
}

func (s *Store) Deploy(desc oci.Descriptor) (string, error) {
	if _, ok := format.FormatOfDescriptor(desc); !ok {
		return "", fmt.Errorf("layer type %s is not supported", desc.MediaType)
	}
	dest, err := s.DeployPath(desc.Digest)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dest); err == nil {
		_ = acl.Protect(dest)
		_, _ = acl.RevokeAccess(dest, acl.Revoke{Kind: acl.RevokePackageSids})
		return dest, nil
	}

	work, err := s.MakeTmpDir("deploy")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	tree := filepath.Join(work, "app")
	hexDig := oci.DigestHex(desc.Digest)
	blob := s.BlobPath(hexDig)

	if err := format.Deploy(s.Verbose, desc, blob, work, tree); err != nil {
		return "", err
	}

	if err := os.Rename(tree, dest); err != nil {
		return "", err
	}

	_ = acl.Protect(dest)
	return dest, nil
}

func (s *Store) DeleteDeployment(layerDigest string) (Deletion, error) {
	dest, err := s.DeployPath(layerDigest)
	if err != nil {
		return DeletionDeleted, err
	}
	if _, err := os.Stat(dest); err != nil {
		return DeletionDeleted, nil
	}

	lockPath := LockPathFor(dest)
	l, err := TryLock(lockPath, true)
	if err != nil || l == nil {
		return DeletionInUse, nil
	}
	defer l.Release()

	_ = acl.Unprotect(dest)
	if err := s.Discard(dest); err != nil {
		_ = acl.Protect(dest)
		return DeletionInUse, nil
	}
	return DeletionDeleted, nil
}

func (s *Store) DeploymentInUse(layerDigest string) bool {
	dest, err := s.DeployPath(layerDigest)
	if err != nil {
		return false
	}
	return IsLocked(LockPathFor(dest))
}

func (s *Store) UsedLayers(includeBuildImages bool) (map[string]struct{}, error) {
	used := make(map[string]struct{})
	refs, err := s.ListRefs()
	if err != nil {
		return nil, err
	}

	var manifests []string
	for _, r := range refs {
		manifests = append(manifests, r.Manifest)
	}
	if includeBuildImages {
		buildImages, _ := s.ListBuildImages()
		manifests = append(manifests, buildImages...)
	}

	for _, mDig := range manifests {
		m, err := s.ReadManifest(mDig)
		if err != nil {
			continue
		}
		for _, l := range m.Layers {
			if h := oci.DigestHex(l.Digest); h != "" {
				used[h] = struct{}{}
			}
		}
	}
	return used, nil
}

// ---------------------------------------------------------------------------
// Build Cache & Images

func (s *Store) MarkBuildImage(manifestDigest string) error {
	hexDig := oci.DigestHex(manifestDigest)
	if hexDig == "" {
		return fmt.Errorf("%w: %s", ErrMalformedDig, manifestDigest)
	}
	return os.WriteFile(s.Path("cache", "images", hexDig), nil, 0o644)
}

func (s *Store) ListBuildImages() ([]string, error) {
	entries, err := os.ReadDir(s.Path("cache", "images"))
	if err != nil {
		return nil, err
	}
	var digests []string
	for _, e := range entries {
		if e.IsDir() || !oci.IsSha256Hex(e.Name()) {
			continue
		}
		if s.HasBlob(e.Name()) {
			digests = append(digests, "sha256:"+e.Name())
		}
	}
	return digests, nil
}

// ---------------------------------------------------------------------------
// Grants

type GrantKind int

const (
	GrantAppContainer GrantKind = iota
	GrantLow
	GrantLoopback
)

type Grant struct {
	Kind GrantKind
	Path string
}

func ParseGrant(line string) Grant {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "low ") {
		return Grant{Kind: GrantLow, Path: line[4:]}
	}
	if line == "loopback" {
		return Grant{Kind: GrantLoopback, Path: ""}
	}
	return Grant{Kind: GrantAppContainer, Path: line}
}

func (g Grant) String() string {
	switch g.Kind {
	case GrantLow:
		return "low " + g.Path
	case GrantLoopback:
		return "loopback"
	default:
		return g.Path
	}
}

func (g Grant) Equal(other Grant) bool {
	if g.Kind != other.Kind {
		return false
	}
	return strings.EqualFold(g.Path, other.Path)
}

func (s *Store) grantsPath(id string) string {
	return s.Path("grants", id+".txt")
}

func (s *Store) cwdGrantsPath(id string) string {
	return s.Path("grants", id+".cwd")
}

func (s *Store) programsPath(id string) string {
	return s.Path("grants", id+".programs")
}

func (s *Store) RunsLockPath(id string) string {
	return LockPathFor(s.grantsPath(id))
}

func (s *Store) ReadGrants(id string) ([]Grant, error) {
	data, err := os.ReadFile(s.grantsPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var grants []Grant
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			grants = append(grants, ParseGrant(line))
		}
	}
	return grants, nil
}

func (s *Store) RecordGrant(id string, grant Grant) error {
	existing, err := s.ReadGrants(id)
	if err != nil {
		return err
	}
	for _, g := range existing {
		if g.Equal(grant) {
			return nil
		}
	}
	all := append(existing, grant)
	return s.WriteGrants(id, all)
}

func (s *Store) ForgetGrant(id string, grant Grant) error {
	existing, err := s.ReadGrants(id)
	if err != nil {
		return err
	}
	var rest []Grant
	for _, g := range existing {
		if !g.Equal(grant) {
			rest = append(rest, g)
		}
	}
	return s.WriteGrants(id, rest)
}

func (s *Store) WriteGrants(id string, grants []Grant) error {
	var lines []string
	for _, g := range grants {
		lines = append(lines, g.String())
	}
	return s.replaceFile(s.grantsPath(id), strings.Join(lines, "\n"))
}

func (s *Store) ReadCwdGrants(id string) ([]string, error) {
	return s.readLines(s.cwdGrantsPath(id))
}

func (s *Store) RecordCwdGrant(id string, p string) error {
	existing, _ := s.ReadCwdGrants(id)
	for _, e := range existing {
		if strings.EqualFold(e, p) {
			return nil
		}
	}
	all := append(existing, p)
	return s.WriteCwdGrants(id, all)
}

func (s *Store) WriteCwdGrants(id string, paths []string) error {
	return s.replaceFile(s.cwdGrantsPath(id), strings.Join(paths, "\n"))
}

func (s *Store) ReadPrograms(id string) ([]string, error) {
	return s.readLines(s.programsPath(id))
}

func (s *Store) WritePrograms(id string, paths []string) error {
	return s.replaceFile(s.programsPath(id), strings.Join(paths, "\n"))
}

func (s *Store) DeleteGrants(id string) error {
	_ = s.WriteGrants(id, nil)
	_ = s.WriteCwdGrants(id, nil)
	_ = s.WritePrograms(id, nil)
	return nil
}

func (s *Store) ListGrantIds() ([]string, error) {
	entries, err := os.ReadDir(s.Path("grants"))
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		for _, ext := range []string{".txt", ".cwd", ".programs"} {
			if strings.HasSuffix(e.Name(), ext) {
				id := strings.TrimSuffix(e.Name(), ext)
				if oci.IsValidID(id) {
					if _, ok := seen[id]; !ok {
						seen[id] = struct{}{}
						ids = append(ids, id)
					}
				}
				break
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *Store) GrantedToOthers(id string, grant Grant) (bool, error) {
	entries, err := os.ReadDir(s.Path("grants"))
	if err != nil {
		return false, err
	}
	ownFile := strings.ToLower(id + ".txt")
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		if strings.ToLower(e.Name()) == ownFile {
			continue
		}
		grants, err := s.ReadGrants(strings.TrimSuffix(e.Name(), ".txt"))
		if err != nil {
			continue
		}
		for _, g := range grants {
			if g.Equal(grant) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *Store) readLines(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func (s *Store) replaceFile(dest, data string) error {
	data = strings.TrimSpace(data)
	if data == "" {
		err := os.Remove(dest)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	tmp := s.TmpPath("grants")
	if err := os.WriteFile(tmp, []byte(data+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// ---------------------------------------------------------------------------
// Helpers

func RenameRetrying(from, to string, maxWaitMs int) error {
	waited := 0
	for {
		err := os.Rename(from, to)
		if err == nil {
			return nil
		}
		if waited >= maxWaitMs {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		waited += 100
	}
}

func Sha256File(filePath string) (FileHash, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return FileHash{}, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return FileHash{}, err
	}
	return FileHash{
		Hex:  hex.EncodeToString(h.Sum(nil)),
		Size: n,
	}, nil
}
