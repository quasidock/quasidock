package cli

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/hkcu"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/store"
)

func containerCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 0 {
		return 1, errors.New("usage: quasidock container <create|commit|export|ls|stats|top|port|inspect|diff|cp|exec|attach|wait|start|restart|rename|update|pause|unpause|kill|stop|rm|logs|prune>")
	}
	cmd, rest := args[0], args[1:]
	if cmd == "--help" || cmd == "help" {
		fmt.Fprint(ctx.Stdout, usage)
		return 0, nil
	}
	switch cmd {
	case "port":
		return portCommand(ctx, rest)
	case "rename":
		return renameCommand(ctx, rest)
	case "update":
		return updateCommand(ctx, rest)
	case "create":
		return createCommand(ctx, rest)
	case "commit":
		return commitCommand(ctx, rest)
	case "export":
		return exportCommand(ctx, rest)
	case "exec":
		return execCommand(ctx, rest)
	case "attach":
		return attachCommand(ctx, rest)
	case "wait":
		return waitCommand(ctx, rest)
	case "start":
		return startCommand(ctx, rest)
	case "ls", "list":
		return 0, containerList(ctx, rest)
	case "stats":
		return statsCommand(ctx, rest)
	case "top":
		return topCommand(ctx, rest)
	case "inspect":
		return 0, containerInspect(ctx, rest)
	case "diff":
		return diffCommand(ctx, rest)
	case "cp":
		return cpCommand(ctx, rest)
	case "restart", "pause", "unpause", "kill", "stop":
		return containerLifecycle(ctx, cmd, rest)
	case "rm", "remove":
		return 0, containerRemove(ctx, rest)
	case "logs":
		return logsCommand(ctx, rest)
	case "prune":
		return 0, containerPrune(ctx, rest)
	default:
		return 1, fmt.Errorf("unknown container command %q", cmd)
	}
}

func createCommand(ctx *context.Context, args []string) (uint32, error) {
	opts, err := parseCreateOptions(ctx, args)
	if err != nil {
		return 1, err
	}
	opts.Image, err = selectContainerImage(ctx, opts)
	if err != nil {
		return 1, err
	}
	rec, err := run.Create(ctx, opts)
	if err != nil {
		return 1, err
	}
	_, err = fmt.Fprintln(ctx.Stdout, rec.ID)
	return 0, err
}

func parseInspectFlags(args []string, usageText string) (string, []string, error) {
	format := ""
	var targets []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-f" || arg == "--format" {
			i++
			if i == len(args) {
				return "", nil, errors.New("inspect --format needs a template")
			}
			format = args[i]
		} else if strings.HasPrefix(arg, "--format=") {
			format = strings.TrimPrefix(arg, "--format=")
		} else if strings.HasPrefix(arg, "-") {
			return "", nil, fmt.Errorf("unknown inspect option %q", arg)
		} else {
			targets = append(targets, arg)
		}
	}
	if len(targets) == 0 {
		return "", nil, errors.New(usageText)
	}
	return format, targets, nil
}

