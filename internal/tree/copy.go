package tree

import (
	"fmt"
	"path"
	"strings"
)

// MissingCopySource reports a selector with no matches.
type MissingCopySource struct{ Path string }

func (e *MissingCopySource) Error() string { return fmt.Sprintf("COPY source %q is missing", e.Path) }

// Copy selects files and directory contents from this tree using ordinary globs.
// The source tree is unchanged; directory destinations preserve file basenames.
func (source *Tree) Copy(paths []string, dest string, directory bool) (*Tree, error) {
	copied, _, err := source.copySelected(paths, dest, directory, nil)
	return copied, err
}

func (source *Tree) copySelected(paths []string, dest string, directory bool, executables map[string]bool) (*Tree, map[string]string, error) {
	t := New()
	outputs := map[string]string{}
	copyFile := func(node Node, target string) error {
		if executables[strings.ToLower(node.Path)] {
			outputs[target] = NativeExecutable(target)
			target = NativeExecutable(target)
		}
		return t.AddFile(target, node.Content)
	}
	nodes := source.Sorted()
	var roots []Node
	for _, pattern := range paths {
		pattern = path.Clean(pattern)
		if pattern == "." {
			roots = append(roots, Node{Path: ".", Content: Content{Kind: ContentDir}})
			continue
		}
		found := false
		for _, node := range nodes {
			matched, err := path.Match(pattern, node.Path)
			if err != nil {
				return nil, nil, err
			}
			if matched {
				roots = append(roots, node)
				found = true
			}
		}
		if !found {
			return nil, nil, &MissingCopySource{Path: pattern}
		}
	}
	if len(roots) > 1 && !strings.HasSuffix(dest, "/") {
		return nil, nil, fmt.Errorf("COPY with multiple sources requires a destination ending in '/'")
	}
	directory = directory || strings.HasSuffix(dest, "/")
	for _, root := range roots {
		if root.Content.Kind != ContentDir {
			target := dest
			if directory {
				target = path.Join(dest, path.Base(root.Path))
			}
			if err := copyFile(root, target); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err := t.AddDir(dest); err != nil {
			return nil, nil, err
		}
		for _, node := range nodes {
			name := node.Path
			if root.Path != "." {
				if !strings.HasPrefix(name, root.Path+"/") {
					continue
				}
				name = strings.TrimPrefix(name, root.Path+"/")
			}
			target := path.Join(dest, name)
			var err error
			if node.Content.Kind == ContentDir {
				err = t.AddDir(target)
			} else {
				err = copyFile(node, target)
			}
			if err != nil {
				return nil, nil, err
			}
		}
	}
	return t, outputs, nil
}
