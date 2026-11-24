package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/store"
)

const psUsage = "usage: quasidock ps [-a|--all] [-q|--quiet] [-f|--filter KEY=VALUE] [-n|--last N] [-s|--size] [--no-trunc]"

type containerListOptions struct {
	all, quiet, size, noTrunc bool
	last                      int
	filters                   containerListFilters
}

type containerListFilters struct {
	ids           []string
	names         []*regexp.Regexp
	statuses      map[string]bool
	exitCodes     map[int]bool
	before, since []string
}

func (f *containerListFilters) add(raw string) error {
	key, value, ok := strings.Cut(raw, "=")
	if !ok || key == "" || value == "" {
		return fmt.Errorf("invalid ps filter %q: expected KEY=VALUE with a nonempty value", raw)
	}
	switch key {
	case "id":
		f.ids = append(f.ids, strings.ToLower(value))
	case "name":
		pattern, err := regexp.Compile(value)
		if err != nil {
			return fmt.Errorf("invalid ps name filter %q: %w", value, err)
		}
		f.names = append(f.names, pattern)
	case "status":
		switch value {
		case "created", "running", "paused", "exited":
		default:
			return fmt.Errorf("invalid ps status %q: use created, running, paused, or exited", value)
		}
		if f.statuses == nil {
			f.statuses = make(map[string]bool)
		}
		f.statuses[value] = true
	case "exited":
		code, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid ps exited filter %q: expected an integer exit code", value)
		}
		if f.exitCodes == nil {
			f.exitCodes = make(map[int]bool)
		}
		f.exitCodes[code] = true
	case "before":
		f.before = append(f.before, value)
	case "since":
		f.since = append(f.since, value)
	default:
		return fmt.Errorf("unsupported ps filter %q: use id, name, status, exited, before, or since", key)
	}
	return nil
}

func parseContainerListOptions(args []string) (containerListOptions, error) {
	opts := containerListOptions{last: -1}
	boolOption := func(name string) *bool {
		switch name {
		case "all", "a":
			return &opts.all
		case "quiet", "q":
			return &opts.quiet
		case "size", "s":
			return &opts.size
		case "no-trunc":
			return &opts.noTrunc
		}
		return nil
	}
	setBool := func(dest *bool, raw string) error {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid ps boolean %q: expected true or false", raw)
		}
		*dest = value
		return nil
	}
	setValue := func(name, raw string) error {
		if name == "filter" || name == "f" {
			return opts.filters.add(raw)
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("invalid ps --last %q: expected an integer", raw)
		}
		opts.last = n
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i != len(args)-1 {
				return opts, errors.New(psUsage)
			}
			break
		}
		if strings.HasPrefix(arg, "--") {
			name, raw, hasValue := strings.Cut(arg[2:], "=")
			switch name {
			case "all", "quiet", "size", "no-trunc", "filter", "last":
			default:
				return opts, fmt.Errorf("unknown ps option %q", arg)
			}
			if dest := boolOption(name); dest != nil {
				if !hasValue {
					raw = "true"
				}
				if err := setBool(dest, raw); err != nil {
					return opts, err
				}
				continue
			}
			if !hasValue {
				i++
				if i == len(args) {
					return opts, fmt.Errorf("ps --%s needs a value", name)
				}
				raw = args[i]
			}
			if err := setValue(name, raw); err != nil {
				return opts, err
			}
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return opts, errors.New(psUsage)
		}
		for short := arg[1:]; short != ""; {
			name := short[:1]
			short = short[1:]
			if dest := boolOption(name); dest != nil {
				*dest = true
				if strings.HasPrefix(short, "=") {
					if err := setBool(dest, short[1:]); err != nil {
						return opts, err
					}
					short = ""
				}
				continue
			}
			if name != "f" && name != "n" {
				return opts, fmt.Errorf("unknown ps option %q", "-"+name)
			}
			raw := strings.TrimPrefix(short, "=")
			if short == "" {
				i++
				if i == len(args) {
					return opts, fmt.Errorf("ps -%s needs a value", name)
				}
				raw = args[i]
			}
			if err := setValue(name, raw); err != nil {
				return opts, err
			}
			short = ""
		}
	}
	return opts, nil
}

