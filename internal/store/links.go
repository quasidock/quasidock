package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/quasidock/quasidock/internal/oci"
)

type LinkRequest struct {
	Target string
	Alias  string
}

type ContainerLink struct {
	TargetID string
	Alias    string
	TCPPorts []uint16
}

// LinkEnvPrefix is shared by alias validation and environment generation.
func LinkEnvPrefix(alias string) string {
	return strings.Map(func(r rune) rune {
		if r == '.' || r == '-' {
			return '_'
		}
		return r
	}, strings.ToUpper(alias))
}

func (link ContainerLink) EnvKeys() []string {
	prefix := LinkEnvPrefix(link.Alias)
	keys := []string{prefix + "_NAME", prefix + "_PORT"}
	for _, port := range link.TCPPorts {
		key := LinkPortEnvKey(link.Alias, port)
		keys = append(keys, key, key+"_ADDR", key+"_PORT", key+"_PROTO")
	}
	return keys
}

func LinkPortEnvKey(alias string, port uint16) string {
	return fmt.Sprintf("%s_PORT_%d_TCP", LinkEnvPrefix(alias), port)
}

func validateLinkAlias(alias string) error {
	if strings.HasPrefix(alias, "/") {
		return fmt.Errorf("invalid link alias %q", alias)
	}
	if _, err := ValidateContainerName(alias); err != nil {
		return fmt.Errorf("invalid link alias %q: %w", alias, err)
	}
	return nil
}

func ParseLinkRequest(value string) (LinkRequest, error) {
	target, alias, explicit := strings.Cut(value, ":")
	if strings.TrimPrefix(target, "/") == "" || strings.Contains(strings.TrimPrefix(target, "/"), "/") {
		return LinkRequest{}, fmt.Errorf("invalid --link %q: expected TARGET[:ALIAS]", value)
	}
	if explicit {
		if err := validateLinkAlias(alias); err != nil {
			return LinkRequest{}, err
		}
	}
	return LinkRequest{Target: target, Alias: alias}, nil
}

func ParseLinkPath(value string) (source, alias string, err error) {
	source, alias, found := strings.Cut(strings.TrimPrefix(value, "/"), "/")
	if !found || source == "" {
		return "", "", fmt.Errorf("invalid link %q: expected SOURCE/ALIAS", value)
	}
	if err := validateLinkAlias(alias); err != nil {
		return "", "", err
	}
	return source, alias, nil
}

func (s *Store) resolveLinks(requests []LinkRequest, existing []ContainerRecord) ([]ContainerLink, error) {
	links := make([]ContainerLink, 0, len(requests))
	envKeys := make(map[string]string)
	for _, request := range requests {
		target, err := resolveContainer(request.Target, existing)
		if err != nil {
			return nil, fmt.Errorf("--link %s: %w", request.Target, err)
		}
		if len(target.HostConfig.Networks) != 0 || target.State.NetworkManaged {
			return nil, fmt.Errorf("--link %s requires a target on the default network", request.Target)
		}
		alias := request.Alias
		if alias == "" {
			alias = target.BareName()
		}
		if err := validateLinkAlias(alias); err != nil {
			return nil, err
		}
		img, err := s.ReadImage(target.AppID, target.Image)
		if err != nil {
			return nil, fmt.Errorf("reading linked target %s: %w", target.BareName(), err)
		}
		var ports []uint16
		for _, service := range img.Config.Permissions.Services {
			ports = append(ports, service.Port)
		}
		if len(ports) == 0 {
			return nil, fmt.Errorf("linked target %s has no declared TCP services", target.BareName())
		}
		slices.Sort(ports)
		link := ContainerLink{TargetID: target.ID, Alias: alias, TCPPorts: slices.Compact(ports)}
		for _, key := range link.EnvKeys() {
			if prior, exists := envKeys[key]; exists {
				return nil, fmt.Errorf("duplicate link alias environment key %s from aliases %q and %q", key, prior, alias)
			}
			envKeys[key] = alias
		}
		links = append(links, link)
	}
	return links, nil
}

func (s *Store) checkNetworkLinkUsersNoLock(rec *ContainerRecord) error {
	if len(rec.HostConfig.Links) != 0 {
		return fmt.Errorf("container %s has saved links; remove them with rm --link before connecting a private network", rec.BareName())
	}
	all, err := s.rawContainersNoLock()
	if err != nil {
		return err
	}
	for _, source := range all {
		for _, link := range source.HostConfig.Links {
			if link.TargetID == rec.ID {
				return fmt.Errorf("container %s is linked as %s/%s; remove the link before connecting a private network", rec.BareName(), source.BareName(), link.Alias)
			}
		}
	}
	return nil
}

func FindLink(links []ContainerLink, alias string) int {
	return slices.IndexFunc(links, func(link ContainerLink) bool {
		return strings.EqualFold(link.Alias, alias)
	})
}

// LiveLink validates a requested route against both current run records under
// the same lock. The read does not reconcile or delete either container.
func (s *Store) LiveLink(sourceID, sourceRun, alias string, port uint16) (source, target *ContainerRecord, err error) {
	if !oci.IsSha256Hex(sourceID) {
		return nil, nil, errors.New("invalid link source ID")
	}
	lk, err := s.containersLock(false)
	if err != nil {
		return nil, nil, err
	}
	defer lk.Release()
	source, err = s.readRawContainer(sourceID)
	if err != nil {
		return nil, nil, err
	}
	if source.State.RunID != sourceRun || !source.State.Running || source.State.LinkSupervisorPID == 0 || !s.IsContainerRunning(sourceID) {
		return nil, nil, errors.New("link source run is unavailable")
	}
	i := FindLink(source.HostConfig.Links, alias)
	if i < 0 || !slices.Contains(source.HostConfig.Links[i].TCPPorts, port) {
		return nil, nil, fmt.Errorf("link %q does not permit TCP port %d", alias, port)
	}
	targetID := source.HostConfig.Links[i].TargetID
	if !oci.IsSha256Hex(targetID) {
		return nil, nil, errors.New("invalid saved link target ID")
	}
	target, err = s.readRawContainer(targetID)
	if err != nil {
		return nil, nil, err
	}
	if !target.State.Running || target.State.Paused || !s.IsContainerRunning(targetID) {
		return nil, nil, errors.New("linked target is not running or is paused")
	}
	if target.State.LinkSupervisorPID == 0 {
		if target.HostConfig.PidsLimit == 1 {
			return nil, nil, errors.New("linked target requires room for a relay process; increase --pids-limit and restart it")
		}
		return nil, nil, errors.New("linked target has no link broker; restart it with this binary on the default network")
	}
	return source, target, nil
}
