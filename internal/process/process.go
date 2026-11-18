package process

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"github.com/quasidock/quasidock/internal/win32"
)

var (
	ErrInvalidBatchScript   = errors.New("invalid batch script")
	ErrInvalidBatchArgument = errors.New("invalid batch argument")
)

// BuildCommandLine quotes arguments the way the Microsoft C runtime (and CommandLineToArgvW)
// parses them back.
func BuildCommandLine(exe string, args []string) string {
	var out strings.Builder
	AppendQuoted(&out, exe)
	for _, arg := range args {
		out.WriteByte(' ')
		AppendQuoted(&out, arg)
	}
	return out.String()
}

func AppendQuoted(out *strings.Builder, arg string) {
	if len(arg) > 0 && !strings.ContainsAny(arg, " \t\n\x0b\"") {
		out.WriteString(arg)
		return
	}
	out.WriteByte('"')
	backslashes := 0
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		switch c {
		case '\\':
			backslashes++
		case '"':
			for k := 0; k < backslashes*2+1; k++ {
				out.WriteByte('\\')
			}
			out.WriteByte('"')
			backslashes = 0
		default:
			for k := 0; k < backslashes; k++ {
				out.WriteByte('\\')
			}
			out.WriteByte(c)
			backslashes = 0
		}
	}
	for k := 0; k < backslashes*2; k++ {
		out.WriteByte('\\')
	}
	out.WriteByte('"')
}

// BuildBatchCommandLine builds a command line that runs a batch file through cmd_exe.
// Modelled on Rust's fix for CVE-2024-24576 ("BatBadBut"):
//
//	"<cmd.exe>" /d /e:ON /v:OFF /c ""<script>" args..."
func BuildBatchCommandLine(cmdExe, script string, args []string) (string, error) {
	if len(script) == 0 || strings.ContainsRune(script, '"') || script[len(script)-1] == '\\' {
		return "", ErrInvalidBatchScript
	}
	var out strings.Builder
	AppendQuoted(&out, cmdExe)
	out.WriteString(" /d /e:ON /v:OFF /c \"\"")
	for i := 0; i < len(script); i++ {
		appendBatchChar(&out, script[i])
	}
	out.WriteByte('"')
	for _, arg := range args {
		out.WriteByte(' ')
		if err := appendBatchArg(&out, arg); err != nil {
			return "", err
		}
	}
	out.WriteByte('"')
	return out.String(), nil
}

func appendBatchArg(out *strings.Builder, arg string) error {
	if strings.ContainsAny(arg, "\r\n\x00") {
		return ErrInvalidBatchArgument
	}
	quote := len(arg) == 0 || arg[len(arg)-1] == '\\' || needsBatchQuotes(arg)
	if quote {
		out.WriteByte('"')
	}
	backslashes := 0
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		if c == '\\' {
			backslashes++
		} else {
			if c == '"' {
				for k := 0; k < backslashes; k++ {
					out.WriteByte('\\')
				}
			}
			backslashes = 0
		}
		appendBatchChar(out, c)
	}
	if quote {
		for k := 0; k < backslashes; k++ {
			out.WriteByte('\\')
		}
		out.WriteByte('"')
	}
	return nil
}

func appendBatchChar(out *strings.Builder, c byte) {
	switch c {
	case '"':
		out.WriteString("\"\"")
	case '%':
		out.WriteString("%%cd:~,%")
	default:
		out.WriteByte(c)
	}
}

func needsBatchQuotes(arg string) bool {
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
		case c == '#' || c == '$' || c == '*' || c == '+' || c == '-' || c == '.' || c == '/' || c == ':' || c == '?' || c == '@' || c == '\\' || c == '_':
		case c >= 0x80:
			if c == 0xc2 && i+1 < len(arg) && arg[i+1] <= 0x9f {
				return true
			}
		default:
			return true
		}
	}
	return false
}

