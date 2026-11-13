package shim

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/zigsaw-io/zigsaw/internal/process"
	"github.com/zigsaw-io/zigsaw/internal/sidecar"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

// Tail holds circular buffer of the last capacity bytes.
type Tail struct {
	capacity int
	buf      []byte
	total    int
}

func NewTail(capacity int) *Tail {
	return &Tail{
		capacity: capacity,
		buf:      make([]byte, capacity),
	}
}

func (t *Tail) Append(b []byte) {
	for _, c := range b {
		t.buf[t.total%t.capacity] = c
		t.total++
	}
}

func (t *Tail) Text() string {
	if t.total <= t.capacity {
		return string(t.buf[:t.total])
	}
	out := make([]byte, t.capacity)
	start := t.total % t.capacity
	copy(out[:t.capacity-start], t.buf[start:])
	copy(out[t.capacity-start:], t.buf[:start])
	idx := bytes.IndexByte(out, '\n')
	if idx == -1 {
		return string(out)
	}
	return string(out[idx+1:])
}

func LastLines(text string, n int) string {
	trimmed := strings.TrimRight(text, " \t\r\n")
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) <= n {
		return trimmed
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

var (
	ErrSeveralArguments = errors.New("several arguments")
	ErrOtherScheme      = errors.New("other scheme")
)

func SplitArguments(args string) ([]string, error) {
	if strings.TrimSpace(args) == "" {
		return nil, nil
	}
	argv, err := win32.CommandLineToArgvW("x " + args)
	if err != nil {
		return nil, err
	}
	if len(argv) > 1 {
		return argv[1:], nil
	}
	return nil, nil
}

func OneUrl(args string, schemes string) (string, error) {
	argv, err := SplitArguments(args)
	if err != nil {
		return "", err
	}
	if len(argv) == 0 {
		return "", ErrOtherScheme
	}
	if len(argv) > 1 {
		return "", ErrSeveralArguments
	}
	url := argv[0]
	if !sidecar.HasScheme(schemes, url) {
		return "", ErrOtherScheme
	}
	return url, nil
}

func UrlOrReport(args string, schemes string, ownLog *Tail) (string, error) {
	url, err := OneUrl(args, schemes)
	if err != nil {
		var msg string
		if errors.Is(err, ErrSeveralArguments) {
			msg = fmt.Sprintf("a link is one argument, and this came as several, so it wasn't opened:%s", args)
		} else {
			msg = fmt.Sprintf("this opens links of %s, and this isn't one, so it wasn't opened:%s", schemes, args)
		}
		if ownLog != nil {
			ownLog.Append([]byte(msg + "\n"))
		}
		_, _ = fmt.Fprintln(os.Stderr, msg)
		return "", err
	}
	return url, nil
}

func AppendFiles(b *strings.Builder, args string) {
	argv, err := SplitArguments(args)
	if err != nil {
		return
	}
	for _, arg := range argv {
		if arg == "" {
			continue
		}
		b.WriteByte(' ')
		process.AppendQuoted(b, fmt.Sprintf("--file=%s", arg))
	}
}

func AppendKept(b *strings.Builder, args string, drop []string) {
	argv, err := SplitArguments(args)
	if err != nil {
		return
	}
	for _, arg := range argv {
		dropped := false
		for _, d := range drop {
			if arg == d {
				dropped = true
				break
			}
		}
		if dropped {
			continue
		}
		b.WriteByte(' ')
		process.AppendQuoted(b, arg)
	}
}

func AppendArguments(b *strings.Builder, args string) {
	if len(args) > 0 && args[0] != ' ' && args[0] != '\t' {
		b.WriteByte(' ')
	}
	b.WriteString(args)
}

func ArgumentsAsTyped(commandLine string) string {
	end := ProgramNameEnd(commandLine)
	if end >= len(commandLine) {
		return ""
	}
	return commandLine[end:]
}

func ProgramNameEnd(commandLine string) int {
	if len(commandLine) > 0 && commandLine[0] == '"' {
		closeIdx := strings.IndexByte(commandLine[1:], '"')
		if closeIdx == -1 {
			return len(commandLine)
		}
		return closeIdx + 2
	}
	idx := strings.IndexAny(commandLine, " \t")
	if idx == -1 {
		return len(commandLine)
	}
	return idx
}

func SelfName(self string) string {
	base := filepath.Base(self)
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext)
}

func HasStderr() bool {
	h := win32.GetStdHandle(win32.STD_ERROR_HANDLE)
	if h == 0 || h == win32.INVALID_HANDLE_VALUE {
		return false
	}
	kind := win32.GetFileType(h)
	return kind == win32.FILE_TYPE_DISK || kind == win32.FILE_TYPE_PIPE
}

func Report(title, message string) {
	if reportPath := os.Getenv("ZIGSAW_SHIM_REPORT"); reportPath != "" {
		_ = os.WriteFile(reportPath, []byte(fmt.Sprintf("%s\n%s\n", title, message)), 0644)
		return
	}
	win32.MessageBox(0, message, title, win32.MB_OK|win32.MB_ICONERROR)
}

