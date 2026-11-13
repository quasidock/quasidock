package schedule

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/zigsaw-io/zigsaw/internal/acl"
	"github.com/zigsaw-io/zigsaw/internal/context"
	"github.com/zigsaw-io/zigsaw/internal/process"
	"github.com/zigsaw-io/zigsaw/internal/shortcuts"
	"github.com/zigsaw-io/zigsaw/internal/store"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

const defaultBase = "Zigsaw update check"
const notifyProgram = "zigsaw-notify.exe"

func base(ctx *context.Context) string {
	if ctx != nil && ctx.Env != nil && ctx.Env["ZIGSAW_TASK_NAME"] != "" {
		return ctx.Env["ZIGSAW_TASK_NAME"]
	}
	if v := os.Getenv("ZIGSAW_TASK_NAME"); v != "" {
		return v
	}
	return defaultBase
}

func NameFor(nameBase, root string) string {
	trimmed := strings.TrimRight(root, "\\/")
	lower := strings.ToLower(trimmed)
	h := sha256.Sum256([]byte(lower))
	hexDig := hex.EncodeToString(h[:4])
	return fmt.Sprintf("%s %s", nameBase, hexDig)
}

func TaskName(ctx *context.Context) (string, error) {
	localAppData := ""
	if ctx != nil && ctx.Env != nil {
		localAppData = ctx.Env["LOCALAPPDATA"]
	}
	if localAppData == "" {
		localAppData = os.Getenv("LOCALAPPDATA")
	}
	if shortcuts.IsDefaultStore(localAppData, ctx.Store.Root) {
		return base(ctx), nil
	}
	return NameFor(base(ctx), ctx.Store.FullRoot()), nil
}

type ran struct {
	code   uint32
	stdout string
}

func schtasks(args []string) (*ran, error) {
	exe, err := win32.SystemProgram("schtasks.exe")
	if err != nil {
		return nil, err
	}
	nul, err := win32.CreateFile("NUL", win32.GENERIC_WRITE, win32.FILE_SHARE_WRITE, nil, win32.OPEN_EXISTING, 0, 0)
	if err != nil {
		nul = 0
	}
	defer func() {
		if nul != 0 && nul != win32.INVALID_HANDLE_VALUE {
			_ = win32.CloseHandle(nul)
		}
	}()

	cmdLine := process.BuildCommandLine(exe, args)
	stdio := [3]win32.HANDLE{0, 0, nul}
	res, err := process.Output(process.SpawnSpec{
		Exe:           exe,
		CommandLine:   cmdLine,
		CreationFlags: win32.CREATE_NO_WINDOW,
		Stdio:         &stdio,
	}, nil, 60*1000)
	if err != nil {
		return nil, nil
	}
	return &ran{code: res.Code, stdout: string(res.Stdout)}, nil
}

func IsOn(ctx *context.Context) (bool, error) {
	tn, err := TaskName(ctx)
	if err != nil {
		return false, err
	}
	r, err := schtasks([]string{"/query", "/tn", tn})
	if err != nil || r == nil {
		return false, nil
	}
	return r.code == 0, nil
}

func offMarker(ctx *context.Context) string {
	return ctx.Store.Path("updates", "off")
}

func TurnedOff(ctx *context.Context) bool {
	return store.Exists(offMarker(ctx))
}

func TurnOn(ctx *context.Context) error {
	_ = os.Remove(offMarker(ctx))
	if err := register(ctx); err != nil {
		return err
	}
	context.Note("the daily check is on: it tells you when updates are waiting, and installs nothing. `zigsaw updates off` stops it")
	return nil
}

func TurnOff(ctx *context.Context) error {
	marker := offMarker(ctx)
	if err := os.MkdirAll(filepath.Dir(marker), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(marker, []byte("`zigsaw updates on` starts the daily check again.\n"), 0644); err != nil {
		return err
	}
	name, err := TaskName(ctx)
	if err != nil {
		return err
	}
	r, err := schtasks([]string{"/delete", "/tn", name, "/f"})
	if err != nil || r == nil {
		return fmt.Errorf("couldn't run schtasks to remove the task %q", name)
	}
	if r.code != 0 {
		on, _ := IsOn(ctx)
		if on {
			return fmt.Errorf("Windows wouldn't remove the task %q", name)
		}
	}
	context.Note("the daily check is off")
	return nil
}

func Repair(ctx *context.Context) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	selfDir := filepath.Dir(self)
	bin := ctx.Store.Path("bin")
	if strings.EqualFold(selfDir, bin) && !TurnedOff(ctx) {
		if on, _ := IsOn(ctx); on {
			_ = register(ctx)
		}
	}
	tasks, err := ListTasks(ctx)
	if err == nil {
		for _, task := range tasks {
			if task.Gone {
				r, _ := schtasks([]string{"/delete", "/tn", task.Name, "/f"})
				if r != nil {
					st := ""
					if task.Store != nil {
						st = *task.Store
					}
					context.Note("removed the task %q, as its store %s is gone", task.Name, st)
				}
			}
		}
	}
	return nil
}

