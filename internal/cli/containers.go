package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
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
		return 1, errors.New("usage: quasidock container <create|ls|inspect|exec|attach|start|restart|pause|unpause|kill|stop|rm|logs|prune>")
	}
	cmd, rest := args[0], args[1:]
	if cmd == "--help" || cmd == "help" {
		fmt.Fprint(ctx.Stdout, usage)
		return 0, nil
	}
	switch cmd {
	case "create":
		return createCommand(ctx, rest)
	case "exec":
		return execCommand(ctx, rest)
	case "attach":
		return attachCommand(ctx, rest)
	case "start":
		return startCommand(ctx, rest)
	case "ls", "list":
		return 0, containerList(ctx, rest)
	case "inspect":
		return 0, containerInspect(ctx, rest)
	case "restart", "pause", "unpause", "kill", "stop":
		return 0, containerLifecycle(ctx, cmd, rest)
	case "rm", "remove":
		return 0, containerRemove(ctx, rest)
	case "logs":
		return 0, containerLogs(ctx, rest)
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

func containerList(ctx *context.Context, args []string) error {
	all := false
	quiet := false
	for _, arg := range args {
		switch arg {
		case "-a", "--all":
			all = true
		case "-q", "--quiet":
			quiet = true
		case "-aq", "-qa":
			all = true
			quiet = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown ps option %q", arg)
			}
			return errors.New("usage: quasidock ps [-a|--all] [-q|--quiet]")
		}
	}
	containers, err := ctx.Store.ListContainers(all)
	if err != nil {
		return err
	}
	if quiet {
		for _, c := range containers {
			fmt.Fprintln(ctx.Stdout, c.ShortID())
		}
		return nil
	}
	tw := tabwriter.NewWriter(ctx.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "CONTAINER ID\tIMAGE\tCOMMAND\tCREATED\tSTATUS\tNAMES")
	now := time.Now()
	for _, c := range containers {
		img := c.ImageRef
		if img == "" {
			img = c.AppID
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			c.ShortID(),
			img,
			formatContainerCommand(c),
			formatRelativeTime(c.Created, now),
			formatContainerStatus(c, now),
			c.BareName(),
		)
	}
	return tw.Flush()
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

func parseLifecycleArgs(command string, args []string) (int, run.KillSignal, []string, error) {
	timeout := 10
	signal := run.SignalKILL
	var targets []string
	parseTime := func(raw string) error {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 || uint64(v) > uint64((1<<63-1)/int64(time.Second)) {
			return fmt.Errorf("invalid --time %q: expected non-negative seconds within the supported duration", raw)
		}
		timeout = v
		return nil
	}
	parseSignal := func(raw string) error {
		switch strings.ToUpper(raw) {
		case "KILL", "SIGKILL", "9":
			signal = run.SignalKILL
		case "TERM", "SIGTERM", "15":
			signal = run.SignalTERM
		default:
			return fmt.Errorf("unsupported signal %q: use KILL or TERM", raw)
		}
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var parse func(string) error
		var option string
		if command == "stop" || command == "restart" {
			parse, option = parseTime, "--time"
		} else if command == "kill" {
			parse, option = parseSignal, "--signal"
		}
		switch {
		case arg == "--":
			targets = append(targets, args[i+1:]...)
			i = len(args)
		case (command == "stop" || command == "restart") && (arg == "-t" || arg == "--time"),
			command == "kill" && (arg == "-s" || arg == "--signal"):
			i++
			if i == len(args) {
				return 0, "", nil, fmt.Errorf("%s %s needs a value", command, option)
			}
			if err := parse(args[i]); err != nil {
				return 0, "", nil, err
			}
		case (command == "stop" || command == "restart") && (strings.HasPrefix(arg, "--time=") || strings.HasPrefix(arg, "-t=")),
			command == "kill" && (strings.HasPrefix(arg, "--signal=") || strings.HasPrefix(arg, "-s=")):
			if err := parse(arg[strings.IndexByte(arg, '=')+1:]); err != nil {
				return 0, "", nil, err
			}
		case strings.HasPrefix(arg, "-"):
			return 0, "", nil, fmt.Errorf("unknown %s option %q", command, arg)
		default:
			targets = append(targets, arg)
		}
	}
	if len(targets) == 0 {
		return 0, "", nil, fmt.Errorf("usage: quasidock %s [OPTIONS] CONTAINER [CONTAINER...]", command)
	}
	return timeout, signal, targets, nil
}

func containerLifecycle(ctx *context.Context, command string, args []string) error {
	timeout, signal, targets, err := parseLifecycleArgs(command, args)
	if err != nil {
		return err
	}
	var errs []error
	for _, target := range targets {
		var err error
		switch command {
		case "start":
			_, err = run.StartContainer(ctx, target)
		case "restart":
			_, err = run.RestartContainer(ctx, target, timeout)
		case "pause":
			_, err = run.PauseContainer(ctx, target)
		case "unpause":
			_, err = run.UnpauseContainer(ctx, target)
		case "kill":
			_, err = run.KillContainer(ctx, target, signal)
		case "stop":
			_, err = run.StopContainer(ctx, target, timeout)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", command, target, err))
			continue
		}
		fmt.Fprintln(ctx.Stdout, target)
	}
	return errors.Join(errs...)
}

func containerRemove(ctx *context.Context, args []string) error {
	force := false
	var targets []string
	for _, arg := range args {
		if arg == "-f" || arg == "--force" {
			force = true
		} else if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unknown rm option %q", arg)
		} else {
			targets = append(targets, arg)
		}
	}
	if len(targets) == 0 {
		return errors.New("usage: quasidock rm [-f] CONTAINER [CONTAINER...]")
	}
	var errs []error
	for _, target := range targets {
		if _, err := run.RemoveContainer(ctx, target, force); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintln(ctx.Stdout, target)
	}
	return errors.Join(errs...)
}

