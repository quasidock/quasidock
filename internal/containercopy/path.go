// Package containercopy copies regular files and directories through a
// container's data-relative namespace, including its declared storage mounts.
package containercopy

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/mounts"
)

type endpoint struct {
	container, name             string
	stream, contents, directory bool
}

func parseEndpoint(raw string) (endpoint, error) {
	e := endpoint{}
	if raw == "" {
		return e, errors.New("copy path cannot be empty")
	}
	if raw == "-" {
		e.stream = true
		return e, nil
	}
	s := strings.ReplaceAll(raw, `\`, "/")
	drive := len(s) >= 2 && s[1] == ':' && (s[0] >= 'A' && s[0] <= 'Z' || s[0] >= 'a' && s[0] <= 'z')
	if i := strings.IndexByte(s, ':'); i >= 0 && !drive && !strings.Contains(s[:i], "/") {
		e.container, s = s[:i], s[i+1:]
		if e.container == "" || s == "" || strings.HasPrefix(s, "//") {
			return e, fmt.Errorf("invalid container copy path %q", raw)
		}
	}
	e.contents = s == "." || strings.HasSuffix(s, "/.")
	e.directory = e.contents || strings.HasSuffix(s, "/")
	if e.container == "" {
		if strings.HasPrefix(s, "//?/") || strings.HasPrefix(s, "//./") {
			return e, errors.New("Windows device namespaces are not supported")
		}
		// Validate the original spelling before GetFullPathName can normalize
		// trailing spaces/dots. Host . and .. retain their usual meaning.
		rawPath := filepath.FromSlash(s)
		rawPath = strings.TrimPrefix(rawPath, filepath.VolumeName(rawPath))
		for _, part := range strings.Split(filepath.ToSlash(rawPath), "/") {
			if part == "" || part == "." || part == ".." {
				continue
			}
			if err := mounts.ValidateComponent(part); err != nil {
				return e, err
			}
		}
		abs, err := filepath.Abs(filepath.FromSlash(s))
		if err != nil {
			return e, err
		}
		vol := filepath.VolumeName(abs)
		rel := strings.TrimLeft(strings.TrimPrefix(abs, vol), `\`)
		if _, err := relativePath(filepath.ToSlash(rel)); err != nil {
			return e, err
		}
		e.name = filepath.Clean(abs)
		if rel == "" {
			e.contents = true
		}
		return e, nil
	}
	var err error
	e.name, err = relativePath(strings.TrimPrefix(s, "/"))
	if e.name == "" {
		e.contents = true
	}
	return e, err
}

// relativePath rejects traversal before cleaning, so a/../b cannot hide it.
func relativePath(s string) (string, error) {
	s = strings.ReplaceAll(s, `\`, "/")
	if strings.HasPrefix(s, "/") {
		return "", fmt.Errorf("absolute path is not allowed: %q", s)
	}
	var parts []string
	for _, p := range strings.Split(s, "/") {
		if p == "" || p == "." {
			continue
		}
		if err := mounts.ValidateComponent(p); err != nil {
			return "", err
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "/"), nil
}

func below(name, root string) bool {
	if root == "" {
		return true
	}
	a, b := strings.Split(name, "/"), strings.Split(root, "/")
	if len(a) < len(b) {
		return false
	}
	for i := range b {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

func relativeTo(name, root string) string {
	if root == "" {
		return name
	}
	return strings.Join(strings.Split(name, "/")[len(strings.Split(root, "/")):], "/")
}

func join(a, b string) string {
	s := path.Join(a, b)
	if s == "." {
		return ""
	}
	return s
}
