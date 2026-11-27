package store

import (
	"errors"
	"fmt"
	"os"

	"github.com/quasidock/quasidock/internal/win32"
)

// TrackNetwork reserves ownership by GUID before a network is created in HCN.
func (s *Store) TrackNetwork(id string) error {
	id, err := win32.CanonicalHCNNetworkID(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Path("networks"), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path("networks", id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func (s *Store) TrackedNetworkIDs() ([]string, error) {
	entries, err := os.ReadDir(s.Path("networks"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if id, err := win32.CanonicalHCNNetworkID(entry.Name()); err == nil && id == entry.Name() && entry.Type().IsRegular() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (s *Store) ForgetNetwork(id string) error {
	id, err := win32.CanonicalHCNNetworkID(id)
	if err != nil {
		return err
	}
	err = os.Remove(s.Path("networks", id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// WithContainerNetworkUsers holds the container index lock through the caller's
// network removal check and action. Each GUID maps to one saved container user.
func (s *Store) WithContainerNetworkUsers(excluded map[string]bool, use func(map[string]*ContainerRecord)) error {
	lock, err := s.containersLock(false)
	if err != nil {
		return err
	}
	defer lock.Release()
	all, err := s.rawContainersNoLock()
	if err != nil {
		return err
	}
	users := make(map[string]*ContainerRecord)
	for i := range all {
		if excluded[all[i].ID] {
			continue
		}
		for id := range all[i].HostConfig.Networks {
			guid, err := win32.CanonicalHCNNetworkID(id)
			if err != nil {
				return fmt.Errorf("reading network membership of container %s: %w", all[i].ShortID(), err)
			}
			users[guid] = &all[i]
		}
	}
	use(users)
	return nil
}