func containerLogs(ctx *context.Context, args []string) error {
	follow := false
	tail := -1
	var targets []string
	parseTail := func(raw string) error {
		if raw == "all" {
			tail = -1
			return nil
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid --tail %q: expected non-negative integer or \"all\"", raw)
		}
		tail = n
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-f" || arg == "--follow":
			follow = true
		case arg == "-n" || arg == "--tail":
			i++
			if i == len(args) {
				return errors.New("logs --tail needs a number of lines")
			}
			if err := parseTail(args[i]); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "--tail="):
			if err := parseTail(strings.TrimPrefix(arg, "--tail=")); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-n="):
			if err := parseTail(strings.TrimPrefix(arg, "-n=")); err != nil {
				return err
			}
		case len(arg) > 2 && strings.HasPrefix(arg, "-n"):
			if err := parseTail(arg[2:]); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("unknown logs option %q", arg)
		default:
			targets = append(targets, arg)
		}
	}
	if len(targets) != 1 {
		return errors.New("usage: quasidock logs [-f|--follow] [-n|--tail LINES] CONTAINER")
	}

	rec, err := ctx.Store.ResolveContainer(targets[0])
	if err != nil {
		return err
	}

	var sentOffset int64
	if data, err := os.ReadFile(rec.LogPath); err == nil {
		sentOffset = int64(len(data))
		chunk := tailLines(data, tail)
		if len(chunk) > 0 {
			if _, werr := ctx.Stdout.Write(chunk); werr != nil {
				return werr
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if !follow {
		return nil
	}

	for {
		running := ctx.Store.IsContainerRunning(rec.ID)
		if f, err := os.Open(rec.LogPath); err == nil {
			if _, serr := f.Seek(sentOffset, io.SeekStart); serr == nil {
				n, _ := io.Copy(ctx.Stdout, f)
				sentOffset += n
			}
			_ = f.Close()
		}
		if !running {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
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

func tailLines(data []byte, n int) []byte {
	if n < 0 {
		return data
	}
	if n == 0 || len(data) == 0 {
		return nil
	}
	end := len(data)
	if data[end-1] == '\n' {
		end--
	}
	lines := 0
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			lines++
			if lines == n {
				return data[i+1:]
			}
		}
	}
	return data
}

func formatContainerCommand(c store.ContainerRecord) string {
	var parts []string
	if c.Path != "" {
		parts = append(parts, filepath.Base(c.Path))
	}
	parts = append(parts, c.Args...)
	if len(parts) == 0 && len(c.Config.Entrypoint) > 0 {
		parts = append(parts, c.Config.Entrypoint...)
	}
	if len(parts) == 0 {
		parts = append(parts, c.Config.Cmd...)
	}
	text := strings.Join(parts, " ")
	if len(text) > 28 {
		text = text[:25] + "..."
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