func containerFilterStatus(c store.ContainerRecord) string {
	if c.State.Paused {
		return "paused"
	}
	if c.State.Running {
		return "running"
	}
	if c.State.Status == "created" {
		return "created"
	}
	return "exited"
}

func (f *containerListFilters) matches(c store.ContainerRecord) bool {
	if len(f.ids) > 0 {
		matched := false
		for _, id := range f.ids {
			matched = matched || strings.HasPrefix(strings.ToLower(c.ID), id)
		}
		if !matched {
			return false
		}
	}
	if len(f.names) > 0 {
		matched := false
		for _, pattern := range f.names {
			matched = matched || pattern.MatchString(c.Name) || pattern.MatchString(c.BareName())
		}
		if !matched {
			return false
		}
	}
	status := containerFilterStatus(c)
	if len(f.statuses) > 0 && !f.statuses[status] {
		return false
	}
	if len(f.exitCodes) > 0 && (status != "exited" || c.State.StartedAt == "" || !f.exitCodes[c.State.ExitCode]) {
		return false
	}
	return true
}

func selectContainers(st *store.Store, containers []store.ContainerRecord, opts containerListOptions) ([]store.ContainerRecord, error) {
	position := func(target string) (int, error) {
		rec, err := st.ResolveContainer(target)
		if err != nil {
			return 0, err
		}
		for i, c := range containers {
			if c.ID == rec.ID {
				return i, nil
			}
		}
		return 0, fmt.Errorf("%w: %s", store.ErrContainerNotFound, target)
	}
	before, since := -1, len(containers)
	for _, target := range opts.filters.before {
		i, err := position(target)
		if err != nil {
			return nil, fmt.Errorf("ps before=%s: %w", target, err)
		}
		if before == -1 || i < before {
			before = i
		}
	}
	for n, target := range opts.filters.since {
		i, err := position(target)
		if err != nil {
			return nil, fmt.Errorf("ps since=%s: %w", target, err)
		}
		if n == 0 || i > since {
			since = i
		}
	}
	all := opts.all || opts.last > 0 || len(opts.filters.statuses) > 0
	var selected []store.ContainerRecord
	for i, c := range containers {
		if i <= before || i >= since || (!all && !c.State.Running) || !opts.filters.matches(c) {
			continue
		}
		selected = append(selected, c)
		if opts.last > 0 && len(selected) == opts.last {
			break
		}
	}
	return selected, nil
}

func containerList(ctx *context.Context, args []string) error {
	opts, err := parseContainerListOptions(args)
	if err != nil {
		return err
	}
	containers, err := ctx.Store.ListContainers(true)
	if err != nil {
		return err
	}
	containers, err = selectContainers(ctx.Store, containers, opts)
	if err != nil {
		return err
	}
	id := func(c store.ContainerRecord) string {
		if opts.noTrunc {
			return c.ID
		}
		return c.ShortID()
	}
	if opts.quiet {
		for _, c := range containers {
			if _, err := fmt.Fprintln(ctx.Stdout, id(c)); err != nil {
				return err
			}
		}
		return nil
	}
	sizes := make([]uint64, len(containers))
	if opts.size {
		for i, c := range containers {
			sizes[i], err = ctx.Store.ContainerSize(c.ID)
			if err != nil {
				return err
			}
		}
	}
	tw := tabwriter.NewWriter(ctx.Stdout, 0, 0, 3, ' ', 0)
	header := "CONTAINER ID\tIMAGE\tCOMMAND\tCREATED\tSTATUS\tNAMES"
	if opts.size {
		header += "\tSIZE"
	}
	if _, err := fmt.Fprintln(tw, header); err != nil {
		return err
	}
	now := time.Now()
	for i, c := range containers {
		img := c.ImageRef
		if img == "" {
			img = c.AppID
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s", id(c), img,
			formatContainerCommand(c, opts.noTrunc), formatRelativeTime(c.Created, now),
			formatContainerStatus(c, now), c.BareName()); err != nil {
			return err
		}
		if opts.size {
			if _, err := fmt.Fprintf(tw, "\t%s", lifecycle.FormatBytes(sizes[i])); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(tw); err != nil {
			return err
		}
	}
	return tw.Flush()
}
