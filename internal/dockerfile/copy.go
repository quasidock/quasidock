package dockerfile

import (
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/tree"
)

func (s *state) copy(command *instructions.CopyCommand, node *parser.Node) error {
	for _, flag := range node.Flags {
		name, _, _ := strings.Cut(flag, "=")
		if name != "--from" {
			return fmt.Errorf("COPY flag %s is unsupported; only --from can be translated", name)
		}
	}
	if len(node.Flags) > 0 && command.From == "" {
		return fmt.Errorf("COPY --from needs an earlier stage name or index")
	}
	dest, err := s.expand(command.DestPath, s.variables())
	if err != nil {
		return err
	}
	normal, err := appPath(s.workdir, dest)
	if err != nil {
		return err
	}
	directory := strings.HasSuffix(dest, "/") || normal == s.workdir || dest == "." || s.directories[normal]
	if directory {
		normal += "/"
	}
	paths := make([]string, len(command.SourcePaths))
	for index, raw := range command.SourcePaths {
		value, err := s.expand(raw, s.variables())
		if err != nil {
			return err
		}
		if command.From != "" {
			value = strings.TrimLeft(value, "/")
			if value == "" {
				value = "."
			}
		}
		if strings.HasPrefix(value, "/") || !oci.IsSafeRelPath(value) || strings.ContainsAny(value, "\\:\x00") {
			return fmt.Errorf("COPY source %q must stay inside its source root", raw)
		}
		if _, err := path.Match(value, ""); err != nil {
			return fmt.Errorf("invalid COPY selector %q: %w", raw, err)
		}
		paths[index] = path.Clean(value)
	}
	var source recipe.Source
	if command.From == "" {
		root, err := filepath.Rel(filepath.Dir(s.opts.Output), s.opts.Context)
		if err != nil {
			return err
		}
		dockerfile, err := filepath.Rel(s.opts.Context, s.opts.Dockerfile)
		if err != nil {
			return err
		}
		source.Context = &recipe.ContextSource{Root: filepath.ToSlash(root), Dockerfile: filepath.ToSlash(dockerfile), Paths: paths}
		// Literal context directories establish a directory destination even
		// without a trailing slash. Contents remain live build-time selectors.
		if len(paths) == 1 && !strings.ContainsAny(paths[0], "*?[") {
			info, statErr := os.Lstat(filepath.Join(s.opts.Context, filepath.FromSlash(paths[0])))
			if statErr == nil && info.IsDir() && info.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
				s.rememberDirectory(strings.TrimSuffix(normal, "/"))
			}
		}
	} else {
		if s.graph == nil || s.input == nil {
			return fmt.Errorf("COPY --from requires a parsed stage graph")
		}
		index, err := s.graph.copySource(s.input, command.From)
		if err != nil {
			return err
		}
		producer := s.previous[index]
		if producer == nil || producer.failed() {
			return fmt.Errorf("COPY --from stage %s must be translated successfully first", command.From)
		}
		if len(paths) > 1 && !strings.HasSuffix(normal, "/") {
			return fmt.Errorf("COPY with multiple sources requires a destination ending in '/'")
		}
		if err := s.copyExecutablePaths(producer, paths, normal, directory, node.StartLine); err != nil {
			return err
		}
		source.Build = &recipe.BuildSource{Name: s.graph.stages[index].unitName(), Paths: paths}
	}
	source.Dest = &normal
	s.addStage(recipe.Stage{Sources: []recipe.Source{source}})
	s.result.SourceLines[s.r.Stages[len(s.r.Stages)-1].Name] = node.StartLine
	if directory {
		s.rememberDirectory(strings.TrimSuffix(normal, "/"))
	} else {
		s.rememberDirectory(path.Dir(normal))
	}
	return nil
}

// Infer only paths whose executable type is known. Other generated files and
// glob matches remain live selectors evaluated by the builder.
func (s *state) copyExecutablePaths(producer *state, selectors []string, dest string, directory bool, line int) error {
	known := tree.New()
	for _, logical := range producer.executablePaths() {
		if err := known.AddFile(producer.outputs[logical], tree.Content{Kind: tree.ContentFile}); err != nil {
			return fmt.Errorf("ambiguous producer output paths: %w", err)
		}
	}
	view, err := known.ExecutableView(producer.executablePaths())
	if err != nil {
		return err
	}
	var matched []string
	for _, selector := range selectors {
		if selector == "." {
			matched = append(matched, selector)
			continue
		}
		for _, node := range view.Sorted() {
			if matches, _ := path.Match(selector, node.Path); matches {
				matched = append(matched, selector)
				break
			}
		}
	}
	if len(matched) == 0 {
		return nil
	}
	copied, mappings, err := known.CopyExecutables(matched, dest, directory, producer.executablePaths())
	if err != nil {
		return err
	}
	for _, logical := range slices.Sorted(maps.Keys(mappings)) {
		native := mappings[logical]
		if err := s.registerOutput(logical, native); err != nil {
			return err
		}
		if native != logical {
			s.diagnostic(line, "COPY", fmt.Sprintf("mapped executable %s to %s", logical, native), true)
		}
	}
	for _, node := range copied.Sorted() {
		if node.Content.Kind == tree.ContentDir {
			s.rememberDirectory(node.Path)
		}
	}
	return nil
}
