package builder

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/quasidock/quasidock/internal/buildcontext"
	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/tree"
)

const ManifestName = "quasidock.json"

type Snapshot struct {
	Context *buildcontext.Snapshot
	Dir     string
	Tree    *tree.Tree
	Hash    string
	Files   int
}

type snapshotEntry struct {
	module   string
	index    int
	snapshot Snapshot
}

type Snapshots struct {
	entries           []snapshotEntry
	units             map[string]*Snapshots
	outputs           map[string]*tree.Tree
	executables       []string
	outputExecutables map[string][]string
}

func TakeSnapshots(ctx *context.Context, r *recipe.Recipe, recipePath string) (*Snapshots, error) {
	order, err := r.ReachableBuilds()
	if err != nil {
		return nil, err
	}
	contexts := map[string]*buildcontext.Snapshot{}
	outputs := map[string]*tree.Tree{}
	units := map[string]*Snapshots{}
	outputExecutables := map[string][]string{}
	for _, unit := range order {
		outputExecutables[strings.ToLower(unit.Name)] = unit.Executables
		snapshots, err := takeScopeSnapshots(ctx, unitRecipe(r, unit), recipePath, contexts, outputs)
		if err != nil {
			return nil, fmt.Errorf("build %s: %w", unit.Name, err)
		}
		units[strings.ToLower(unit.Name)] = snapshots
	}
	s, err := takeScopeSnapshots(ctx, r, recipePath, contexts, outputs)
	if err != nil {
		return nil, err
	}
	s.units = units
	s.outputExecutables = outputExecutables
	for _, local := range units {
		local.outputExecutables = outputExecutables
	}
	return s, nil
}

func takeScopeSnapshots(ctx *context.Context, r *recipe.Recipe, recipePath string, contexts map[string]*buildcontext.Snapshot, outputs map[string]*tree.Tree) (*Snapshots, error) {
	s := &Snapshots{outputs: outputs, executables: r.Executables}
	baseDir := filepath.Dir(recipePath)
	if baseDir == "" {
		baseDir = "."
	}

	for _, m := range r.Stages {
		for i, src := range m.Sources {
			if c := src.Context; c != nil {
				root := c.Root
				if !filepath.IsAbs(root) {
					root = filepath.Join(baseDir, root)
				}
				root, _ = filepath.Abs(root)
				exclude, _ := filepath.Abs(recipePath)
				key := strings.ToLower(root + "\x00" + filepath.Clean(filepath.FromSlash(c.Dockerfile)))
				captured := contexts[key]
				if captured == nil {
					var err error
					captured, err = buildcontext.Read(root, c.Dockerfile, exclude)
					if err != nil {
						return nil, fmt.Errorf("stage %s: %w", m.Name, err)
					}
					contexts[key] = captured
				}
				s.entries = append(s.entries, snapshotEntry{module: m.Name, index: i, snapshot: Snapshot{Dir: captured.Root, Tree: captured.Tree, Hash: captured.Hash, Context: captured}})
				continue
			}
			if src.Path == nil {
				continue
			}
			p := *src.Path
			full := p
			if !filepath.IsAbs(p) {
				full = filepath.Join(baseDir, p)
			}

			evalFull, err := filepath.EvalSymlinks(full)
			if err != nil {
				continue
			}
			stat, err := os.Stat(evalFull)
			if err != nil {
				continue
			}
			if !stat.IsDir() {
				if len(src.Skip) > 0 {
					return nil, fmt.Errorf("stage %s: %s is a file; \"skip\" is for a directory", m.Name, p)
				}
				continue
			}
			if src.Sha256 != nil || src.Type != nil || src.Strip != 0 {
				return nil, fmt.Errorf("stage %s: %s is a directory, whose files are hashed as the build takes them; \"sha256\", \"type\" and \"strip\" don't apply", m.Name, p)
			}

			start := ctx.Now()
			snap, err := TakeSnapshot(evalFull, src.Skip)
			if err != nil {
				return nil, err
			}
			ctx.Timed(start, "take %d files from %s", snap.Files, evalFull)
			if ctx.Verbose {
				ctx.Note("module %s: %d files from %s (sha256 %s)", m.Name, snap.Files, evalFull, snap.Hash)
			}
			s.entries = append(s.entries, snapshotEntry{
				module:   m.Name,
				index:    i,
				snapshot: *snap,
			})
		}
	}
	return s, nil
}

func (s *Snapshots) recheckGraph(ctx *context.Context) error {
	if err := s.Recheck(ctx, ""); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(s.units)) {
		if err := s.units[name].Recheck(ctx, ""); err != nil {
			return fmt.Errorf("build %s: %w", name, err)
		}
	}
	return nil
}

func (s *Snapshots) Get(module string, index int) *Snapshot {
	for i := range s.entries {
		if s.entries[i].index == index && s.entries[i].module == module {
			return &s.entries[i].snapshot
		}
	}
	return nil
}

