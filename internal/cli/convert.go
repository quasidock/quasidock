package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/quasidock/quasidock/internal/buildcontext"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/dockerfile"
	"github.com/quasidock/quasidock/internal/recipe"
)

const convertUsage = "usage: quasidock convert [CONTEXT] [-f DOCKERFILE] [-o RECIPE|-] [--id ID] [--version VERSION] [--build-arg NAME=VALUE] [--target NAME] [--base IMAGE] [--stage-base NAME_OR_INDEX=IMAGE]... [--tool TOOL=IMAGE]... [--stage-tool NAME_OR_INDEX:TOOL=IMAGE]... [--force]"

func convertCommand(ctx *context.Context, args []string) (uint32, error) {
	opts := dockerfile.Options{BuildArgs: map[string]string{}, StageBases: map[string]string{}, Tools: map[string]string{}, StageTools: map[string]map[string]string{}}
	force, stdout := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" {
			fmt.Fprintln(ctx.Stdout, convertUsage)
			return 0, nil
		}
		if arg == "--force" {
			force = true
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			if opts.Context != "" {
				return 1, errors.New(convertUsage)
			}
			opts.Context = arg
			continue
		}
		name, value, has := strings.Cut(arg, "=")
		if !has {
			i++
			if i >= len(args) {
				return 1, fmt.Errorf("%s needs a value", name)
			}
			value = args[i]
		}
		switch name {
		case "-f", "--file":
			opts.Dockerfile = value
		case "-o", "--output":
			opts.Output = value
		case "--id":
			opts.ID = value
		case "--version":
			opts.Version = value
		case "--base":
			opts.Base = value
		case "--target":
			if value == "" || strings.HasPrefix(value, "-") {
				return 1, errors.New("--target needs a stage name")
			}
			if opts.Target != "" {
				return 1, errors.New("--target may only be supplied once")
			}
			opts.Target = value
		case "--stage-base":
			selector, image, ok := strings.Cut(value, "=")
			if !ok || selector == "" || strings.TrimSpace(image) == "" {
				return 1, errors.New("--stage-base needs NAME_OR_INDEX=IMAGE")
			}
			selector = strings.ToLower(selector)
			if _, exists := opts.StageBases[selector]; exists {
				return 1, fmt.Errorf("duplicate --stage-base override for %q", selector)
			}
			opts.StageBases[selector] = image
		case "--tool", "--stage-tool":
			selector, spec := "", value
			if name == "--stage-tool" {
				var ok bool
				selector, spec, ok = strings.Cut(value, ":")
				if !ok || selector == "" {
					return 1, errors.New("--stage-tool needs NAME_OR_INDEX:TOOL=IMAGE")
				}
				selector = strings.ToLower(selector)
			}
			alias, image, ok := strings.Cut(spec, "=")
			if !ok || alias == "" || strings.TrimSpace(image) == "" {
				return 1, fmt.Errorf("%s needs a tool alias and a nonempty IMAGE separated by =", name)
			}
			alias = strings.ToLower(alias)
			destination := opts.Tools
			if name == "--stage-tool" {
				if opts.StageTools[selector] == nil {
					opts.StageTools[selector] = map[string]string{}
				}
				destination = opts.StageTools[selector]
			}
			if _, exists := destination[alias]; exists {
				return 1, fmt.Errorf("duplicate %s override for tool %q", name, alias)
			}
			destination[alias] = image
		case "--build-arg":
			k, v, ok := strings.Cut(value, "=")
			if !ok || k == "" {
				return 1, errors.New("--build-arg needs NAME=VALUE")
			}
			opts.BuildArgs[k] = v
		default:
			return 1, fmt.Errorf("unknown conversion option %q", name)
		}
	}
	if opts.Context == "" {
		opts.Context = "."
	}
	var err error
	opts.Context, err = filepath.Abs(opts.Context)
	if err != nil {
		return 1, err
	}
	if opts.Dockerfile == "" {
		opts.Dockerfile = filepath.Join(opts.Context, "Dockerfile")
	}
	opts.Dockerfile, err = filepath.Abs(opts.Dockerfile)
	if err != nil {
		return 1, err
	}
	rel, err := filepath.Rel(opts.Context, opts.Dockerfile)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return 1, errors.New("Dockerfile must be inside the selected build context")
	}
	if opts.Output == "-" {
		stdout = true
		opts.Output = ""
	}
	if opts.Output == "" {
		opts.Output = filepath.Join(opts.Context, "quasidock.json")
	}
	opts.Output, err = filepath.Abs(opts.Output)
	if err != nil {
		return 1, err
	}
	if strings.EqualFold(opts.Output, opts.Dockerfile) {
		return 1, errors.New("output must not overwrite the Dockerfile")
	}
	if !stdout && !force {
		if _, err := os.Lstat(opts.Output); err == nil {
			return 1, fmt.Errorf("%s already exists; use --force to replace it", opts.Output)
		} else if !os.IsNotExist(err) {
			return 1, err
		}
	}
	data, err := os.ReadFile(opts.Dockerfile)
	if err != nil {
		return 1, err
	}
	if !utf8.Valid(data) {
		return 1, errors.New("Dockerfile must be UTF-8")
	}
	lock, err := ctx.Store.Lock(false)
	if err != nil {
		return 1, err
	}
	defer lock.Release()
	result, err := dockerfile.Convert(data, opts, dockerfile.ImageResolver{Context: ctx})
	for _, d := range result.Diagnostics {
		level := "error"
		if d.Warning {
			level = "note"
		}
		fmt.Fprintf(ctx.Stderr, "%s: %s\n", level, d.String())
	}
	if err != nil {
		return 1, err
	}
	// Check COPY metadata now; only builds need content hashes and full snapshots.
	var paths []string
	collectPaths := func(stages []recipe.Stage) {
		for _, stage := range stages {
			for _, src := range stage.Sources {
				if src.Context != nil {
					paths = append(paths, src.Context.Paths...)
				}
			}
		}
	}
	for _, unit := range result.Recipe.Builds {
		collectPaths(unit.Stages)
	}
	collectPaths(result.Recipe.Stages)
	var captured *buildcontext.Snapshot
	if len(paths) > 0 {
		captured, err = buildcontext.Inspect(opts.Context, filepath.ToSlash(rel), opts.Output, paths)
		if err != nil {
			return 1, fmt.Errorf("%s:1: context: %w", opts.Dockerfile, err)
		}
	}
	validateContext := func(stages []recipe.Stage, prefix string, executables []string) error {
		for _, stage := range stages {
			for _, src := range stage.Sources {
				if src.Context == nil {
					continue
				}
				copied, err := captured.Copy(src.Context.Paths, *src.Dest, false)
				if err == nil {
					err = copied.CheckExecutableCollisions(executables, nil)
				}
				if err != nil {
					key := prefix + stage.Name
					where := ""
					if name := result.SourceStages[key]; name != "" {
						where = "stage " + name + ": "
					}
					return fmt.Errorf("%s:%d: COPY: %s%w", opts.Dockerfile, result.SourceLines[key], where, err)
				}
			}
		}
		return nil
	}
	for _, unit := range result.Recipe.Builds {
		if err := validateContext(unit.Stages, unit.Name+"/", unit.Executables); err != nil {
			return 1, err
		}
	}
	if err := validateContext(result.Recipe.Stages, "", result.Recipe.Executables); err != nil {
		return 1, err
	}
	encoded, err := json.MarshalIndent(result.Recipe, "", "  ")
	if err != nil {
		return 1, err
	}
	encoded = append(encoded, '\n')
	if stdout {
		_, err = ctx.Stdout.Write(encoded)
		return 0, err
	}
	if err := writeConvertedRecipe(opts.Output, encoded, force); err != nil {
		return 1, err
	}
	fmt.Fprintf(ctx.Stderr, "wrote %s; review it, then run quasidock build %s\n", opts.Output, opts.Output)
	return 0, nil
}

func writeConvertedRecipe(name string, data []byte, force bool) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".quasidock-convert-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !force {
		return os.Link(tmp, name)
	} // Atomic publication without replacing a competing writer.
	return os.Rename(tmp, name)
}
