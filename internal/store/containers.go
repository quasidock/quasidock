package store

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
)

var (
	ErrContainerNotFound    = errors.New("container not found")
	ErrContainerNameInUse   = errors.New("container name is already in use")
	ErrInvalidContainerName = errors.New("invalid container name")
	ErrContainerRunning     = errors.New("container is running")
)

type ContainerState struct {
	ExecProtocol int    `json:"ExecProtocol,omitempty"`
	RunID        string `json:"RunID,omitempty"`
	Status       string `json:"Status"`
	Running      bool   `json:"Running"`
	Pid          uint32 `json:"Pid"`
	ExitCode     int    `json:"ExitCode"`
	Error        string `json:"Error,omitempty"`
	StartedAt    string `json:"StartedAt,omitempty"`
	FinishedAt   string `json:"FinishedAt,omitempty"`
	Launched     bool   `json:"Launched,omitempty"`
}

type ContainerConfig struct {
	Image       string   `json:"Image"`
	Entrypoint  []string `json:"Entrypoint"`
	Cmd         []string `json:"Cmd"`
	Env         []string `json:"Env"`
	WorkingDir  string   `json:"WorkingDir"`
	Tty         bool     `json:"Tty"`
	AttachStdin bool     `json:"AttachStdin"`
	OpenStdin   bool     `json:"OpenStdin"`
}

type ContainerHostConfig struct {
	Mounts       []mounts.Spec `json:"Mounts,omitempty"`
	AutoRemove   bool          `json:"AutoRemove"`
	Sandbox      string        `json:"Sandbox,omitempty"`
	Network      *bool         `json:"Network,omitempty"`
	Host         *bool         `json:"Host,omitempty"`
	Filesystem   []string      `json:"Filesystem,omitempty"`
	NoFilesystem []string      `json:"NoFilesystem,omitempty"`
	Ports        []string      `json:"Ports,omitempty"`
}

// ContainerRunSpec records the original launch inputs, independently of the
// resolved command, so retained containers can be started again.
type ContainerRunSpec struct {
	// A nil value records an explicitly inherited variable that was absent.
	InheritedEnv map[string]*string `json:"InheritedEnv,omitempty"`
	Args         []string           `json:"Args,omitempty"`
	Entrypoint   *string            `json:"Entrypoint,omitempty"`
	Env          []string           `json:"Env,omitempty"`
	HostCwd      string             `json:"HostCwd,omitempty"`
}

type ContainerRecord struct {
	Mounts     []mounts.Mount      `json:"Mounts,omitempty"`
	ID         string              `json:"Id"`
	Created    string              `json:"Created"`
	Name       string              `json:"Name"`
	Image      string              `json:"Image"`
	ImageRef   string              `json:"ImageRef"`
	AppID      string              `json:"AppId"`
	AppVersion string              `json:"AppVersion"`
	Path       string              `json:"Path"`
	Args       []string            `json:"Args"`
	State      ContainerState      `json:"State"`
	Config     ContainerConfig     `json:"Config"`
	HostConfig ContainerHostConfig `json:"HostConfig"`
	RunSpec    ContainerRunSpec    `json:"RunSpec,omitempty"`
	DataDir    string              `json:"DataDir"`
	LogPath    string              `json:"LogPath"`
}

func (c *ContainerRecord) ShortID() string {
	if len(c.ID) >= 12 {
		return c.ID[:12]
	}
	return c.ID
}

func (c *ContainerRecord) BareName() string {
	return strings.TrimPrefix(c.Name, "/")
}

// ValidateContainerName checks Docker-compatible container names while rejecting 64-hex IDs.
func ValidateContainerName(name string) (string, error) {
	bare := strings.TrimPrefix(name, "/")
	if bare == "" {
		return "", fmt.Errorf("%w: name cannot be empty", ErrInvalidContainerName)
	}
	for i := 0; i < len(bare); i++ {
		ch := bare[i]
		alnum := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
		if i == 0 {
			if !alnum {
				return "", fmt.Errorf("%w %q: must start with an alphanumeric character", ErrInvalidContainerName, name)
			}
			continue
		}
		if !alnum && ch != '_' && ch != '.' && ch != '-' {
			return "", fmt.Errorf("%w %q: only [a-zA-Z0-9_.-] are allowed", ErrInvalidContainerName, name)
		}
	}
	if oci.IsSha256Hex(strings.ToLower(bare)) {
		return "", fmt.Errorf("%w %q: cannot look like a 64-character container ID", ErrInvalidContainerName, name)
	}
	return bare, nil
}

