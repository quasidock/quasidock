package run

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func validateMountSources(ctx *context.Context, specs []mounts.Spec, cwd string) error {
	if err := mounts.ValidateTargets(specs); err != nil {
		return err
	}
	own := grants.NewOwn(ctx.Store.FullRoot())
	if p, e := mounts.CanonicalPath(own.SelfDir); e == nil {
		own.SelfDir = p
	}
	profile := ctx.Env["USERPROFILE"]
	if p, e := mounts.CanonicalPath(profile); e == nil {
		profile = p
	}
	for i, s := range specs {
		if s.Type == "volume" {
			if err := mounts.ValidateName(s.Source); err != nil {
				return err
			}
			continue
		}
		if s.Type != "bind" {
			return fmt.Errorf("unsupported mount type %q", s.Type)
		}
		source := s.Source
		if !filepath.IsAbs(source) {
			source = filepath.Join(cwd, source)
		}
		p, err := mounts.CanonicalDir(source)
		if err != nil {
			return err
		}
		if why := LabelRefusal(p, profile, own); why != "" {
			return fmt.Errorf("cannot bind mount %s: %s", p, why)
		}
		specs[i].Source = p
	}
	return nil
}

func prepareContainerMounts(ctx *context.Context, c *store.ContainerRecord, sandbox oci.Sandbox, host []HostGrant, cwd string) (*grants.MountSession, error) {
	for _, m := range c.Mounts {
		if err := mounts.Detach(c.DataDir, m); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return nil, err
	}
	if len(c.Mounts) > 0 {
		// Label/grant the empty private data tree before attaching external trees,
		// so inherited ACL changes cannot propagate through their junctions.
		switch sandbox {
		case oci.SandboxLow:
			if _, err := acl.LabelLow(c.DataDir); err != nil {
				return nil, err
			}
		case oci.SandboxAppContainer:
			sid, err := appcontainer.AppCapability(c.AppID)
			if err != nil {
				return nil, err
			}
			_, err = acl.Grant(c.DataDir, sid, acl.AccessFull, acl.Revoke{Kind: acl.RevokeNone})
			win32.LocalFree(sid)
			if err != nil {
				return nil, err
			}
		}
	}
	requests := []grants.MountRequest{}
	for _, m := range c.Mounts {
		p, err := mounts.CanonicalDir(m.Source)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(p, m.Source) {
			return nil, fmt.Errorf("mount source changed: %s", m.Source)
		}
		requests = append(requests, grants.MountRequest{Path: p})
	}
	// Ordinary host filesystem grants participate too: an older run or rmi
	// must not take away a label/capability needed by a mounted consumer.
	own := grants.NewOwn(ctx.Store.Root)
	for _, g := range host {
		p := cwd
		if g.Path != nil {
			p = *g.Path
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		ro := g.ReadOnly
		if !ro {
			if sandbox == oci.SandboxLow && LabelRefusal(p, ctx.Env["USERPROFILE"], own) != "" {
				continue
			}
			if sandbox == oci.SandboxAppContainer {
				if _, refused := own.Refusal(p); refused {
					continue
				}
			}
		}
		requests = append(requests, grants.MountRequest{Path: p, ReadOnly: ro})
	}
	s, err := grants.BeginMounts(ctx.Store, c, sandbox, requests)
	if err != nil {
		return s, err
	}
	for _, m := range c.Mounts {
		if err := mounts.Attach(c.DataDir, m); err != nil {
			return s, err
		}
	}
	return s, nil
}
