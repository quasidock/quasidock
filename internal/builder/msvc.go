package builder

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/environment"
	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/store"
)

type MSVCToolchain struct {
	Path         []string
	Vars         []environment.Var
	ToolsVersion string
	SDKVersion   string
}

const vcComponent = "Microsoft.VisualStudio.Component.VC.Tools.x86.x64"

func FindMSVC(ctx *context.Context, scratch string) (*MSVCToolchain, error) {
	programFiles := ""
	if ctx != nil && ctx.Env != nil {
		programFiles = ctx.Env["ProgramFiles(x86)"]
		if programFiles == "" {
			programFiles = ctx.Env["ProgramFiles"]
		}
	}
	if programFiles == "" {
		programFiles = os.Getenv("ProgramFiles(x86)")
		if programFiles == "" {
			programFiles = os.Getenv("ProgramFiles")
		}
	}
	if programFiles == "" {
		programFiles = `C:\Program Files (x86)`
	}

	vswhere := filepath.Join(programFiles, `Microsoft Visual Studio`, `Installer`, `vswhere.exe`)
	if !store.Exists(vswhere) {
		return nil, fmt.Errorf("the recipe builds with MSVC (\"host\": [\"msvc\"]), but Visual Studio isn't installed: there's no %s", vswhere)
	}

	whereOut := filepath.Join(scratch, "vswhere.txt")
	cmdLine := fmt.Sprintf(`"%s" -latest -prerelease -products * -requires %s -property installationPath > "%s"`, vswhere, vcComponent, whereOut)
	if err := runCmd(ctx, scratch, cmdLine); err != nil {
		return nil, err
	}

	whereBytes, err := readUtf16OrUtf8File(whereOut)
	if err != nil {
		return nil, err
	}
	installDir := strings.Trim(whereBytes, " \r\n")
	if installDir == "" {
		return nil, fmt.Errorf("the recipe builds with MSVC, but no Visual Studio installation has the C++ tools (%s)", vcComponent)
	}

	vcvars := filepath.Join(installDir, "VC", "Auxiliary", "Build", "vcvars64.bat")
	if !store.Exists(vcvars) {
		return nil, fmt.Errorf("%s has no %s", installDir, vcvars)
	}

	setOut := filepath.Join(scratch, "vcvars.txt")
	vswhereDir := filepath.Dir(vswhere)
	setCmd := fmt.Sprintf(`set "PATH=%s;%%PATH%%" && set "VSCMD_SKIP_SENDTELEMETRY=1" && "%s" > nul && set > "%s"`, vswhereDir, vcvars, setOut)
	if err := runCmd(ctx, scratch, setCmd); err != nil {
		return nil, err
	}

	setBytes, err := readUtf16OrUtf8File(setOut)
	if err != nil {
		return nil, err
	}

	var hostEnv map[string]string
	if ctx != nil {
		hostEnv = ctx.Env
	}
	tc, err := FromSetOutput(hostEnv, setBytes)
	if err != nil {
		return nil, err
	}
	if tc == nil {
		return nil, fmt.Errorf("%s didn't set up MSVC (no VCToolsVersion)", vcvars)
	}
	return tc, nil
}

func runCmd(ctx *context.Context, cwd, command string) error {
	systemRoot := ""
	if ctx != nil && ctx.Env != nil {
		systemRoot = ctx.Env["SystemRoot"]
	}
	if systemRoot == "" {
		systemRoot = os.Getenv("SystemRoot")
	}
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	cmdExe := filepath.Join(systemRoot, `System32`, `cmd.exe`)
	line := fmt.Sprintf(`"%s" /d /u /s /c "%s"`, cmdExe, command)

	var envBlock []uint16
	if ctx != nil && ctx.Env != nil {
		var vars []environment.Var
		for k, v := range ctx.Env {
			vars = append(vars, environment.Var{Name: k, Value: v})
		}
		b, err := environment.EncodeBlock(vars)
		if err == nil {
			envBlock = b
		}
	}
	code, err := process.Spawn(process.SpawnSpec{
		Exe:         cmdExe,
		CommandLine: line,
		EnvBlock:    envBlock,
		Cwd:         cwd,
	})
	if err != nil {
		return fmt.Errorf("finding MSVC: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("finding MSVC: `%s` exited with code %d", command, code)
	}
	return nil
}

func readUtf16OrUtf8File(p string) (string, error) {
	bytesRead, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return decodeUtf16OrUtf8(bytesRead), nil
}

func decodeUtf16OrUtf8(bytesRead []byte) string {
	utf16LE := (len(bytesRead) >= 2 && bytesRead[1] == 0) || bytes.HasPrefix(bytesRead, []byte("\xff\xfe"))
	if !utf16LE {
		return string(bytesRead)
	}
	start := 0
	if bytes.HasPrefix(bytesRead, []byte("\xff\xfe")) {
		start = 2
	}
	n := (len(bytesRead) - start) / 2
	u16 := make([]uint16, n)
	for i := 0; i < n; i++ {
		u16[i] = binary.LittleEndian.Uint16(bytesRead[start+2*i : start+2*i+2])
	}
	return string(utf16.Decode(u16))
}

func FromSetOutput(host map[string]string, text string) (*MSVCToolchain, error) {
	var path []string
	var vars []environment.Var
	var toolsVersion string
	var sdkVersion string

	lines := strings.Split(text, "\n")
	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		name := line[:eq]
		value := line[eq+1:]

		if strings.EqualFold(name, "PATH") {
			before := ""
			if host != nil {
				before = host["PATH"]
				if before == "" {
					before = host["Path"]
				}
			}
			entries := strings.Split(value, ";")
			for _, entry := range entries {
				if entry == "" {
					continue
				}
				if !hasPathEntry(before, entry) {
					path = append(path, entry)
				}
			}
			continue
		}

		if host != nil {
			if old, ok := host[name]; ok && old == value {
				continue
			}
		}
		vars = append(vars, environment.Var{Name: name, Value: value})
		if strings.EqualFold(name, "VCToolsVersion") {
			toolsVersion = strings.TrimRight(value, "\\")
		}
		if strings.EqualFold(name, "WindowsSDKVersion") {
			sdkVersion = strings.TrimRight(value, "\\")
		}
	}

	if toolsVersion == "" {
		return nil, nil
	}
	if sdkVersion == "" {
		sdkVersion = "unknown"
	}
	return &MSVCToolchain{
		Path:         path,
		Vars:         vars,
		ToolsVersion: toolsVersion,
		SDKVersion:   sdkVersion,
	}, nil
}

func hasPathEntry(pathVar, dir string) bool {
	want := strings.TrimRight(dir, "\\")
	for _, entry := range strings.Split(pathVar, ";") {
		if strings.EqualFold(strings.TrimRight(entry, "\\"), want) {
			return true
		}
	}
	return false
}
