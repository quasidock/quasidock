package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/bits"
	"os"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/version"
	"github.com/quasidock/quasidock/internal/win32"
)

const (
	systemUsage     = "usage: quasidock system <info|df|events|prune> [OPTIONS]"
	systemInfoUsage = "usage: quasidock system info [-f|--format json|TEMPLATE]"
	systemDFUsage   = "usage: quasidock system df [-v|--verbose] [--format table|json|TEMPLATE]"
	systemDFTable   = "{{.Type}}\t{{.TotalCount}}\t{{.Active}}\t{{.Size}}\t{{.Reclaimable}}"
)

type systemOutput struct {
	verbose, json, table bool
	template             *template.Template
}

func parseSystemOutput(command string, args []string) (systemOutput, error) {
	var opts systemOutput
	var format string
	flags := flag.NewFlagSet("system "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	setFormat := func(value string) error {
		if strings.TrimSpace(value) == "" {
			return errors.New("format is empty")
		}
		format = value
		return nil
	}
	flags.Func("format", "", setFormat)
	if command == "info" {
		flags.Func("f", "", setFormat)
	} else {
		flags.BoolVar(&opts.verbose, "verbose", false, "")
		flags.BoolVar(&opts.verbose, "v", false, "")
	}
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if opts.verbose && format != "" && format != "table" {
		return opts, errors.New("--verbose accepts only table output")
	}
	if format == "json" {
		opts.json = true
		return opts, nil
	}
	if command == "df" {
		switch {
		case format == "" || format == "table":
			opts.table, format = true, systemDFTable
		case strings.HasPrefix(format, "table "):
			opts.table, format = true, strings.TrimPrefix(format, "table ")
			if strings.TrimSpace(format) == "" {
				return opts, errors.New("table format needs a template")
			}
		}
	}
	if format != "" {
		var err error
		opts.template, err = template.New("system " + command).Funcs(template.FuncMap{
			"json": func(value any) (string, error) {
				data, err := json.Marshal(value)
				return string(data), err
			},
		}).Parse(format)
		if err != nil {
			return opts, err
		}
	}
	return opts, nil
}

func systemCommand(ctx *context.Context, args []string) (uint32, error) {
	if len(args) == 0 {
		return 1, errors.New(systemUsage)
	}
	command, rest := args[0], args[1:]
	var usage string
	switch command {
	case "help", "-h", "--help":
		return writeSystemHelp(ctx.Stdout, systemUsage+"\n"+systemInfoUsage+"\n"+systemDFUsage+"\n"+systemPruneUsage)
	case "prune":
		return systemPrune(ctx, rest, os.Stdin, windowsNetworkBackend)
	case "events":
		if err := openCommandStore(ctx); err != nil {
			return 1, err
		}
		return eventsCommand(ctx, rest)
	case "info":
		usage = systemInfoUsage
	case "df":
		usage = systemDFUsage
	default:
		return 1, fmt.Errorf("unknown system command %q\n%s", command, systemUsage)
	}
	opts, err := parseSystemOutput(command, rest)
	if errors.Is(err, flag.ErrHelp) {
		return writeSystemHelp(ctx.Stdout, usage)
	}
	if err != nil {
		return 1, fmt.Errorf("system %s: %w\n%s", command, err, usage)
	}
	if err := openCommandStore(ctx); err != nil {
		return 1, err
	}
	var frame bytes.Buffer
	if command == "info" {
		report, err := loadSystemInfo(ctx)
		if err != nil {
			return 1, err
		}
		if err := renderSystemInfo(&frame, opts, report); err != nil {
			return 1, err
		}
	} else {
		usage, err := loadSystemDF(ctx.Store)
		if err != nil {
			return 1, err
		}
		if err := renderSystemDF(&frame, opts, usage); err != nil {
			return 1, err
		}
	}
	_, err = frame.WriteTo(ctx.Stdout)
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func writeSystemHelp(out io.Writer, usage string) (uint32, error) {
	if _, err := fmt.Fprintln(out, usage); err != nil {
		return 1, err
	}
	return 0, nil
}

type systemInfoReport struct {
	Version, OSType, OSVersion, Architecture, StoreRoot, Registry string
	CPUs                                                          uint32
	MemTotal                                                      uint64
	Images, Volumes, TrackedNetworks, Containers                  int
	ContainersRunning, ContainersPaused, ContainersRestarting     int
	ContainersCreated, ContainersExited                           int
}

func loadSystemInfo(ctx *context.Context) (systemInfoReport, error) {
	st := ctx.Store
	lock, err := st.Lock(false)
	if err != nil {
		return systemInfoReport{}, err
	}
	defer lock.Release()
	inventory, err := st.ReadSystemInventory(nil)
	if err != nil {
		return systemInfoReport{}, err
	}
	networks, err := st.TrackedNetworkIDs()
	if err != nil {
		return systemInfoReport{}, err
	}
	report := systemInfoReport{
		Version: version.Version, OSType: runtime.GOOS, Architecture: runtime.GOARCH,
		StoreRoot: st.FullRoot(), Registry: ctx.Env["QUASIDOCK_REGISTRY"],
		Images: len(inventory.Images.Images), Volumes: len(inventory.Volumes),
		TrackedNetworks: len(networks), Containers: len(inventory.Containers),
	}
	if report.Registry == "" {
		report.Registry = store.DefaultImageRegistry
	}
	report.OSVersion, err = win32.OSVersion()
	if err != nil {
		return systemInfoReport{}, err
	}
	report.CPUs, err = win32.ActiveProcessorCount()
	if err != nil {
		return systemInfoReport{}, fmt.Errorf("querying logical CPUs: %w", err)
	}
	report.MemTotal, err = win32.TotalPhysicalMemory()
	if err != nil {
		return systemInfoReport{}, err
	}
	for _, c := range inventory.Containers {
		switch c.Status {
		case "running":
			report.ContainersRunning++
		case "paused":
			report.ContainersPaused++
		case "restarting":
			report.ContainersRestarting++
		case "created":
			report.ContainersCreated++
		case "exited":
			report.ContainersExited++
		}
	}
	return report, nil
}

func renderSystemInfo(out *bytes.Buffer, opts systemOutput, r systemInfoReport) error {
	if opts.json {
		return json.NewEncoder(out).Encode(r)
	}
	if opts.template != nil {
		if err := opts.template.Execute(out, r); err != nil {
			return err
		}
		out.WriteByte('\n')
		return nil
	}
	fmt.Fprintf(out, "Quasidock Version: %s\nOS: %s (%s)\nArchitecture: %s\nCPUs: %d\nTotal Memory: %s\n",
		r.Version, r.OSType, r.OSVersion, r.Architecture, r.CPUs, lifecycle.FormatBytes(r.MemTotal))
	fmt.Fprintf(out, "Store Root: %s\nRegistry: %s\nImages: %d\nVolumes: %d\nTracked Networks: %d\n",
		escapeTableText(r.StoreRoot), escapeTableText(r.Registry), r.Images, r.Volumes, r.TrackedNetworks)
	fmt.Fprintf(out, "Containers: %d\n  Running: %d\n  Paused: %d\n  Restarting: %d\n  Created: %d\n  Exited: %d\n",
		r.Containers, r.ContainersRunning, r.ContainersPaused, r.ContainersRestarting, r.ContainersCreated, r.ContainersExited)
	return nil
}

func loadSystemDF(st *store.Store) (*store.DiskUsage, error) {
	lock, err := st.Lock(true)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	guard, err := grants.LockMountSources(st)
	if err != nil {
		return nil, err
	}
	inventory, err := st.ReadSystemInventory(guard.Protected)
	guard.Close()
	if err != nil {
		return nil, err
	}
	return st.DiskUsage(inventory)
}

type systemDFRow struct {
	Type, TotalCount, Active, Size, Reclaimable string
}

func dfRow(category string, totals store.DiskUsageTotals) systemDFRow {
	reclaimable := lifecycle.FormatBytes(totals.Reclaimable)
	if totals.Size != 0 {
		hi, lo := bits.Mul64(totals.Reclaimable, 100)
		percent, _ := bits.Div64(hi, lo, totals.Size)
		reclaimable = fmt.Sprintf("%s (%d%%)", reclaimable, percent)
	}
	return systemDFRow{
		Type: category, TotalCount: strconv.Itoa(totals.Total), Active: strconv.Itoa(totals.Active),
		Size: lifecycle.FormatBytes(totals.Size), Reclaimable: reclaimable,
	}
}

func renderSystemDF(out *bytes.Buffer, opts systemOutput, usage *store.DiskUsage) error {
	if opts.verbose {
		return renderSystemDFDetails(out, usage)
	}
	rows := []systemDFRow{
		dfRow("Images", usage.ImageTotals), dfRow("Containers", usage.ContainerTotals),
		dfRow("Local Volumes", usage.VolumeTotals), dfRow("Build Cache", usage.CacheTotals),
	}
	if opts.json {
		encoder := json.NewEncoder(out)
		for _, row := range rows {
			if err := encoder.Encode(row); err != nil {
				return err
			}
		}
		return nil
	}
	var writer io.Writer = out
	var table *tabwriter.Writer
	if opts.table {
		table = tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
		writer = table
		heading := systemDFRow{"TYPE", "TOTAL", "ACTIVE", "SIZE", "RECLAIMABLE"}
		rows = append([]systemDFRow{heading}, rows...)
	}
	for _, row := range rows {
		if err := opts.template.Execute(writer, row); err != nil {
			return err
		}
		fmt.Fprintln(writer)
	}
	if table != nil {
		return table.Flush()
	}
	return nil
}

func renderSystemDFDetails(out *bytes.Buffer, usage *store.DiskUsage) error {
	table := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	format := lifecycle.FormatBytes
	fmt.Fprintln(table, "Images space usage\nIMAGE ID\tNAMES\tSIZE\tSHARED SIZE\tUNIQUE SIZE\tCONTAINERS")
	for _, row := range usage.Images {
		names := strings.Join(row.Names, ", ")
		if names == "" {
			names = "<none>"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%d\n", row.ID, names, format(row.SharedSize+row.UniqueSize),
			format(row.SharedSize), format(row.UniqueSize), row.Containers)
	}
	fmt.Fprintln(table, "\nContainers space usage\nCONTAINER ID\tNAME\tIMAGE\tSTATUS\tSIZE")
	for _, row := range usage.Containers {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", row.ID, escapeTableText(row.Name), escapeTableText(row.Image), row.Status, format(row.Size))
	}
	fmt.Fprintln(table, "\nLocal Volumes space usage\nNAME\tLINKS\tSIZE")
	for _, row := range usage.Volumes {
		fmt.Fprintf(table, "%s\t%d\t%s\n", escapeTableText(row.Name), row.References, format(row.Size))
	}
	fmt.Fprintln(table, "\nBuild Cache space usage\nAREA\tENTRY\tSIZE")
	for _, row := range usage.Cache {
		fmt.Fprintf(table, "%s\t%s\t%s\n", row.Area, escapeTableText(row.Name), format(row.Size))
	}
	return table.Flush()
}