var (
	nameAdjectives = []string{
		"admiring", "adoring", "agitated", "amazing", "angry", "awesome", "beautiful",
		"blissful", "bold", "boring", "brave", "busy", "charming", "clever", "cool",
		"compassionate", "competent", "condescending", "confident", "cranky", "crazy",
		"dazzling", "determined", "distracted", "dreamy", "eager", "ecstatic", "elastic",
		"elated", "elegant", "eloquent", "epic", "exciting", "fervent", "festive",
	}
	nameNouns = []string{
		"albattani", "allen", "almeida", "antonelli", "agnesi", "archimedes", "ardinghelli",
		"aryabhata", "austin", "babbage", "banach", "banzai", "bardeen", "bartik", "bassi",
		"beaver", "bell", "benz", "bhabha", "bhaskara", "black", "blackburn", "blackwell",
		"bohr", "booth", "borg", "bose", "bouman", "boyd", "brahmagupta", "brattain",
		"brown", "buck", "burnell", "cannon",
	}
)

func randomInt(n int) int {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return int(binary.LittleEndian.Uint32(b[:]) % uint32(n))
}

func generateContainerID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *Store) containersLock(exclusive bool) (*Lock, error) {
	return OpenLock(s.Path("containers", "index.lock"), exclusive, true)
}

func (s *Store) IsContainerRunning(id string) bool {
	return IsLocked(s.ContainerRunLockPath(id))
}

// ContainerOperation serializes commands without holding the container index
// while waiting for a supervisor. Its lock lives outside the removable record.
type ContainerOperation struct {
	store *Store
	id    string
	lock  *Lock
}

func (s *Store) ContainerOperationLockPath(id string) string {
	return s.Path("containers", "operations", id+".lock")
}

func (s *Store) LockContainer(id string) (*ContainerOperation, error) {
	if !oci.IsSha256Hex(id) {
		return nil, fmt.Errorf("invalid container ID %q", id)
	}
	lk, err := OpenLock(s.ContainerOperationLockPath(id), true, true)
	if err != nil {
		return nil, err
	}
	return &ContainerOperation{store: s, id: id, lock: lk}, nil
}

func (o *ContainerOperation) Release() { o.lock.Release() }

// Remove removes the stopped container while the caller owns its operation lock.
func (o *ContainerOperation) Remove() error {
	return o.store.removeContainer(o.id)
}

func (s *Store) writeContainerStateNoLock(rec *ContainerRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.TmpPath("container-state")
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return RenameRetrying(tmp, s.ContainerStatePath(rec.ID), 5000)
}

func (s *Store) deleteContainerDirNoLock(id string) error {
	dir := s.ContainerDir(id)
	if !Exists(dir) {
		return nil
	}
	trash := s.TmpPath("trash-container")
	if err := RenameRetrying(dir, trash, 2000); err == nil {
		return mounts.RemoveTree(trash)
	}
	return mounts.RemoveTree(dir)
}

func (s *Store) readRawContainer(id string) (*ContainerRecord, error) {
	data, err := os.ReadFile(s.ContainerStatePath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrContainerNotFound, id)
		}
		return nil, err
	}
	var rec ContainerRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("reading container %s: %w", id, err)
	}
	return &rec, nil
}