func (s *Snapshots) Recheck(ctx *context.Context, module string) error {
	for _, e := range s.entries {
		if module != "" && e.module != module {
			continue
		}
		same, err := isSnapshotUnchanged(e.snapshot)
		if err != nil || !same {
			return fmt.Errorf("module %s: files in %s changed while quasidock took them; build again", e.module, e.snapshot.Dir)
		}
	}
	return nil
}

func isSnapshotUnchanged(s Snapshot) (bool, error) {
	if s.Context != nil {
		err := s.Context.Recheck()
		return err == nil, err
	}
	nodes := s.Tree.Sorted()
	for _, node := range nodes {
		if node.Content.Kind == tree.ContentFile && node.Content.FilePath != "" {
			stat, err := os.Stat(node.Content.FilePath)
			if err != nil {
				return false, nil
			}
			if stat.Size() != node.Content.FileSize {
				return false, nil
			}
		}
	}
	_, hex, err := s.Tree.ContentHash()
	if err != nil {
		return false, err
	}
	return hex == s.Hash, nil
}

func TakeSnapshot(dir string, skip []string) (*Snapshot, error) {
	w := &walkState{
		skip: skip,
		tree: tree.New(),
	}

	topRel := ""
	top := repositoryTop(dir)
	if top != "" {
		at := top
		relPart := strings.Trim(dir[len(top):], `/\`)
		if len(relPart) > 0 {
			parts := strings.FieldsFunc(relPart, func(r rune) bool {
				return r == '/' || r == '\\'
			})
			for _, part := range parts {
				w.pushGitignore(at, topRel)
				topRel = joinSlash(topRel, part)
				if w.rules.Ignored(topRel, true) {
					return nil, fmt.Errorf("%s is ignored by its repository's .gitignore, so git would commit none of it", dir)
				}
				at = filepath.Join(at, part)
			}
		}
	}

	if err := w.walk(dir, "", topRel); err != nil {
		return nil, err
	}
	if w.files == 0 {
		return nil, fmt.Errorf("%s has no files that git would commit", dir)
	}

	_, hex, err := w.tree.ContentHash()
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		Dir:   dir,
		Tree:  w.tree,
		Hash:  hex,
		Files: w.files,
	}, nil
}

func repositoryTop(dir string) string {
	at := dir
	for {
		if store.Exists(filepath.Join(at, ".git")) {
			return at
		}
		parent := filepath.Dir(at)
		if parent == at || parent == "" {
			return ""
		}
		at = parent
	}
}

type walkState struct {
	skip  []string
	rules GitignoreRules
	tree  *tree.Tree
	files int
}

func (w *walkState) pushGitignore(dir, topRel string) bool {
	giPath := filepath.Join(dir, ".gitignore")
	data, err := os.ReadFile(giPath)
	if err != nil {
		return false
	}
	patterns := ParseGitignore(data)
	w.rules.Push(topRel, patterns)
	return true
}

func (w *walkState) walk(path, rel, topRel string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	pushed := w.pushGitignore(path, topRel)
	defer func() {
		if pushed {
			w.rules.Pop()
		}
	}()

	for _, entry := range entries {
		name := entry.Name()
		if strings.EqualFold(name, ".git") {
			continue
		}
		entryRel := joinSlash(rel, name)
		entryTop := joinSlash(topRel, name)
		isDir := entry.IsDir()

		if w.rules.Ignored(entryTop, isDir) || recipe.CleanupMatches(w.skip, entryRel) {
			continue
		}

		full := filepath.Join(path, name)
		if entry.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			// Windows junctions are ModeIrregular since Go 1.23. Readlink
			// distinguishes them from other reparse points, such as cloud files.
			_, linkErr := os.Readlink(full)
			if entry.Type()&os.ModeSymlink != 0 || linkErr == nil {
				return fmt.Errorf("%s is a link, which an image can't hold; leave it out with the source's \"skip\"", full)
			}
		}
		if isDir {
			if store.Exists(filepath.Join(full, ".git")) {
				oldRules := w.rules
				w.rules = GitignoreRules{}
				err := w.walk(full, entryRel, "")
				w.rules = oldRules
				if err != nil {
					return err
				}
			} else {
				if err := w.walk(full, entryRel, entryTop); err != nil {
					return err
				}
			}
		} else {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if (info.Mode() & os.ModeSymlink) != 0 {
				return fmt.Errorf("%s is a link, which an image can't hold; leave it out with the source's \"skip\"", full)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file, which an image can't hold; leave it out with the source's \"skip\"", full)
			}
			if err := w.tree.AddFile(entryRel, tree.Content{
				Kind:     tree.ContentFile,
				FilePath: full,
				FileSize: info.Size(),
			}); err != nil {
				return fmt.Errorf("%s: %w", full, err)
			}
			w.files++
		}
	}
	return nil
}

func joinSlash(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}