func renderInspect(out io.Writer, format string, items any) error {
	if format != "" {
		tmpl, err := template.New("inspect").Funcs(template.FuncMap{
			"json": func(v any) (string, error) {
				b, e := json.Marshal(v)
				return string(b), e
			},
		}).Parse(format)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		switch slice := items.(type) {
		case []*store.ContainerRecord:
			for _, item := range slice {
				if err := tmpl.Execute(&buf, item); err != nil {
					return err
				}
				buf.WriteByte('\n')
			}
		case []any:
			for _, item := range slice {
				if err := tmpl.Execute(&buf, item); err != nil {
					return err
				}
				buf.WriteByte('\n')
			}
		}
		_, err = out.Write(buf.Bytes())
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(items)
}

func containerInspect(ctx *context.Context, args []string) error {
	format, names, err := parseInspectFlags(args, "usage: quasidock container inspect [-f TEMPLATE] CONTAINER [CONTAINER...]")
	if err != nil {
		return err
	}
	recs := make([]*store.ContainerRecord, 0, len(names))
	for _, name := range names {
		rec, err := ctx.Store.ResolveContainer(name)
		if err != nil {
			return err
		}
		recs = append(recs, rec)
	}
	return renderInspect(ctx.Stdout, format, recs)
}

func parseStopTimeout(option, raw string) (*int, error) {
	seconds, err := strconv.Atoi(raw)
	if err != nil || run.ValidateStopTimeout(seconds) != nil {
		return nil, fmt.Errorf("invalid %s %q: expected -1 or non-negative whole seconds within the supported duration", option, raw)
	}
	return &seconds, nil
}

func parseLifecycleArgs(command string, args []string) (*int, run.KillSignal, []string, error) {
	var timeout *int
	killSignal := run.SignalKILL
	var targets []string
	parseTime := func(raw string) error {
		var err error
		timeout, err = parseStopTimeout("--time", raw)
		return err
	}
	parseSignal := func(raw string) error {
		var err error
		killSignal, err = run.ParseKillSignal(raw)
		return err
	}
	var parse func(string) error
	var option string
	switch command {
	case "stop", "restart":
		parse, option = parseTime, "--time"
	case "kill":
		parse, option = parseSignal, "--signal"
	}
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		switch {
		case arg == "--":
			targets = append(targets, args...)
			args = nil
		case (command == "stop" || command == "restart") && (arg == "-t" || arg == "--time"),
			command == "kill" && (arg == "-s" || arg == "--signal"):
			if len(args) == 0 {
				return nil, "", nil, fmt.Errorf("%s %s needs a value", command, option)
			}
			if err := parse(args[0]); err != nil {
				return nil, "", nil, err
			}
			args = args[1:]
		case (command == "stop" || command == "restart") && (strings.HasPrefix(arg, "--time=") || strings.HasPrefix(arg, "-t=")),
			command == "kill" && (strings.HasPrefix(arg, "--signal=") || strings.HasPrefix(arg, "-s=")):
			if err := parse(arg[strings.IndexByte(arg, '=')+1:]); err != nil {
				return nil, "", nil, err
			}
		case strings.HasPrefix(arg, "-"):
			return nil, "", nil, fmt.Errorf("unknown %s option %q", command, arg)
		default:
			targets = append(targets, arg)
		}
	}
	if len(targets) == 0 {
		return nil, "", nil, fmt.Errorf("usage: quasidock %s [OPTIONS] CONTAINER [CONTAINER...]", command)
	}
	return timeout, killSignal, targets, nil
}

func containerLifecycle(ctx *context.Context, command string, args []string) (uint32, error) {
	timeout, killSignal, targets, err := parseLifecycleArgs(command, args)
	if err != nil {
		return 1, err
	}
	cancel := stdcontext.Background()
	if command == "stop" || command == "restart" {
		var stop stdcontext.CancelFunc
		cancel, stop = signal.NotifyContext(cancel, os.Interrupt)
		defer stop()
	}
	var errs []error
	for _, target := range targets {
		if cancel.Err() != nil {
			return 130, nil
		}
		var err error
		switch command {
		case "start":
			_, err = run.StartContainer(ctx, target)
		case "restart":
			_, err = run.RestartContainer(cancel, ctx, target, timeout)
		case "pause":
			_, err = run.PauseContainer(ctx, target)
		case "unpause":
			_, err = run.UnpauseContainer(ctx, target)
		case "kill":
			_, err = run.KillContainer(ctx, target, killSignal)
		case "stop":
			_, err = run.StopContainer(cancel, ctx, target, timeout)
		}
		if cancel.Err() != nil {
			return 130, nil
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", command, target, err))
			continue
		}
		fmt.Fprintln(ctx.Stdout, target)
	}
	if err := errors.Join(errs...); err != nil {
		return 1, err
	}
	return 0, nil
}

