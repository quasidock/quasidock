// Package buildcontext snapshots Docker build contexts independently of git.
package buildcontext

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/quasidock/quasidock/internal/tree"
)

type Snapshot struct {
	Root, Dockerfile, Exclude, Hash string
	Tree                            *tree.Tree
}

func Read(root, dockerfile, exclude string) (*Snapshot, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := noLink(root); err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("context root %s is not a directory", root)
	}
	ignoreName := filepath.Join(root, filepath.FromSlash(dockerfile)) + ".dockerignore"
	data, err := os.ReadFile(ignoreName)
	if os.IsNotExist(err) {
		ignoreName = filepath.Join(root, ".dockerignore")
		data, err = os.ReadFile(ignoreName)
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	patterns, err := ignorefile.ReadAll(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ignoreName, err)
	}
	t, err := readTree(root, exclude, matcher, "context")
	if err != nil {
		return nil, err
	}
	_, content, err := t.ContentHash()
	if err != nil {
		return nil, err
	}
	ignoreRel, _ := filepath.Rel(root, ignoreName)
	h := sha256.Sum256(append([]byte(content+"\n"+filepath.ToSlash(ignoreRel)+"\n"), data...))
	return &Snapshot{Root: root, Dockerfile: dockerfile, Exclude: exclude, Tree: t, Hash: fmt.Sprintf("%x", h)}, nil
}

func noLink(full string) error {
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		if _, err := os.Readlink(full); err == nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %s is a link or junction", full)
		}
	}
	return nil
}

func ValidName(name string) bool {
	if name == "" || strings.ContainsAny(name, "<>:\"\\|?*\x00") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	for _, r := range name {
		if r < 32 {
			return false
		}
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || stem == "CONIN$" || stem == "CONOUT$" {
		return false
	}
	runes := []rune(stem)
	return !(len(runes) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && strings.ContainsRune("123456789¹²³", runes[3]))
}

func (s *Snapshot) Recheck() error {
	now, err := Read(s.Root, s.Dockerfile, s.Exclude)
	if err != nil {
		return err
	}
	if now.Hash != s.Hash {
		return fmt.Errorf("context %s changed while building; build again", s.Root)
	}
	return nil
}

// Copy evaluates selectors against the original context, not previous COPY output.
func (s *Snapshot) Copy(paths []string, dest string, directory bool) (*tree.Tree, error) {
	copied, err := s.Tree.Copy(paths, dest, directory)
	var missing *tree.MissingCopySource
	if errors.As(err, &missing) {
		return nil, fmt.Errorf("COPY source %q is missing or excluded by .dockerignore", missing.Path)
	}
	return copied, err
}

func readTree(root, exclude string, matcher *patternmatcher.PatternMatcher, scope string) (*tree.Tree, error) {
	t := tree.New()
	seen := map[string]string{}
	err := filepath.WalkDir(root, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if full == root {
			return nil
		}
		if strings.EqualFold(full, exclude) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, full)
		rel = filepath.ToSlash(rel)
		ignored := false
		if matcher != nil {
			var err error
			ignored, err = matcher.MatchesOrParentMatches(rel)
			if err != nil {
				return err
			}
		}
		// Do not prune excluded directories: a later ! rule can include a child.
		if ignored {
			if entry.IsDir() && noLink(full) != nil {
				return fs.SkipDir
			}
			return nil
		}
		if err := noLink(full); err != nil {
			return err
		}
		for _, part := range strings.Split(rel, "/") {
			if !ValidName(part) {
				return fmt.Errorf("%s path %q is not a valid Windows filename", scope, rel)
			}
		}
		key := strings.ToLower(rel)
		if old, ok := seen[key]; ok && old != rel {
			return fmt.Errorf("%s paths %q and %q collide on Windows", scope, old, rel)
		}
		seen[key] = rel
		if entry.IsDir() {
			return t.AddDir(rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s path %s is not a regular file", scope, full)
		}
		return t.AddFile(rel, tree.Content{Kind: tree.ContentFile, FilePath: full, FileSize: info.Size()})
	})
	if err != nil {
		return nil, err
	}

	return t, nil
}
