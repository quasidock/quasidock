package aliases

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/process"
	"github.com/zigsaw-io/zigsaw/internal/sidecar"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

type Provider struct {
	ID       string
	Dir      string
	Aliases  map[string]oci.Export
	Runtimes []oci.DirPlaceholder
}

type Resolved struct {
	Name  string
	Alias sidecar.Alias
}

type Missing int

const (
	MissingFail Missing = iota
	MissingSkip
)

func Resolve(st *store.Store, providers []Provider, missing Missing, isTest bool) ([]Resolved, error) {
	var out []Resolved

	for _, pr := range providers {
		if pr.Aliases == nil {
			continue
		}
		for name, e := range pr.Aliases {
			if find(out, name) != nil {
				continue
			}

			p := oci.Placeholders{
				App:      pr.Dir,
				Data:     "${data}",
				Runtimes: pr.Runtimes,
			}
			exe := p.CommandPath(e.Command)

			if _, err := os.Stat(exe); err != nil {
				switch missing {
				case MissingFail:
					return nil, fmt.Errorf("%s's alias %s runs %s, which isn't in it", pr.ID, name, e.Command)
				case MissingSkip:
					if !isTest {
						fmt.Fprintf(os.Stderr, "warning: %s's alias %s runs %s, which isn't in it; leaving it out\n", pr.ID, name, e.Command)
					}
					continue
				}
			}

			args := make([]string, len(e.Args))
			for i, template := range e.Args {
				args[i] = p.Expand(template)
			}

			cmdLine := process.BuildCommandLine(exe, args)
			out = append(out, Resolved{
				Name: name,
				Alias: sidecar.Alias{
					Exe:         exe,
					CommandLine: cmdLine,
					Drop:        e.Drop,
				},
			})
		}
	}
	return out, nil
}

func find(resolved []Resolved, name string) *Resolved {
	for i := range resolved {
		if strings.EqualFold(resolved[i].Name, name) {
			return &resolved[i]
		}
	}
	return nil
}

func Names(resolved []Resolved) string {
	var names []string
	for _, r := range resolved {
		names = append(names, r.Name)
	}
	return strings.Join(names, " ")
}

func WriteAll(dir string, resolved []Resolved, shimExe []byte) error {
	if len(resolved) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	for _, r := range resolved {
		exePath := filepath.Join(dir, r.Name+".exe")
		if err := os.WriteFile(exePath, shimExe, 0755); err != nil {
			return err
		}

		var buf bytes.Buffer
		_ = r.Alias.Format(&buf)
		shimP := filepath.Join(dir, r.Name+sidecar.Extension)
		if err := os.WriteFile(shimP, buf.Bytes(), 0644); err != nil {
			return err
		}
	}
	return nil
}

func Sync(dir string, resolved []Resolved, shimExe []byte, verbose bool) (int, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return 0, err
	}

	written := 0
	for _, r := range resolved {
		exePath := filepath.Join(dir, r.Name+".exe")
		if len(shimExe) > 0 {
			w, err := ReplaceIfDifferent(exePath, shimExe, verbose)
			if err != nil {
				return written, err
			}
			if w {
				written++
			}
		}

		var buf bytes.Buffer
		_ = r.Alias.Format(&buf)
		sidecarPath := filepath.Join(dir, r.Name+sidecar.Extension)
		w, err := ReplaceIfDifferent(sidecarPath, buf.Bytes(), verbose)
		if err != nil {
			return written, err
		}
		if w {
			written++
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return written, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if IsWanted(resolved, name) {
			continue
		}
		p := filepath.Join(dir, name)
		if err := os.Remove(p); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "couldn't remove %s (%v); a later run will\n", p, err)
		}
	}
	return written, nil
}

func IsWanted(resolved []Resolved, fileName string) bool {
	for _, ext := range []string{".exe", sidecar.Extension} {
		if len(fileName) <= len(ext) || !strings.EqualFold(fileName[len(fileName)-len(ext):], ext) {
			continue
		}
		stem := fileName[:len(fileName)-len(ext)]
		for _, r := range resolved {
			if strings.EqualFold(r.Name, stem) {
				return true
			}
		}
	}
	return false
}

func ReplaceIfDifferent(p string, b []byte, verbose bool) (bool, error) {
	current, err := os.ReadFile(p)
	if err == nil && bytes.Equal(current, b) {
		return false, nil
	}

	var random [4]byte
	_, _ = rand.Read(random[:])
	tmp := fmt.Sprintf("%s.%s.tmp", p, hex.EncodeToString(random[:]))
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return false, err
	}

	err = store.RenameRetrying(tmp, p, 1000)
	if err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, os.ErrPermission) {
			if verbose {
				fmt.Fprintf(os.Stderr, "%s is in use; a later run will update it\n", p)
			}
			return false, nil
		}
		return false, err
	}
	return true, nil
}
