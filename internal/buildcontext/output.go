package buildcontext

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/tree"
)

// ReadOutput validates a completed native output without applying ignore files.
func ReadOutput(root string) (*tree.Tree, error) {
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
		return nil, fmt.Errorf("build output root %s is not a directory", root)
	}
	return readTree(root, "", nil, "build output")
}

// ValidateOutput also checks virtual outputs before any destination is written.
func ValidateOutput(output *tree.Tree) error {
	for _, node := range output.Sorted() {
		if !oci.IsSafeRelPath(node.Path) || strings.ContainsRune(node.Path, '\\') {
			return fmt.Errorf("build output path %q escapes the output root", node.Path)
		}
		for _, part := range strings.Split(node.Path, "/") {
			if !ValidName(part) || part == "." || part == ".." {
				return fmt.Errorf("build output path %q is not a valid Windows filename", node.Path)
			}
		}
	}
	return nil
}
