package run

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

// CommitConfiguration preserves launch inputs rather than expanded process
// arguments or the environment of the caller doing the commit.
func CommitConfiguration(ctx *context.Context, c *store.ContainerRecord, img *store.Image) (oci.AppConfig, error) {
	data, err := json.Marshal(img.Config)
	if err != nil {
		return oci.AppConfig{}, err
	}
	var cfg oci.AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	cfg.FormatVersion, cfg.Build = 4, nil
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	for _, entry := range c.RunSpec.Env {
		name, value, explicit := strings.Cut(entry, "=")
		var replacement *string
		if explicit {
			replacement = &value
		} else {
			replacement = c.RunSpec.InheritedEnv[strings.ToLower(name)]
		}
		for key := range cfg.Env {
			if strings.EqualFold(key, name) {
				delete(cfg.Env, key)
			}
		}
		kept := cfg.UnsetEnv[:0]
		for _, old := range cfg.UnsetEnv {
			if !strings.EqualFold(old, name) {
				kept = append(kept, old)
			}
		}
		cfg.UnsetEnv = kept
		if replacement == nil {
			cfg.UnsetEnv = append(cfg.UnsetEnv, name)
		} else {
			cfg.Env[name] = *replacement
		}
	}
	opts := Options{Entrypoint: c.RunSpec.Entrypoint, Args: c.RunSpec.Args}
	if opts.Entrypoint == nil && len(opts.Args) == 0 {
		return cfg, nil
	}
	p := oci.Placeholders{App: img.DeployDir, Data: c.DataDir, Cache: filepath.Join(c.DataDir, "cache"), Runtimes: oci.RuntimeDirectories(img.Config.From, img.RuntimeDirs)}
	paths, _ := environment.PathDirs(p, img.Config.Path)
	for _, r := range p.Runtimes {
		rp := p
		rp.App = r.Path
		dirs, _ := environment.PathDirs(rp, img.Config.From[r.Alias].Path)
		paths = append(paths, dirs...)
	}
	systemRoot := ctx.Env["SystemRoot"]
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	cmd, err := commandForRun(p, img.Config, opts, paths, systemRoot)
	if err != nil {
		return cfg, err
	}
	portable := ""
	anchors := append([]oci.DirPlaceholder{{Alias: "app", Path: p.App}, {Alias: "data", Path: p.Data}}, p.Runtimes...)
	for _, a := range anchors {
		rel, err := filepath.Rel(a.Path, cmd.Exe)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, `..\`) && !filepath.IsAbs(rel) {
			portable = "${" + a.Alias + "}/" + filepath.ToSlash(rel)
			break
		}
	}
	if portable == "" && strings.EqualFold(filepath.Dir(cmd.Exe), filepath.Join(systemRoot, "System32")) {
		portable = filepath.Base(cmd.Exe)
		resolved, ok := findCommand(p.App, portable, paths, systemRoot)
		if !ok || !strings.EqualFold(resolved, cmd.Exe) {
			portable = ""
		}
	}
	if portable == "" {
		return cfg, fmt.Errorf("cannot commit host executable %q; choose an image or private-data command with --change", cmd.Exe)
	}
	// resolveCommand only contributes fixed image-entrypoint arguments. Keep their
	// original placeholders rather than its expanded values.
	fixed := []string{}
	if len(cmd.Args) > 0 {
		fixed = append(fixed, img.Config.Entrypoint[1:]...)
	}
	args := opts.Args
	if opts.Entrypoint == nil || *opts.Entrypoint == "" {
		args = args[1:]
	}
	cfg.Entrypoint = append([]string{portable}, fixed...)
	cfg.Cmd = append([]string(nil), args...)
	return cfg, nil
}