func containerRemove(ctx *context.Context, args []string) error {
	const usage = "usage: quasidock rm [-f|--force] [-v|--volumes] CONTAINER [CONTAINER...]\n" +
		"       quasidock rm -l|--link SOURCE/ALIAS [SOURCE/ALIAS...]"
	var force, volumes, link bool
	var targets []string
	for i, arg := range args {
		switch arg {
		case "-f", "--force":
			force = true
		case "-v", "--volumes":
			volumes = true
		case "-fv", "-vf":
			force, volumes = true, true
		case "-l", "--link":
			link = true
		case "--help":
			_, err := fmt.Fprintln(ctx.Stdout, usage)
			return err
		case "--":
			targets = append(targets, args[i+1:]...)
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown rm option %q", arg)
			}
			targets = append(targets, arg)
		}
		if arg == "--" {
			break
		}
	}
	if link && (force || volumes) {
		return errors.New("rm --link cannot be combined with --force or --volumes")
	}
	if len(targets) == 0 {
		return errors.New(usage)
	}
	var errs []error
	for _, target := range targets {
		var err error
		if link {
			err = run.RemoveContainerLink(ctx, target)
		} else {
			_, err = run.RemoveContainer(ctx, target, force, volumes)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("rm %s: %w", target, err))
			continue
		}
		fmt.Fprintln(ctx.Stdout, target)
	}
	return errors.Join(errs...)
}

func containerPrune(ctx *context.Context, args []string) error {
	for _, arg := range args {
		if arg == "-f" || arg == "--force" {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unknown prune option %q", arg)
		}
		return errors.New("usage: quasidock container prune [-f]")
	}
	removed, freed, _, err := ctx.Store.PruneContainers(false)
	if err == nil {
		err = grants.RepairMounts(ctx.Store)
	}
	if err != nil {
		return err
	}
	if len(removed) > 0 {
		_ = hkcu.SettleLeftovers(ctx.Store, "")
		fmt.Fprintln(ctx.Stdout, "Deleted Containers:")
		for _, c := range removed {
			fmt.Fprintln(ctx.Stdout, c.ID)
		}
		fmt.Fprintln(ctx.Stdout)
	}
	fmt.Fprintf(ctx.Stdout, "Total reclaimed space: %s\n", lifecycle.FormatBytes(freed))
	return nil
}

func formatContainerCommand(c store.ContainerRecord, noTrunc bool) string {
	var parts []string
	if c.Path != "" {
		path := c.Path
		if !noTrunc {
			path = filepath.Base(path)
		}
		parts = append(parts, path)
		parts = append(parts, c.Args...)
	} else {
		parts = append(parts, c.Config.Entrypoint...)
		if len(c.Args) > 0 {
			parts = append(parts, c.Args...)
		} else {
			parts = append(parts, c.Config.Cmd...)
		}
	}
	text := strings.Join(parts, " ")
	if !noTrunc {
		runes := []rune(text)
		if len(runes) > 28 {
			text = string(runes[:25]) + "..."
		}
	}
	return `"` + text + `"`
}

func formatRelativeTime(ts string, now time.Time) string {
	t, err := parseTimestamp(ts)
	if err != nil {
		return ts
	}
	return humanDuration(now.Sub(t)) + " ago"
}

func formatContainerStatus(c store.ContainerRecord, now time.Time) string {
	if c.State.Restarting {
		return fmt.Sprintf("Restarting (%d)", c.State.ExitCode)
	}
	if c.State.Running {
		status := "Up"
		if t, err := parseTimestamp(c.State.StartedAt); err == nil {
			status += " " + humanDuration(now.Sub(t))
		}
		if c.State.Paused {
			status += " (Paused)"
		}
		return status
	}
	if c.State.Status == "created" {
		return "Created"
	}
	if t, err := parseTimestamp(c.State.FinishedAt); err == nil {
		return fmt.Sprintf("Exited (%d) %s ago", c.State.ExitCode, humanDuration(now.Sub(t)))
	}
	return fmt.Sprintf("Exited (%d)", c.State.ExitCode)
}

func parseTimestamp(ts string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, ts)
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < 2*time.Second:
		return "Less than a second"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < 2*time.Minute:
		return "About a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 2*time.Hour:
		return "About an hour"
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
