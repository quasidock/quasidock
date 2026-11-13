package sidecar

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/pe"
)

var ErrInvalidShimFile = errors.New("invalid shim file")

const Extension = ".shim"

type Sidecar struct {
	Zigsaw  string
	Home    string
	App     string
	Command string
	GUI     bool
	Files   bool
	Schemes string
}

func Parse(data []byte) (*Sidecar, error) {
	var s Sidecar
	var hasZigsaw, hasHome, hasApp, hasCommand bool

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq == -1 {
			return nil, ErrInvalidShimFile
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])

		switch key {
		case "zigsaw":
			s.Zigsaw = val
			hasZigsaw = true
		case "home":
			s.Home = val
			hasHome = true
		case "app":
			s.App = val
			hasApp = true
		case "command":
			s.Command = val
			hasCommand = true
		case "gui":
			s.GUI = (val == "true")
		case "files":
			s.Files = (val == "true")
		case "schemes":
			s.Schemes = val
		}
	}
	if !hasZigsaw || !hasHome || !hasApp || !hasCommand {
		return nil, ErrInvalidShimFile
	}
	return &s, nil
}

func (s *Sidecar) Format(w io.Writer) error {
	fmt.Fprintf(w, "# Written by zigsaw. The .exe next to this file runs:\n#   zigsaw run --command=<command> <app>\nzigsaw = %s\nhome = %s\napp = %s\ncommand = %s\ngui = %t\n\n",
		s.Zigsaw, s.Home, s.App, s.Command, s.GUI)
	if s.Files {
		fmt.Fprintln(w, "files = true")
	}
	if s.Schemes != "" {
		fmt.Fprintf(w, "schemes = %s\n", s.Schemes)
	}
	return nil
}

func (s *Sidecar) Equal(o *Sidecar) bool {
	if o == nil {
		return false
	}
	return s.Zigsaw == o.Zigsaw &&
		s.Home == o.Home &&
		s.App == o.App &&
		s.Command == o.Command &&
		s.GUI == o.GUI &&
		s.Files == o.Files &&
		s.Schemes == o.Schemes
}

type ShimKind int

const (
	ShimConsole ShimKind = iota
	ShimGUI
)

func ShimKindFromExe(path string) ShimKind {
	sub, ok, _ := pe.SubsystemOfFile(path)
	if ok && sub == pe.SubsystemGUI {
		return ShimGUI
	}
	return ShimConsole
}

func ShimExe(zigsawExe string, kind ShimKind) ([]byte, error) {
	name := "zigsaw-shim.exe"
	if kind == ShimGUI {
		name = "zigsaw-shimw.exe"
	}
	dir := filepath.Dir(zigsawExe)
	path := filepath.Join(dir, name)
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s, which should be installed next to zigsaw.exe: %w", path, err)
	}
	return bytes, nil
}

func WriteShimIn(bin, name string, shimExe []byte, sc Sidecar) error {
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	exePath := filepath.Join(bin, name+".exe")
	current, _ := os.ReadFile(exePath)
	if current == nil || !bytes.Equal(current, shimExe) {
		if err := os.WriteFile(exePath, shimExe, 0o755); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	if err := sc.Format(&buf); err != nil {
		return err
	}
	sidecarPath := filepath.Join(bin, name+Extension)
	return os.WriteFile(sidecarPath, buf.Bytes(), 0o644)
}

func HasScheme(schemes, url string) bool {
	colon := strings.IndexByte(url, ':')
	if colon == -1 {
		return false
	}
	prefix := url[:colon]
	for _, scheme := range strings.Split(schemes, ",") {
		scheme = strings.TrimSpace(scheme)
		if scheme != "" && strings.EqualFold(scheme, prefix) {
			return true
		}
	}
	return false
}

type Alias struct {
	Exe         string
	CommandLine string
	Drop        []string
}

func ParseAlias(data []byte) (*Alias, error) {
	var a Alias
	var hasExe, hasCmd bool

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq == -1 {
			return nil, ErrInvalidShimFile
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])

		switch key {
		case "exe":
			a.Exe = val
			hasExe = true
		case "command_line":
			a.CommandLine = val
			hasCmd = true
		case "drop":
			a.Drop = append(a.Drop, val)
		}
	}
	if !hasExe || !hasCmd {
		return nil, ErrInvalidShimFile
	}
	return &a, nil
}

func (a *Alias) Format(w io.Writer) error {
	fmt.Fprintf(w, "# Written by zigsaw for an alias. The .exe next to this file runs\n# command_line, then the caller's arguments, less any it drops.\nexe = %s\ncommand_line = %s\n\n",
		a.Exe, a.CommandLine)
	for _, d := range a.Drop {
		fmt.Fprintf(w, "drop = %s\n", d)
	}
	return nil
}

func (a *Alias) Equal(o *Alias) bool {
	if o == nil {
		return false
	}
	if a.Exe != o.Exe || a.CommandLine != o.CommandLine || len(a.Drop) != len(o.Drop) {
		return false
	}
	for i := range a.Drop {
		if a.Drop[i] != o.Drop[i] {
			return false
		}
	}
	return true
}
