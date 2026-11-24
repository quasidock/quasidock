package commit

import (
	"fmt"
	"path"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/quasidock/quasidock/internal/oci"
)

func ApplyChanges(cfg *oci.AppConfig, changes []string) error {
	for i, change := range changes {
		if err := applyChange(cfg, change); err != nil {
			return fmt.Errorf("--change %d: %w", i+1, err)
		}
	}
	if err := oci.ValidateConfig("committed image", cfg); err != nil {
		return fmt.Errorf("--change result: %w", err)
	}
	return nil
}

func applyChange(cfg *oci.AppConfig, change string) error {
	parsed, err := parser.Parse(strings.NewReader(change))
	if err != nil {
		return err
	}
	if len(parsed.AST.Children) != 1 {
		return fmt.Errorf("expected exactly one instruction")
	}
	node := parsed.AST.Children[0]
	if len(node.Heredocs) != 0 {
		return fmt.Errorf("heredocs are unsupported")
	}
	command, err := instructions.ParseInstruction(node)
	if err != nil {
		return err
	}
	lex := shell.NewLex(parsed.EscapeToken)
	lex.SkipUnsetEnv = true
	literal := func(value string) (string, error) {
		result, _, err := lex.ProcessWord(value, shell.EnvsFromSlice(nil))
		return result, err
	}
	switch c := command.(type) {
	case *instructions.CmdCommand:
		if !node.Attributes["json"] {
			return fmt.Errorf("CMD requires a JSON string array")
		}
		cfg.Cmd = append([]string(nil), c.CmdLine...)
	case *instructions.EntrypointCommand:
		if !node.Attributes["json"] {
			return fmt.Errorf("ENTRYPOINT requires a JSON string array")
		}
		cfg.Entrypoint, cfg.Cmd = append([]string(nil), c.CmdLine...), nil
	case *instructions.EnvCommand:
		if cfg.Env == nil {
			cfg.Env = map[string]string{}
		}
		for _, pair := range c.Env {
			value, err := literal(pair.Value)
			if err != nil {
				return err
			}
			for old := range cfg.Env {
				if strings.EqualFold(old, pair.Key) {
					delete(cfg.Env, old)
				}
			}
			cfg.Env[pair.Key] = value
			kept := cfg.UnsetEnv[:0]
			for _, name := range cfg.UnsetEnv {
				if !strings.EqualFold(name, pair.Key) {
					kept = append(kept, name)
				}
			}
			cfg.UnsetEnv = kept
		}
	case *instructions.WorkdirCommand:
		value, err := literal(c.Path)
		if err != nil {
			return err
		}
		workdir := strings.ReplaceAll(value, "\\", "/")
		if !oci.IsPlaceholderPath(workdir) {
			if !oci.IsSafeRelPath(workdir) {
				return fmt.Errorf("WORKDIR must be app-relative or start with ${data}")
			}
			workdir = path.Join(cfg.Workdir, workdir)
		}
		cfg.Workdir = workdir
	case *instructions.LabelCommand:
		if cfg.Labels == nil {
			cfg.Labels = map[string]string{}
		}
		for _, pair := range c.Labels {
			value, err := literal(pair.Value)
			if err != nil {
				return err
			}
			cfg.Labels[pair.Key] = value
		}
	default:
		return fmt.Errorf("unsupported instruction %s; supported: CMD, ENTRYPOINT, ENV, WORKDIR, LABEL", strings.ToUpper(node.Value))
	}
	return nil
}
