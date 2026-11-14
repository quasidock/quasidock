package info

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/store"
)

func PrintInfo(st *store.Store, envRegistry, imageText string, verbose bool, w io.Writer) error {
	app, err := remote.FetchInfo(st, envRegistry, imageText, verbose)
	if err != nil {
		return err
	}

	reg, err := registry.New(app.Target.Ref, "pull", verbose)
	if err != nil {
		return err
	}

	sigCheck, err := remote.CheckSignature(st, reg, app.Target.Ref, app.Index, app.ManifestDigest)
	if err != nil {
		return err
	}

	var sigText string
	switch sigCheck.Kind {
	case remote.SigNoKey:
		sigText = "not checked: no key here for its registry"
	case remote.SigMissing:
		sigText = "none: nothing says who made it"
	case remote.SigSigned:
		sigText = fmt.Sprintf("signed, by key %s", sigCheck.Fingerprint)
	case remote.SigWrong:
		sigText = fmt.Sprintf("WRONG: %s, so pull refuses it", sigCheck.Why)
	}

	cfg := app.Config
	WriteName(w, *cfg)
	if cfg.Homepage != nil {
		WriteLine(w, "homepage", *cfg.Homepage)
	}
	if cfg.License != nil {
		WriteLine(w, "license", *cfg.License)
	}
	WriteLine(w, "image", app.Target.Text)
	WriteLine(w, "manifest", app.ManifestDigest)
	WriteLine(w, "signature", sigText)
	if app.Index != nil {
		WriteLine(w, "index", fmt.Sprintf("%s, as its image for windows/amd64", *app.Index))
	}

	WriteContents(st, w, *app.Manifest, *cfg)

	if cfg.Build != nil {
		WriteLine(w, "built", DescribeBuild(*cfg.Build))
	}

	installedRef, err := st.ReadRef(cfg.ID)
	if err == nil && installedRef != nil {
		if installedRef.Manifest == app.ManifestDigest {
			WriteLine(w, "installed", fmt.Sprintf("%s, this image", installedRef.Version))
		} else {
			WriteLine(w, "installed", fmt.Sprintf("%s (%s)", installedRef.Version, oci.ShortDigest(installedRef.Manifest)))
		}
	} else {
		WriteLine(w, "installed", "no")
	}

	return nil
}

func WriteName(w io.Writer, cfg oci.AppConfig) {
	if cfg.Name != nil && *cfg.Name != "" {
		fmt.Fprintf(w, "%s %s: %s\n", cfg.ID, cfg.Version, *cfg.Name)
	} else {
		fmt.Fprintf(w, "%s %s\n", cfg.ID, cfg.Version)
	}
	if cfg.Summary != nil && *cfg.Summary != "" {
		fmt.Fprintf(w, "  %s\n", *cfg.Summary)
	}
}

func WriteContents(st *store.Store, w io.Writer, manifest oci.Manifest, cfg oci.AppConfig) {
	var total uint64 = uint64(manifest.Config.Size)
	var missing uint64 = 0

	for _, l := range manifest.Layers {
		total += uint64(l.Size)
		if st == nil || !st.HasBlob(l.Digest) {
			missing += uint64(l.Size)
		}
	}

	cfgSize := uint64(manifest.Config.Size)
	var dlStr string
	if missing == total-cfgSize {
		dlStr = formatBytes(total)
	} else if missing == 0 {
		dlStr = fmt.Sprintf("%s, all in the store already", formatBytes(total))
	} else {
		dlStr = fmt.Sprintf("%s, of which %s isn't in the store yet", formatBytes(total), formatBytes(missing))
	}
	WriteLine(w, "download", dlStr)

	var from []string
	for _, r := range cfg.From {
		from = append(from, fmt.Sprintf("%s %s", r.ID, r.Version))
	}
	sort.Strings(from)
	WriteLine(w, "from", strings.Join(from, ", "))

	ownLayer := oci.OwnLayer(&manifest)
	if ownLayer.Digest != "" {
		if f, ok := format.FormatOfDescriptor(ownLayer); ok && f != format.FormatGzip {
			WriteLine(w, "layer", f.String())
		}
	}

	if len(cfg.Entrypoint) > 0 {
		WriteLine(w, "entrypoint", strings.Join(cfg.Entrypoint, " "))
	}
	if len(cfg.Cmd) > 0 {
		WriteLine(w, "cmd", strings.Join(cfg.Cmd, " "))
	}

	if len(cfg.Permissions.Services) > 0 {
		WriteLine(w, "serves", oci.DescribeServices(cfg.Permissions.Services))
	}

	if cfg.Permissions.Unsupported != nil {
		for _, sb := range []oci.Sandbox{oci.SandboxAppContainer, oci.SandboxLow, oci.SandboxSoft} {
			if why := cfg.Permissions.Unsupported.Of(sb); why != nil && *why != "" {
				WriteLine(w, "can't run", fmt.Sprintf("under %s: %s", sb, *why))
			}
		}
	}

	WriteAccess(w, oci.AccessInfoOf(cfg))
}

func WriteAccess(w io.Writer, access oci.AccessInfo) {
	WriteLine(w, "permits", oci.DescribePermissions(access.Permissions))
	if len(access.Uses) > 0 {
		WriteLine(w, "uses", oci.DescribeUses(access.Uses))
	}
	if len(access.Registry) > 0 {
		WriteLine(w, "keeps", fmt.Sprintf("%s with its data", oci.DescribeRegistry(access.Registry)))
	}
}

func WriteLine(w io.Writer, label, value string) {
	if len(value) > 0 {
		fmt.Fprintf(w, "  %-10s %s\n", label, value)
	}
}

func DescribeBuild(b oci.Build) string {
	var parts []string
	var sdks []string
	for alias := range b.SDK {
		sdks = append(sdks, alias)
	}
	sort.Strings(sdks)

	sText := "s"
	if len(b.Sources) == 1 {
		sText = ""
	}
	downloads := fmt.Sprintf("from %d pinned download%s", len(b.Sources), sText)

	if len(sdks) > 0 {
		parts = append(parts, fmt.Sprintf("%s, with %s", downloads, oci.JoinAnd(sdks)))
	} else {
		parts = append(parts, fmt.Sprintf("%s, without an SDK", downloads))
	}

	if b.Network {
		parts = append(parts, "a step used the network, so it may not rebuild the same")
	}
	if len(b.Host) > 0 {
		var hosts []string
		for k := range b.Host {
			hosts = append(hosts, k)
		}
		sort.Strings(hosts)
		parts = append(parts, fmt.Sprintf("with the build machine's %s, so it won't rebuild the same elsewhere", strings.Join(hosts, ", ")))
	}

	return strings.Join(parts, "; ")
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