type SpawnSpec struct {
	// ParentJobs lists enclosing jobs, from outermost to innermost.
	ParentJobs []win32.HANDLE
	// PreserveConsoleHandler keeps an already-supervised owner's handler intact.
	PreserveConsoleHandler bool
	Exe                    string
	CommandLine            string
	EnvBlock               []uint16
	Cwd                    string
	Security               *win32.SECURITY_CAPABILITIES
	Token                  win32.HANDLE
	CreationFlags          win32.DWORD
	BreakawayOk            bool
	Stdio                  *[3]win32.HANDLE
	Inherit                []win32.HANDLE
}

// LowIntegrityToken creates a copy of the current process token with low integrity level.
func LowIntegrityToken() (win32.HANDLE, error) {
	var own win32.HANDLE
	access := win32.TOKEN_ASSIGN_PRIMARY | win32.TOKEN_DUPLICATE | win32.TOKEN_QUERY | win32.TOKEN_ADJUST_DEFAULT
	own, err := win32.OpenProcessToken(win32.GetCurrentProcess(), access)
	if err != nil {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer win32.CloseHandle(own)

	low, err := win32.DuplicateTokenEx(own, 0, nil, win32.SecurityImpersonation, win32.TokenPrimary)
	if err != nil {
		return 0, fmt.Errorf("DuplicateTokenEx: %w", err)
	}

	sid, err := win32.ConvertStringSidToSid(win32.LowIntegritySidString)
	if err != nil {
		win32.CloseHandle(low)
		return 0, fmt.Errorf("ConvertStringSidToSid: %w", err)
	}
	defer win32.LocalFree(uintptr(sid))

	label := win32.TOKEN_MANDATORY_LABEL{
		Label: win32.SID_AND_ATTRIBUTES{
			Sid:        sid,
			Attributes: win32.SE_GROUP_INTEGRITY,
		},
	}
	labelSize := win32.DWORD(unsafe.Sizeof(label)) + win32.GetLengthSid(sid)
	if err := win32.SetTokenInformation(low, win32.TokenIntegrityLevel, unsafe.Pointer(&label), labelSize); err != nil {
		win32.CloseHandle(low)
		return 0, fmt.Errorf("SetTokenInformation: %w", err)
	}
	return low, nil
}

// UserEnvironmentBlock gets the environment variables of the current user as a null-terminated UTF-16 block.
func UserEnvironmentBlock() ([]uint16, error) {
	token, err := win32.OpenProcessToken(win32.GetCurrentProcess(), win32.TOKEN_QUERY)
	if err != nil {
		return nil, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer win32.CloseHandle(token)

	blockPtr, err := win32.CreateEnvironmentBlock(token, false)
	if err != nil {
		return nil, fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer win32.DestroyEnvironmentBlock(blockPtr)

	p := (*[1 << 28]uint16)(unsafe.Pointer(blockPtr))
	length := 0
	for p[length] != 0 || p[length+1] != 0 {
		length++
	}
	out := make([]uint16, length+2)
	copy(out, p[:length+2])
	return out, nil
}

type Child struct {
	Job     win32.HANDLE
	Process win32.HANDLE
}

// Close releases the owned process and job handles. It is safe after a wait
// method has already released them, including when Windows reuses their values.
func (c *Child) Close() {
	if c.Process != 0 {
		win32.CloseHandle(c.Process)
		c.Process = 0
	}
	if c.Job != 0 {
		win32.CloseHandle(c.Job)
		c.Job = 0
	}
}

func (c *Child) Wait() (uint32, error) {
	defer c.Close()

	if r := win32.WaitForSingleObject(c.Process, win32.INFINITE); r != win32.WAIT_OBJECT_0 {
		return 0, fmt.Errorf("WaitForSingleObject: %d", r)
	}
	code, err := win32.GetExitCodeProcess(c.Process)
	if err != nil {
		return 0, fmt.Errorf("GetExitCodeProcess: %w", err)
	}
	EndJob(c.Job)
	return uint32(code), nil
}

// WaitOrStop waits for the child process to exit or stopEvent to be signaled.
// When stopEvent fires, it gives the process up to graceMs to exit after a
// break signal before forcefully ending its job object. It returns the exit
// code and true if stopping was triggered by stopEvent.
func (c *Child) WaitOrStop(stopEvent win32.HANDLE, graceMs win32.DWORD) (uint32, bool, error) {
	if stopEvent == 0 {
		code, err := c.Wait()
		return code, false, err
	}
	defer c.Close()

	r := win32.WaitForMultipleObjects([]win32.HANDLE{c.Process, stopEvent}, false, win32.INFINITE)
	switch r {
	case win32.WAIT_OBJECT_0:
		code, err := win32.GetExitCodeProcess(c.Process)
		if err != nil {
			return 0, false, fmt.Errorf("GetExitCodeProcess: %w", err)
		}
		EndJob(c.Job)
		return uint32(code), false, nil
	case win32.WAIT_OBJECT_0 + 1:
		if graceMs > 0 {
			if pid := win32.GetProcessId(c.Process); pid != 0 {
				win32.GenerateConsoleCtrlEvent(win32.CTRL_BREAK_EVENT, pid)
			}
			if win32.WaitForSingleObject(c.Process, graceMs) == win32.WAIT_OBJECT_0 {
				code, err := win32.GetExitCodeProcess(c.Process)
				EndJob(c.Job)
				if err != nil {
					return 0, true, fmt.Errorf("GetExitCodeProcess: %w", err)
				}
				return uint32(code), true, nil
			}
		}
		EndJob(c.Job)
		win32.WaitForSingleObject(c.Process, 5000)
		code, err := win32.GetExitCodeProcess(c.Process)
		if err != nil {
			return 1, true, nil
		}
		return uint32(code), true, nil
	default:
		return 0, false, fmt.Errorf("WaitForMultipleObjects: %d", r)
	}
}

func (c *Child) WaitFor(ms win32.DWORD) (uint32, bool, error) {
	switch win32.WaitForSingleObject(c.Process, ms) {
	case win32.WAIT_OBJECT_0:
		code, err := c.Wait()
		return code, true, err
	case win32.WAIT_TIMEOUT:
		EndJob(c.Job)
		c.Close()
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("WaitForSingleObject error")
	}
}

type OutputResult struct {
	Code   uint32
	Stdout []byte
}

var ignoreCtrlCHandler = syscall.NewCallback(func(event uint32) uintptr {
	if event == win32.CTRL_C_EVENT || event == win32.CTRL_BREAK_EVENT {
		return 1
	}
	return 0
})

func Output(spec SpawnSpec, input []byte, ms win32.DWORD) (*OutputResult, error) {
	const pipeSize = 64 * 1024
	inRead, inWrite, err := win32.CreatePipe(nil, pipeSize)
	if err != nil {
		return nil, err
	}
	outRead, outWrite, err := win32.CreatePipe(nil, pipeSize)
	if err != nil {
		win32.CloseHandle(inRead)
		win32.CloseHandle(inWrite)
		return nil, err
	}
	defer win32.CloseHandle(outRead)

	withPipes := spec
	errHandle := win32.GetStdHandle(win32.STD_ERROR_HANDLE)
	if spec.Stdio != nil && (*spec.Stdio)[2] != 0 {
		errHandle = (*spec.Stdio)[2]
	}
	stdio := [3]win32.HANDLE{inRead, outWrite, errHandle}
	withPipes.Stdio = &stdio

	child, err := Start(withPipes)
	defer win32.SetConsoleCtrlHandler(ignoreCtrlCHandler, false)
	win32.CloseHandle(inRead)
	win32.CloseHandle(outWrite)
	if err != nil {
		win32.CloseHandle(inWrite)
		return nil, err
	}

	var writeErr error
	if len(input) > 0 {
		_, writeErr = win32.WriteFile(inWrite, input)
	}
	win32.CloseHandle(inWrite)

	code, ok, err := child.WaitFor(ms)
	if err != nil || !ok || writeErr != nil {
		return nil, err
	}

	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := win32.ReadFile(outRead, buf)
		if err != nil || n == 0 {
			break
		}
		out.Write(buf[:n])
	}
	return &OutputResult{Code: code, Stdout: out.Bytes()}, nil
}

func Spawn(spec SpawnSpec) (uint32, error) {
	child, err := Start(spec)
	if err != nil {
		return 0, err
	}
	return child.Wait()
}

func Start(spec SpawnSpec) (*Child, error) {
	job, err := NewJobAllowing(spec.BreakawayOk)
	if err != nil {
		return nil, err
	}
	proc, err := StartIn(job, spec)
	if err != nil {
		win32.CloseHandle(job)
		return nil, err
	}
	return &Child{Job: job, Process: proc}, nil
}

func NewJob() (win32.HANDLE, error) {
	return NewJobAllowing(false)
}

func NewJobAllowing(breakaway bool) (win32.HANDLE, error) {
	job, err := win32.CreateJobObject("")
	if err != nil {
		return 0, err
	}
	var limits win32.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = win32.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if breakaway {
		limits.BasicLimitInformation.LimitFlags |= win32.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	}
	if err := win32.SetInformationJobObject(job, win32.JobObjectExtendedLimitInformation, unsafe.Pointer(&limits), win32.DWORD(unsafe.Sizeof(limits))); err != nil {
		win32.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

type Detached struct {
	Process   win32.HANDLE
	PID       uint32
	BrokeAway bool
}

func StartDetached(exe, commandLine, cwd string) (Detached, error) {
	for _, breakaway := range []bool{true, false} {
		var startup win32.STARTUPINFOW
		startup.Cb = win32.DWORD(unsafe.Sizeof(startup))
		// Keep a console for targeted CTRL_BREAK delivery, without opening a
		// visible terminal window. Children continue to use explicit pipes.
		startup.DwFlags = win32.STARTF_USESHOWWINDOW
		startup.WShowWindow = 0 // SW_HIDE
		var info win32.PROCESS_INFORMATION
		flags := win32.CREATE_NEW_CONSOLE | win32.CREATE_UNICODE_ENVIRONMENT
		if breakaway {
			flags |= win32.CREATE_BREAKAWAY_FROM_JOB
		}
		err := win32.CreateProcess(exe, commandLine, nil, nil, false, flags, nil, cwd, unsafe.Pointer(&startup), &info)
		if err != nil {
			if breakaway && errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
				continue
			}
			return Detached{}, err
		}
		win32.CloseHandle(info.HThread)
		return Detached{Process: info.HProcess, PID: uint32(info.DwProcessId), BrokeAway: breakaway}, nil
	}
	return Detached{}, errors.New("unreachable")
}

func StartIn(job win32.HANDLE, spec SpawnSpec) (win32.HANDLE, error) {
	var stdHandles [3]win32.HANDLE
	var inherit []win32.HANDLE

	stdIds := [3]win32.DWORD{win32.STD_INPUT_HANDLE, win32.STD_OUTPUT_HANDLE, win32.STD_ERROR_HANDLE}
	for i, which := range stdIds {
		var h win32.HANDLE
		if spec.Stdio != nil {
			h = (*spec.Stdio)[i]
		} else {
			h = win32.GetStdHandle(which)
		}
		if h == 0 || h == win32.INVALID_HANDLE_VALUE {
			continue
		}
		if !win32.SetHandleInformation(h, win32.HANDLE_FLAG_INHERIT, win32.HANDLE_FLAG_INHERIT) {
			continue
		}
		stdHandles[i] = h
		already := false
		for _, ex := range inherit {
			if ex == h {
				already = true
				break
			}
		}
		if !already {
			inherit = append(inherit, h)
		}
	}

	for _, h := range spec.Inherit {
		if !win32.SetHandleInformation(h, win32.HANDLE_FLAG_INHERIT, win32.HANDLE_FLAG_INHERIT) {
			return 0, fmt.Errorf("SetHandleInformation failed on inherit handle")
		}
		inherit = append(inherit, h)
	}

	inheritCount := win32.DWORD(len(inherit))
	var attrCount win32.DWORD = 1
	if inheritCount > 0 {
		attrCount++
	}
	if spec.Security != nil {
		attrCount++
	}

	var attrSize uintptr
	win32.InitializeProcThreadAttributeList(nil, attrCount, 0, &attrSize)
	attrsBuf := make([]byte, attrSize)
	attrs := unsafe.Pointer(&attrsBuf[0])
	if !win32.InitializeProcThreadAttributeList(attrs, attrCount, 0, &attrSize) {
		return 0, fmt.Errorf("InitializeProcThreadAttributeList failed")
	}
	defer win32.DeleteProcThreadAttributeList(attrs)

	jobs := append(append([]win32.HANDLE(nil), spec.ParentJobs...), job)
	if !win32.UpdateProcThreadAttribute(attrs, 0, win32.PROC_THREAD_ATTRIBUTE_JOB_LIST, unsafe.Pointer(&jobs[0]), uintptr(len(jobs))*unsafe.Sizeof(jobs[0]), nil, nil) {
		return 0, fmt.Errorf("UpdateProcThreadAttribute(JOB_LIST) failed")
	}

	if inheritCount > 0 {
		if !win32.UpdateProcThreadAttribute(attrs, 0, win32.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&inherit[0]), uintptr(inheritCount)*unsafe.Sizeof(inherit[0]), nil, nil) {
			return 0, fmt.Errorf("UpdateProcThreadAttribute(HANDLE_LIST) failed")
		}
	}

	if spec.Security != nil {
		if !win32.UpdateProcThreadAttribute(attrs, 0, win32.PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES, unsafe.Pointer(spec.Security), unsafe.Sizeof(*spec.Security), nil, nil) {
			return 0, fmt.Errorf("UpdateProcThreadAttribute(SECURITY_CAPABILITIES) failed")
		}
	}

	var startup win32.STARTUPINFOEXW
	startup.StartupInfo.Cb = win32.DWORD(unsafe.Sizeof(startup))
	startup.LpAttributeList = uintptr(attrs)

	if stdHandles[0] != 0 || stdHandles[1] != 0 || stdHandles[2] != 0 {
		startup.StartupInfo.DwFlags |= win32.STARTF_USESTDHANDLES
		startup.StartupInfo.HStdInput = stdHandles[0]
		startup.StartupInfo.HStdOutput = stdHandles[1]
		startup.StartupInfo.HStdError = stdHandles[2]
	}

	if !spec.PreserveConsoleHandler {
		win32.SetConsoleCtrlHandler(ignoreCtrlCHandler, true)
	}

	var info win32.PROCESS_INFORMATION
	inheritHandles := inheritCount > 0
	flags := win32.EXTENDED_STARTUPINFO_PRESENT | win32.CREATE_UNICODE_ENVIRONMENT | spec.CreationFlags

	var envPtr unsafe.Pointer
	if len(spec.EnvBlock) > 0 {
		envPtr = unsafe.Pointer(&spec.EnvBlock[0])
	}

	var err error
	if spec.Token != 0 {
		err = win32.CreateProcessAsUser(spec.Token, spec.Exe, spec.CommandLine, nil, nil, inheritHandles, flags, envPtr, spec.Cwd, unsafe.Pointer(&startup.StartupInfo), &info)
	} else {
		err = win32.CreateProcess(spec.Exe, spec.CommandLine, nil, nil, inheritHandles, flags, envPtr, spec.Cwd, unsafe.Pointer(&startup.StartupInfo), &info)
	}
	if err != nil {
		return 0, err
	}
	win32.CloseHandle(info.HThread)
	return info.HProcess, nil
}

func EndJob(job win32.HANDLE) {
	win32.TerminateJobObject(job, 1)
	var info win32.JOBOBJECT_BASIC_ACCOUNTING_INFORMATION
	for waited := 0; waited < 5000; waited += 10 {
		if err := win32.QueryInformationJobObject(job, win32.JobObjectBasicAccountingInformation, unsafe.Pointer(&info), win32.DWORD(unsafe.Sizeof(info))); err != nil {
			return
		}
		if info.ActiveProcesses == 0 {
			return
		}
		win32.Sleep(10)
	}
}