func RunAlias(alias *sidecar.Alias, args string) uint32 {
	var b strings.Builder
	b.WriteString(alias.CommandLine)
	if len(alias.Drop) == 0 {
		AppendArguments(&b, args)
	} else {
		AppendKept(&b, args, alias.Drop)
	}
	spec := process.SpawnSpec{
		Exe:         alias.Exe,
		CommandLine: b.String(),
	}
	code, err := process.Spawn(spec)
	if err != nil {
		return 1
	}
	return code
}

func RunReporting(spec process.SpawnSpec, sc *sidecar.Sidecar) uint32 {
	read, write, err := win32.CreatePipe(nil, 0)
	if err != nil {
		return 1
	}
	defer win32.CloseHandle(read)

	detached := spec
	detached.CreationFlags = win32.DETACHED_PROCESS
	detached.Stdio = &[3]win32.HANDLE{0, write, write}

	child, err := process.Start(detached)
	win32.CloseHandle(write)
	if err != nil {
		return 1
	}

	tail := NewTail(8192)
	buf := make([]byte, 4096)
	for {
		n, err := win32.ReadFile(read, buf)
		if err != nil || n == 0 {
			break
		}
		tail.Append(buf[:n])
	}

	code, err := child.Wait()
	if err != nil {
		code = 1
	}
	if code != 0 {
		output := LastLines(tail.Text(), 20)
		var exit string
		if code >= 0xC0000000 {
			exit = fmt.Sprintf("0x%08X", code)
		} else {
			exit = fmt.Sprintf("%d", code)
		}
		sep := ""
		if len(output) > 0 {
			sep = "\n\n"
		}
		Report(sc.Command, fmt.Sprintf("%s (%s) exited with code %s.%s%s", sc.Command, sc.App, exit, sep, output))
	}
	return code
}

// Run is the entrypoint for zigsaw-shim and zigsaw-shimw.
func Run(gui bool) uint32 {
	reportFailures := gui && !HasStderr()
	ownLog := NewTail(8192)

	logErr := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if gui {
			ownLog.Append([]byte(msg + "\n"))
		}
		_, _ = fmt.Fprintln(os.Stderr, msg)
	}

	self, err := win32.GetModuleFileName()
	if err != nil {
		logErr("zigsaw shim: can't get self path: %v", err)
		if reportFailures {
			Report("zigsaw shim", ownLog.Text())
		}
		return 1
	}

	sidecarPath := strings.TrimSuffix(self, filepath.Ext(self)) + sidecar.Extension
	bytes, err := os.ReadFile(sidecarPath)
	if err != nil {
		logErr("zigsaw shim: can't read %s: %v", sidecarPath, err)
		if reportFailures {
			Report(SelfName(self), ownLog.Text())
		}
		return 1
	}

	rawCommandLine := win32.UTF16PtrToString(win32.GetCommandLineW())
	args := ArgumentsAsTyped(rawCommandLine)

	sc, err := sidecar.Parse(bytes)
	if err != nil {
		if alias, aerr := sidecar.ParseAlias(bytes); aerr == nil {
			return RunAlias(alias, args)
		}
		logErr("zigsaw shim: %s is missing zigsaw, home, app or command", sidecarPath)
		if reportFailures {
			Report(SelfName(self), ownLog.Text())
		}
		return 1
	}

	_ = win32.SetEnvironmentVariable("ZIGSAW_HOME", sc.Home)

	var startup win32.STARTUPINFOW
	startup.Cb = win32.DWORD(unsafe.Sizeof(startup))
	win32.GetStartupInfoW(&startup)
	if startup.DwFlags&win32.STARTF_TITLEISLINKNAME != 0 && startup.LpTitle != nil {
		shortcut := win32.UTF16PtrToString(startup.LpTitle)
		_ = win32.SetEnvironmentVariable("ZIGSAW_SHORTCUT", shortcut)
	}

	var commandLine strings.Builder
	process.AppendQuoted(&commandLine, sc.Zigsaw)
	commandLine.WriteString(" run ")
	process.AppendQuoted(&commandLine, fmt.Sprintf("--command=%s", sc.Command))
	if sc.Files {
		AppendFiles(&commandLine, args)
	}
	commandLine.WriteByte(' ')
	process.AppendQuoted(&commandLine, sc.App)

	if sc.Schemes != "" {
		url, uerr := UrlOrReport(args, sc.Schemes, ownLog)
		if uerr != nil {
			if reportFailures {
				Report(SelfName(self), ownLog.Text())
			}
			return 1
		}
		commandLine.WriteByte(' ')
		process.AppendQuoted(&commandLine, url)
	} else if !sc.Files {
		AppendArguments(&commandLine, args)
	}

	spec := process.SpawnSpec{
		Exe:         sc.Zigsaw,
		CommandLine: commandLine.String(),
		BreakawayOk: true,
	}

	if !gui {
		code, err := process.Spawn(spec)
		if err != nil {
			return 1
		}
		return code
	}

	if !reportFailures {
		spec.CreationFlags = win32.DETACHED_PROCESS
		code, err := process.Spawn(spec)
		if err != nil {
			return 1
		}
		return code
	}

	return RunReporting(spec, sc)
}