func register(ctx *context.Context) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	notify := filepath.Join(filepath.Dir(self), notifyProgram)
	if !store.Exists(notify) {
		return fmt.Errorf("%s isn't next to zigsaw, so the daily check has nothing to start", notify)
	}
	name, err := TaskName(ctx)
	if err != nil {
		return err
	}
	sid, err := sidText()
	if err != nil {
		return err
	}
	xml := TaskXML(sid, notify, name, ctx.Store.FullRoot())

	tmp := ctx.Store.TmpPath("task")
	defer func() { _ = os.Remove(tmp) }()

	// schtasks takes XML in UTF-16 with BOM (0xFF, 0xFE)
	u16, err := syscall.UTF16FromString(xml)
	if err != nil {
		return err
	}
	bytes := make([]byte, 2+len(u16)*2)
	bytes[0] = 0xff
	bytes[1] = 0xfe
	for i, w := range u16 {
		bytes[2+i*2] = byte(w)
		bytes[2+i*2+1] = byte(w >> 8)
	}
	if err := os.WriteFile(tmp, bytes, 0644); err != nil {
		return err
	}

	r, err := schtasks([]string{"/create", "/tn", name, "/xml", tmp, "/f"})
	if err != nil || r == nil {
		return fmt.Errorf("couldn't run schtasks to add the task %q", name)
	}
	if r.code != 0 {
		return fmt.Errorf("Windows wouldn't add the task %q, so nothing will check for updates by itself", name)
	}
	return nil
}

func sidText() (string, error) {
	sid, err := acl.CurrentUserSid()
	if err != nil {
		return "", err
	}
	return win32.ConvertSidToStringSid(sid)
}

func escapeXML(s string) string {
	var sb strings.Builder
	for _, c := range s {
		switch c {
		case '&':
			sb.WriteString("&amp;")
		case '<':
			sb.WriteString("&lt;")
		case '>':
			sb.WriteString("&gt;")
		case '"':
			sb.WriteString("&quot;")
		default:
			sb.WriteRune(c)
		}
	}
	return sb.String()
}

func notifyArguments(root, name string) string {
	var sb strings.Builder
	sb.WriteString("--store ")
	process.AppendQuoted(&sb, strings.TrimRight(root, "\\/"))
	sb.WriteString(" --task ")
	process.AppendQuoted(&sb, name)
	return sb.String()
}

func TaskXML(sid, notify, name, root string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Tells you when updates of zigsaw or its apps are waiting. It installs nothing. `+"`zigsaw updates off`"+` removes it.</Description></RegistrationInfo>
  <Triggers>
    <CalendarTrigger>
      <StartBoundary>2020-01-01T12:00:00</StartBoundary>
      <RandomDelay>PT1H</RandomDelay>
      <ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>
    </CalendarTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>true</RunOnlyIfNetworkAvailable>
    <ExecutionTimeLimit>PT10M</ExecutionTimeLimit>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec><Command>%s</Command><Arguments>%s</Arguments></Exec>
  </Actions>
</Task>
`, sid, escapeXML(notify), escapeXML(notifyArguments(root, name)))
}

type Task struct {
	Name  string
	Store *string
	Gone  bool
}

func ListTasks(ctx *context.Context) ([]Task, error) {
	var tasks []Task
	all, err := schtasks([]string{"/query", "/fo", "csv", "/nh"})
	if err != nil || all == nil || all.code != 0 {
		return tasks, nil
	}
	prefix := fmt.Sprintf("\"\\%s", base(ctx))
	lines := strings.Split(all.stdout, "\n")
	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		end := strings.Index(line[1:], "\"")
		if end == -1 {
			continue
		}
		name := line[2 : end+1]
		storePath := storeOfTask(schtasks([]string{"/query", "/tn", name, "/xml"}))
		gone := false
		if storePath != nil {
			gone = !store.Exists(*storePath)
		}
		tasks = append(tasks, Task{Name: name, Store: storePath, Gone: gone})
	}
	return tasks, nil
}

func storeOfTask(r *ran, err error) *string {
	if err != nil || r == nil || r.code != 0 {
		return nil
	}
	return ParseStore(r.stdout)
}

func ParseStore(xml string) *string {
	open := "<Arguments>--store "
	idx := strings.Index(xml, open)
	if idx == -1 {
		return nil
	}
	start := idx + len(open)
	end := strings.Index(xml[start:], " --task ")
	if end == -1 {
		return nil
	}
	text := xml[start : start+end]
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			return nil
		}
	}
	unquoted := strings.ReplaceAll(text, "&quot;", "\"")
	plain := strings.ReplaceAll(unquoted, "&amp;", "&")
	trimmed := strings.Trim(plain, "\"")
	return &trimmed
}