// reconcileContainerNoLock repairs stale running state after a crash.
// If the container had AutoRemove set and died unexpectedly, it removes the directory
// and returns (nil, nil).
func (s *Store) reconcileContainerNoLock(rec *ContainerRecord) (*ContainerRecord, error) {
	if rec == nil {
		return nil, nil
	}
	staleRunning := rec.State.Running || (rec.State.Status == "created" && rec.State.Launched)
	staleAutoRemove := rec.HostConfig.AutoRemove && rec.State.Status == "exited" && rec.State.Launched
	if (!staleRunning && !staleAutoRemove) || s.IsContainerRunning(rec.ID) {
		return rec, nil
	}
	if rec.HostConfig.AutoRemove && !IsLocked(s.ContainerOperationLockPath(rec.ID)) {
		_ = s.deleteContainerDirNoLock(rec.ID)
		return nil, nil
	}
	rec.State.Running = false
	rec.State.Status = "exited"
	rec.State.Pid = 0
	if rec.State.FinishedAt == "" {
		if rec.State.ExitCode == 0 {
			rec.State.ExitCode = 137
		}
		rec.State.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_ = s.writeContainerStateNoLock(rec)
	return rec, nil
}

func (s *Store) listContainersNoLock(all bool) ([]ContainerRecord, error) {
	dir := s.Path("containers")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []ContainerRecord
	for _, entry := range entries {
		if !entry.IsDir() || !oci.IsSha256Hex(entry.Name()) {
			continue
		}
		rec, err := s.readRawContainer(entry.Name())
		if err != nil {
			continue
		}
		rec, err = s.reconcileContainerNoLock(rec)
		if err != nil || rec == nil {
			continue
		}
		if !all && !rec.State.Running {
			continue
		}
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created > out[j].Created
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Store) CreateContainer(explicitName string, rec ContainerRecord) (*ContainerRecord, error) {
	lk, err := s.containersLock(true)
	if err != nil {
		return nil, err
	}
	defer lk.Release()

	existing, err := s.listContainersNoLock(true)
	if err != nil {
		return nil, err
	}
	usedNames := make(map[string]string, len(existing))
	for _, c := range existing {
		usedNames[strings.ToLower(c.BareName())] = c.ID
	}

	var bareName string
	if explicitName != "" {
		bareName, err = ValidateContainerName(explicitName)
		if err != nil {
			return nil, err
		}
		if owner, exists := usedNames[strings.ToLower(bareName)]; exists {
			return nil, fmt.Errorf("%w: name %q is already assigned to container %s", ErrContainerNameInUse, bareName, owner[:12])
		}
	} else {
		for attempt := 0; attempt < 200; attempt++ {
			candidate := nameAdjectives[randomInt(len(nameAdjectives))] + "_" + nameNouns[randomInt(len(nameNouns))]
			if attempt > 50 {
				candidate = fmt.Sprintf("%s_%d", candidate, attempt)
			}
			if _, exists := usedNames[strings.ToLower(candidate)]; !exists {
				bareName = candidate
				break
			}
		}
		if bareName == "" {
			suffix, err := generateContainerID()
			if err != nil {
				return nil, err
			}
			bareName = "container_" + suffix[:12]
		}
	}

	var id string
	for attempt := 0; attempt < 16; attempt++ {
		cand, err := generateContainerID()
		if err != nil {
			return nil, err
		}
		if !Exists(s.ContainerDir(cand)) {
			id = cand
			break
		}
	}
	if id == "" {
		return nil, errors.New("failed to allocate unique container ID")
	}

	if err := os.MkdirAll(s.ContainerDataDir(id), 0o755); err != nil {
		return nil, err
	}

	rec.ID = id
	rec.Name = "/" + bareName
	if rec.Created == "" {
		rec.Created = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if rec.State.Status == "" {
		rec.State.Status = "created"
	}
	rec.DataDir = s.ContainerDataDir(id)
	rec.LogPath = s.ContainerLogPath(id)
	if err := s.resolveContainerMountsNoLock(&rec); err != nil {
		_ = mounts.RemoveTree(s.ContainerDir(id))
		return nil, err
	}
	if rec.Args == nil {
		rec.Args = []string{}
	}
	if rec.Config.Entrypoint == nil {
		rec.Config.Entrypoint = []string{}
	}
	if rec.Config.Cmd == nil {
		rec.Config.Cmd = []string{}
	}
	if rec.Config.Env == nil {
		rec.Config.Env = []string{}
	}

	if err := s.writeContainerStateNoLock(&rec); err != nil {
		_ = mounts.RemoveTree(s.ContainerDir(id))
		return nil, err
	}
	return &rec, nil
}

func (s *Store) ReadContainer(id string) (*ContainerRecord, error) {
	lk, err := s.containersLock(true)
	if err != nil {
		return nil, err
	}
	defer lk.Release()

	rec, err := s.readRawContainer(id)
	if err != nil {
		return nil, err
	}
	rec, err = s.reconcileContainerNoLock(rec)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("%w: %s", ErrContainerNotFound, id)
	}
	return rec, nil
}

func (s *Store) UpdateContainer(id string, mutate func(*ContainerRecord) error) (*ContainerRecord, error) {
	lk, err := s.containersLock(true)
	if err != nil {
		return nil, err
	}
	defer lk.Release()

	rec, err := s.readRawContainer(id)
	if err != nil {
		return nil, err
	}
	if err := mutate(rec); err != nil {
		return nil, err
	}
	if err := s.writeContainerStateNoLock(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *Store) ListContainers(all bool) ([]ContainerRecord, error) {
	lk, err := s.containersLock(true)
	if err != nil {
		return nil, err
	}
	defer lk.Release()
	return s.listContainersNoLock(all)
}

func (s *Store) ResolveContainer(text string) (*ContainerRecord, error) {
	if text == "" {
		return nil, fmt.Errorf("%w: %q", ErrContainerNotFound, text)
	}
	all, err := s.ListContainers(true)
	if err != nil {
		return nil, err
	}
	bare := strings.TrimPrefix(text, "/")
	// Exact ID or exact name (case-sensitive first, then case-insensitive).
	for i := range all {
		if all[i].ID == text || all[i].BareName() == bare {
			c := all[i]
			return &c, nil
		}
	}
	for i := range all {
		if strings.EqualFold(all[i].ID, text) || strings.EqualFold(all[i].BareName(), bare) {
			c := all[i]
			return &c, nil
		}
	}
	// Unique hex ID prefix (minimum 2 chars).
	lower := strings.ToLower(text)
	if len(lower) >= 2 && isHexPrefix(lower) {
		var matches []ContainerRecord
		for _, c := range all {
			if strings.HasPrefix(c.ID, lower) {
				matches = append(matches, c)
			}
		}
		if len(matches) == 1 {
			c := matches[0]
			return &c, nil
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("multiple containers match ID prefix %q", text)
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrContainerNotFound, text)
}

func isHexPrefix(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (s *Store) RemoveContainer(id string) error {
	op, err := s.LockContainer(id)
	if err != nil {
		return err
	}
	defer op.Release()
	return op.Remove()
}

func (s *Store) removeContainer(id string) error {
	lk, err := s.containersLock(true)
	if err != nil {
		return err
	}
	defer lk.Release()

	rec, err := s.readRawContainer(id)
	if err != nil {
		return err
	}
	rec, err = s.reconcileContainerNoLock(rec)
	if err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	if rec.State.Running || s.IsContainerRunning(rec.ID) {
		return fmt.Errorf("%w: container %s (%s); stop it first or pass -f", ErrContainerRunning, rec.ShortID(), rec.BareName())
	}
	return s.deleteContainerDirNoLock(rec.ID)
}

func (s *Store) ContainerUsingImage(manifest string) (*ContainerRecord, error) {
	all, err := s.ListContainers(true)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Image == manifest {
			c := all[i]
			return &c, nil
		}
	}
	return nil, nil
}

func containerDirSize(root string) uint64 {
	return mounts.TreeSize(root)
}

func (s *Store) PruneContainers(dryRun bool) ([]ContainerRecord, uint64, int, error) {
	lk, err := s.containersLock(true)
	if err != nil {
		return nil, 0, 0, err
	}
	defer lk.Release()

	var all []ContainerRecord
	if dryRun {
		all, err = s.rawContainersNoLock()
	} else {
		all, err = s.listContainersNoLock(true)
	}
	if err != nil {
		return nil, 0, 0, err
	}
	var removed []ContainerRecord
	var freed uint64
	var running int
	now := time.Now().UTC()
	for _, c := range all {
		// Never wait on an operation while holding the index: the command may
		// itself need the index to finish a launch or a shutdown.
		if IsLocked(s.ContainerOperationLockPath(c.ID)) {
			running++
			continue
		}
		var operation *Lock
		if !dryRun {
			operation, err = TryLock(s.ContainerOperationLockPath(c.ID), true)
			if err != nil {
				return removed, freed, running, err
			}
			if operation == nil {
				running++
				continue
			}
		}
		func() {
			if operation != nil {
				defer operation.Release()
			}
			if dryRun && !s.IsContainerRunning(c.ID) && (c.State.Running || c.State.Status == "created" && c.State.Launched) {
				c.State.Running = false
				c.State.Status = "exited"
			}
			if c.State.Running || s.IsContainerRunning(c.ID) {
				running++
				return
			}
			// Protect an imminent launch while its supervisor initializes. An
			// intentionally unstarted container has no launch token and is eligible now.
			if c.State.Status == "created" && c.State.RunID != "" && !c.State.Launched && c.State.Error == "" && c.State.FinishedAt == "" {
				if createdAt, err := time.Parse(time.RFC3339Nano, c.Created); err == nil && now.Sub(createdAt) < 10*time.Second {
					running++
					return
				}
			}
			sz := containerDirSize(s.ContainerDir(c.ID))
			if !dryRun {
				if err := s.deleteContainerDirNoLock(c.ID); err != nil {
					return
				}
			}
			removed = append(removed, c)
			freed += sz
		}()
	}
	return removed, freed, running, nil
}
