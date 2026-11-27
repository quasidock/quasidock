package run

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/oci"
)

func defaultWorkdir(p oci.Placeholders, imageDir, hostCwd string, cwdGranted bool) string {
	if imageDir != "" {
		return imageWorkdir(p, imageDir)
	}
	if cwdGranted {
		return hostCwd
	}
	return environment.NewProfile(p.Data).Home
}

func resolveWorkdir(p oci.Placeholders, base, override string) (string, error) {
	dir := p.Expand(override)
	if strings.ContainsRune(dir, 0) {
		return "", errors.New("directory contains NUL")
	}
	if filepath.IsAbs(dir) {
		return dir, nil
	}
	if filepath.VolumeName(dir) != "" || strings.HasPrefix(dir, `\`) || strings.HasPrefix(dir, "/") {
		return "", errors.New("expected an absolute or unambiguous relative path")
	}
	return filepath.Join(base, dir), nil
}

func checkWorkdir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", dir)
	}
	return nil
}
