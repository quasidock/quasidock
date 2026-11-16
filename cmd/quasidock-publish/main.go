// quasidock-publish is a repository maintenance tool, not a release binary.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/quasidock/quasidock/internal/catalog"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/publishing"
	"github.com/quasidock/quasidock/internal/registry"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/trust"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(path, append(b, '\n'), 0644)
}
func command(args ...string) ([]byte, error) {
	exe := os.Getenv("QUASIDOCK")
	if exe == "" {
		exe = "out/bin/quasidock.exe"
	}
	c := exec.Command(exe, args...)
	c.Stderr = os.Stderr
	return c.Output()
}
func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: quasidock-publish generate|seed-busybox|prepare|record|collect|catalog|verify|promote|verify-release")
	}
	recipes, err := publishing.Load(".")
	if err != nil {
		return err
	}
	if args[0] == "generate" {
		path := ".github/workflows/publish.yml"
		want := []byte(publishing.Workflow(recipes))
		if len(args) > 1 && args[1] == "--check" {
			have, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			if strings.ReplaceAll(string(have), "\r\n", "\n") != string(want) {
				return fmt.Errorf("publishing workflow is stale: run go run ./cmd/quasidock-publish generate")
			}
			return nil
		}
		return os.WriteFile(path, want, 0644)
	}
	if args[0] == "dependencies" {
		if len(args) != 2 {
			return fmt.Errorf("dependencies RECIPE")
		}
		for _, r := range recipes {
			if r.File == args[1] {
				for _, p := range r.Pins {
					fmt.Println(p.ID)
				}
				return nil
			}
		}
		return fmt.Errorf("recipe is not allowlisted: %s", args[1])
	}
	if args[0] == "seed-busybox" {
		if len(args) > 2 {
			return fmt.Errorf("seed-busybox [LOCAL-EXE]")
		}
		local := ""
		if len(args) == 2 {
			local = args[1]
		}
		for _, r := range recipes {
			if r.ID == "busybox" {
				ctx := context.New()
				ctx.Store, err = store.Open("", false, "")
				if err != nil {
					return err
				}
				path, err := seedBusybox(ctx, r, local)
				if err == nil {
					fmt.Println(path)
				}
				return err
			}
		}
		return fmt.Errorf("busybox is not in the publish allowlist")
	}
	commit, tag := os.Getenv("GITHUB_SHA"), "ci-"+os.Getenv("GITHUB_RUN_ID")
	if len(commit) != 40 || tag == "ci-" || !registry.IsValidTag(tag) {
		return fmt.Errorf("GITHUB_SHA and GITHUB_RUN_ID must identify the publishing run")
	}
	find := func(file string) (publishing.Recipe, error) {
		for _, r := range recipes {
			if r.File == file {
				return r, nil
			}
		}
		return publishing.Recipe{}, fmt.Errorf("recipe %q is not allowlisted", file)
	}
	switch args[0] {
	case "verify-image":
		if len(args) != 2 {
			return fmt.Errorf("verify-image REPORT")
		}
		data, e := os.ReadFile(args[1])
		if e != nil {
			return e
		}
		var rp publishing.Report
		if e = json.Unmarshal(data, &rp); e != nil {
			return e
		}
		r, e := find(rp.Recipe)
		if e != nil {
			return e
		}
		if rp.ID != r.ID || rp.Version != r.Version || rp.Commit != commit || rp.Tag != tag || rp.Registry != publishing.Registry {
			return fmt.Errorf("candidate report belongs to another publishing run")
		}
		ctx := context.New()
		root := os.Getenv("QUASIDOCK_HOME")
		if root == "" {
			return fmt.Errorf("set QUASIDOCK_HOME")
		}
		ctx.Store, e = store.Open(root, false, "")
		if e != nil {
			return e
		}
		if strings.TrimSpace(trust.QuasidockPublicKey) == "" {
			return fmt.Errorf("configure the Quasidock public key")
		}
		return verify(ctx, rp, false)
	case "prepare":
		if len(args) != 5 {
			return fmt.Errorf("prepare RECIPE REPORTS WORKDIR OUTPUT")
		}
		r, e := find(args[1])
		if e != nil {
			return e
		}
		reports, e := publishing.ReadReports(args[2])
		if e != nil {
			return e
		}
		data, e := publishing.Prepare(r, reports, commit, tag)
		if e != nil {
			return e
		}
		if e = copyTree("recipes", filepath.Join(args[3], "recipes")); e != nil {
			return e
		}
		path, e := filepath.Abs(filepath.Join(args[3], "recipes", r.File))
		if e != nil {
			return e
		}
		if e = os.WriteFile(path, data, 0644); e != nil {
			return e
		}
		return os.WriteFile(args[4], []byte("recipe="+path+"\nid="+r.ID+"\n"), 0644)
	case "record":
		if len(args) != 4 {
			return fmt.Errorf("record RECIPE PREPARED REPORTDIR")
		}
		r, e := find(args[1])
		if e != nil {
			return e
		}
		data, e := os.ReadFile(args[2])
		if e != nil {
			return e
		}
		prepared, e := publishing.Parse(r.File, data)
		if e != nil {
			return e
		}
		inspect, e := command("image", "inspect", r.ID)
		if e != nil {
			return e
		}
		var images []struct {
			Id        string
			Quasidock oci.AppConfig
		}
		if e = json.Unmarshal(inspect, &images); e != nil {
			return e
		}
		if len(images) != 1 {
			return fmt.Errorf("expected one built image")
		}
		report := publishing.Report{Recipe: r.File, ID: r.ID, Version: r.Version, Digest: images[0].Id, Commit: commit, Tag: tag, Registry: publishing.Registry, Pins: prepared.Pins, RecipeJSON: data, Sources: []string{}}
		if cfg := images[0].Quasidock.Build; cfg != nil {
			seen := map[string]bool{}
			for _, h := range cfg.Sources {
				seen[h] = true
			}
			for _, h := range cfg.Vendor {
				seen[h] = true
			}
			for h := range seen {
				if _, e := os.Stat(filepath.Join(os.Getenv("QUASIDOCK_HOME"), "cache", "downloads", h)); e == nil {
					report.Sources = append(report.Sources, h)
				}
			}
			sort.Strings(report.Sources)
		}
		if e = os.MkdirAll(args[3], 0755); e != nil {
			return e
		}
		if e = writeJSON(filepath.Join(args[3], r.ID+".report.json"), report); e != nil {
			return e
		}
		fmt.Printf("digest=%s\n", report.Digest)
		return nil
	case "collect":
		if len(args) != 3 {
			return fmt.Errorf("collect REPORTS OUTDIR")
		}
		reports, e := publishing.ReadReports(args[1])
		if e != nil {
			return e
		}
		if e = publishing.Validate(recipes, reports, commit, tag); e != nil {
			return e
		}
		if e = os.MkdirAll(filepath.Join(args[2], "recipes"), 0755); e != nil {
			return e
		}
		var patch, summary strings.Builder
		fmt.Fprintf(&summary, "# Publication %s\n\nSource commit: `%s`.\n\n", tag, commit)
		for _, r := range recipes {
			rp := reports[r.ID]
			if e = os.WriteFile(filepath.Join(args[2], "recipes", r.File), rp.RecipeJSON, 0644); e != nil {
				return e
			}
			fmt.Fprintf(&summary, "- `%s:%s` → `%s`\n", r.ID, r.Version, rp.Digest)
			if string(r.Bytes) != string(rp.RecipeJSON) {
				patch.WriteString(fullPatch("recipes/"+r.File, r.Bytes, rp.RecipeJSON))
			}
		}
		summary.WriteString("\nThis run verifies anonymous access and promotes the signed candidates to version/latest tags, with the catalog last. New packages must be public under https://github.com/orgs/quasidock/packages for anonymous verification to pass.\n\nThe recipes/ directory contains the exact published inputs. If pins.patch is nonempty, review and commit it to synchronize source recipes before running Reproduce; another publication run is not required.\n")
		if e = os.WriteFile(filepath.Join(args[2], "pins.patch"), []byte(patch.String()), 0644); e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(args[2], "summary.md"), []byte(summary.String()), 0644); e != nil {
			return e
		}
		return writeJSON(filepath.Join(args[2], "images.json"), reports)
	case "catalog", "verify", "promote", "verify-release":
		if len(args) < 2 {
			return fmt.Errorf("%s REPORTS [TAG]", args[0])
		}
		reports, e := publishing.ReadReports(args[1])
		if e != nil {
			return e
		}
		// Staging, verification and promotion all use the same complete set of
		// recipes prepared from this run's dependency results.
		if e = publishing.Validate(recipes, reports, commit, tag); e != nil {
			return e
		}
		ctx := context.New()
		root := os.Getenv("QUASIDOCK_HOME")
		if root == "" {
			return fmt.Errorf("set QUASIDOCK_HOME to an isolated publishing store")
		}
		ctx.Store, e = store.Open(root, false, "")
		if e != nil {
			return e
		}
		var refs []string
		for _, r := range recipes {
			refs = append(refs, reports[r.ID].Ref())
		}
		if args[0] == "catalog" {
			if len(args) != 3 || args[2] != tag {
				return fmt.Errorf("candidate catalog tag must be %s", tag)
			}
			return catalog.PushRefs(ctx.Store, publishing.Registry, refs, tag, false)
		}
		if strings.TrimSpace(trust.QuasidockPublicKey) == "" {
			return fmt.Errorf("configure internal/trust/quasidock.pub before publishing")
		}
		for _, r := range recipes {
			rp := reports[r.ID]
			if e = verify(ctx, rp, args[0] == "verify-release"); e != nil {
				return e
			}
		}
		if args[0] == "promote" {
			for _, r := range recipes {
				rp := reports[r.ID]
				ref, e := registry.ParseReference(rp.Ref())
				if e != nil {
					return e
				}
				reg, e := registry.New(ref, "pull,push", false)
				if e != nil {
					return e
				}
				m, e := reg.FetchManifest()
				if e != nil {
					return e
				}
				for _, t := range []string{rp.Version, "latest"} {
					if e = reg.PushManifest(t, m.Bytes); e != nil {
						return fmt.Errorf("promotion stopped at %s:%s; earlier tags may be promoted; retry this run: %w", rp.ID, t, e)
					}
					fmt.Printf("promoted %s:%s %s\n", rp.ID, t, rp.Digest)
				}
			}
			return catalog.PushRefs(ctx.Store, publishing.Registry, refs, "latest", false)
		}
		if args[0] == "verify-release" {
			cat, e := catalog.Load(ctx.Store, publishing.Registry, false)
			if e != nil {
				return e
			}
			if len(cat.Catalog.Apps) != len(recipes) {
				return fmt.Errorf("catalog does not contain the complete release")
			}
			for _, entry := range cat.Catalog.Apps {
				rp, ok := reports[entry.ID]
				if !ok || entry.Manifest != rp.Digest || entry.Version != rp.Version {
					return fmt.Errorf("catalog entry %s does not match release", entry.ID)
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown operation %q", args[0])
	}
}

func verify(ctx *context.Context, r publishing.Report, release bool) error {
	ref, err := registry.ParseReference(r.Ref())
	if err != nil {
		return err
	}
	reg, err := registry.New(ref, "pull", false)
	if err != nil {
		return err
	}
	resolved, err := remote.ResolveImage(reg, ref, "")
	if err != nil {
		return err
	}
	if resolved.Manifest.SchemaVersion != 2 || resolved.Manifest.MediaType != oci.MediaTypeManifest {
		return fmt.Errorf("invalid application manifest for %s", r.ID)
	}
	info, err := remote.FetchInfo(ctx.Store, publishing.Registry, r.Ref(), false)
	if err != nil {
		return err
	}
	if info.Config.ID != r.ID || info.Config.Version != r.Version {
		return fmt.Errorf("%s has the wrong config", r.ID)
	}
	sig, err := remote.CheckSignature(ctx.Store, reg, ref, nil, r.Digest)
	if err != nil {
		return err
	}
	if sig.Kind != remote.SigSigned {
		return fmt.Errorf("%s has no valid Quasidock signature", r.ID)
	}
	tag, _ := oci.SourcesTag(r.Digest)
	source, err := reg.FetchManifestRef(tag)
	if err != nil {
		return err
	}
	var m oci.Manifest
	if err = json.Unmarshal(source.Bytes, &m); err != nil {
		return err
	}
	if m.SchemaVersion != 2 || m.MediaType != oci.MediaTypeManifest || m.Config.MediaType != oci.MediaTypeSourcesConfig {
		return fmt.Errorf("invalid source artifact for %s", r.ID)
	}
	tmp := ctx.Store.TmpPath("source-config")
	defer os.Remove(tmp)
	if _, err = reg.DownloadBlob(m.Config, tmp); err != nil {
		return err
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	var sourceConfig oci.SourcesConfig
	if err = json.Unmarshal(data, &sourceConfig); err != nil {
		return err
	}
	if sourceConfig.ID != r.ID || sourceConfig.Version != r.Version || sourceConfig.Image != r.Digest {
		return fmt.Errorf("source artifact belongs to another image: %s", r.ID)
	}
	for _, hash := range r.Sources {
		found := false
		for _, layer := range m.Layers {
			if layer.Digest == "sha256:"+hash {
				has, e := reg.HasBlob(layer.Digest)
				if e != nil {
					return e
				}
				found = has
			}
		}
		if !found {
			return fmt.Errorf("missing source %s for %s", hash, r.ID)
		}
	}
	if release {
		for _, t := range []string{r.Version, "latest"} {
			got, e := reg.FetchManifestRef(t)
			if e != nil {
				return e
			}
			if got.Digest != resolved.Digest {
				return fmt.Errorf("%s:%s does not match release digest", r.ID, t)
			}
		}
	}
	fmt.Println("verified", r.Ref())
	return nil
}
func copyTree(src, dest string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("recipe tree contains symlink %s", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0644)
	})
}
func fullPatch(path string, old, new []byte) string {
	before, after := strings.Split(strings.TrimSuffix(string(old), "\n"), "\n"), strings.Split(strings.TrimSuffix(string(new), "\n"), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1,%d +1,%d @@\n", path, path, path, path, len(before), len(after))
	for _, line := range before {
		fmt.Fprintf(&b, "-%s\n", line)
	}
	for _, line := range after {
		fmt.Fprintf(&b, "+%s\n", line)
	}
	return b.String()
}
