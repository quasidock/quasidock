package tree

import (
	"fmt"
	"strings"
)

// NativeExecutable preserves an existing .exe suffix or supplies one.
func NativeExecutable(logical string) string {
	if strings.HasSuffix(strings.ToLower(logical), ".exe") {
		return logical
	}
	return logical + ".exe"
}

// ExecutableView exposes declared executable files under their logical names.
// Physical .exe paths are storage names, rather than duplicate COPY candidates.
func (source *Tree) ExecutableView(executables []string) (*Tree, error) {
	if len(executables) == 0 {
		return source, nil
	}
	aliases := map[string]string{}
	for _, logical := range executables {
		native := NativeExecutable(logical)
		key := strings.ToLower(native)
		if previous, exists := aliases[key]; exists && previous != logical {
			return nil, fmt.Errorf("ambiguous executable mappings %q and %q", previous, logical)
		}
		aliases[key] = logical
		if !strings.EqualFold(logical, native) {
			if _, exists := source.nodes[strings.ToLower(logical)]; exists {
				return nil, fmt.Errorf("literal output %q collides with mapped executable %q", logical, native)
			}
		}
		if node, exists := source.nodes[key]; exists && node.Content.Kind == ContentDir {
			return nil, fmt.Errorf("mapped executable %q is a directory", native)
		}
	}
	view := New()
	for _, node := range source.Sorted() {
		name := node.Path
		if logical, mapped := aliases[strings.ToLower(name)]; mapped {
			name = logical
		}
		var err error
		if node.Content.Kind == ContentDir {
			err = view.AddDir(name)
		} else {
			err = view.AddFile(name, node.Content)
		}
		if err != nil {
			return nil, fmt.Errorf("executable output mapping: %w", err)
		}
	}
	return view, nil
}

// CopyExecutables selects logical paths and translates only selected executable
// destinations. The returned map records logical-to-native destination paths.
func (source *Tree) CopyExecutables(paths []string, dest string, directory bool, executables []string) (*Tree, map[string]string, error) {
	view, err := source.ExecutableView(executables)
	if err != nil {
		return nil, nil, err
	}
	known := map[string]bool{}
	for _, logical := range executables {
		known[strings.ToLower(logical)] = true
	}
	return view.copySelected(paths, dest, directory, known)
}

// CheckExecutableCollisions rejects literal files occupying a substituted
// executable's namespace. Mapped copies of that same logical name are allowed.
func (source *Tree) CheckExecutableCollisions(executables []string, mapped map[string]string) error {
	for _, logical := range executables {
		native := NativeExecutable(logical)
		if strings.EqualFold(logical, native) {
			continue
		}
		for _, candidate := range []string{logical, native} {
			if node, exists := source.nodes[strings.ToLower(candidate)]; exists {
				allowed := false
				for copiedLogical, copiedNative := range mapped {
					if strings.EqualFold(copiedLogical, logical) && strings.EqualFold(copiedNative, native) && strings.EqualFold(node.Path, native) {
						allowed = true
					}
				}
				if !allowed {
					return fmt.Errorf("literal output %q collides with mapped executable %q", node.Path, native)
				}
			}
		}
	}
	return nil
}
