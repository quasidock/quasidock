package cli

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/services"
	"github.com/quasidock/quasidock/internal/store"
)

const portUsage = "usage: quasidock port CONTAINER [PORT[/tcp]|SERVICE]"

func portCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		_, err := fmt.Fprintln(ctx.Stdout, portUsage)
		return 0, err
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) < 1 || len(args) > 2 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return 1, errors.New(portUsage)
	}
	selector := ""
	if len(args) == 2 {
		var err error
		selector, err = services.ParseSelector(args[1])
		if err != nil {
			return 1, err
		}
	}
	rec, err := ctx.Store.ResolveContainer(args[0])
	if err != nil {
		return 1, err
	}
	ports := map[string][]store.PortBinding{}
	if rec.State.Running {
		if rec.NetworkSettings == nil {
			return 1, errors.New("container's supervisor does not record port mappings; stop and start it with this binary")
		}
		ports = rec.NetworkSettings.Ports
	}
	if selector != "" {
		if n, err := strconv.ParseUint(selector, 10, 16); err == nil {
			selector = services.PortKey(uint16(n))
		} else {
			image, err := ctx.Store.ReadImage(rec.AppID, rec.Image)
			if err != nil {
				return 1, err
			}
			for _, s := range image.Config.Permissions.Services {
				if strings.EqualFold(s.Name, selector) {
					selector = services.PortKey(s.Port)
					break
				}
			}
		}
		if len(ports[selector]) == 0 {
			return 1, fmt.Errorf("no public port %q published for %s", args[1], args[0])
		}
	}
	type row struct {
		key string
		store.PortBinding
	}
	var rows []row
	for key, bindings := range ports {
		if selector == "" || selector == key {
			for _, binding := range bindings {
				rows = append(rows, row{key, binding})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		portA, _ := strconv.Atoi(strings.TrimSuffix(a.key, "/tcp"))
		portB, _ := strconv.Atoi(strings.TrimSuffix(b.key, "/tcp"))
		if portA != portB {
			return portA < portB
		}
		if a.HostIP != b.HostIP {
			return a.HostIP < b.HostIP
		}
		hostA, _ := strconv.Atoi(a.HostPort)
		hostB, _ := strconv.Atoi(b.HostPort)
		return hostA < hostB
	})
	for _, row := range rows {
		line := net.JoinHostPort(row.HostIP, row.HostPort)
		if selector == "" {
			line = row.key + " -> " + line
		}
		if _, err := fmt.Fprintln(ctx.Stdout, line); err != nil {
			return 1, err
		}
	}
	return 0, nil
}
