package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/win32"
)

var ErrEndOfInput = errors.New("end of input")

var (
	promptMu      sync.Mutex
	promptConsole win32.HANDLE
	promptMode    win32.DWORD
)

func restoreEcho(event uint32) uintptr {
	promptMu.Lock()
	defer promptMu.Unlock()
	if promptConsole != 0 {
		win32.SetConsoleMode(promptConsole, promptMode)
	}
	return uintptr(win32.FALSE)
}

var ctrlHandlerCallback = syscall.NewCallback(restoreEcho)

type Echo int

const (
	EchoInput Echo = iota
	NoEcho
)

type Answer int

const (
	AnswerNone Answer = iota
	AnswerYes
	AnswerNo
)

func ParseAnswer(line string) *Answer {
	a := strings.Trim(line, " \t\r\n")
	if len(a) == 0 || strings.EqualFold(a, "y") || strings.EqualFold(a, "yes") {
		ans := AnswerYes
		return &ans
	}
	if strings.EqualFold(a, "n") || strings.EqualFold(a, "no") {
		ans := AnswerNo
		return &ans
	}
	return nil
}

// ConsoleLine reads a line directly from the Windows console.
// Returns nil, nil if stdin is not a console.
func ConsoleLine(label string, echo Echo) (*string, error) {
	console := win32.GetStdHandle(win32.STD_INPUT_HANDLE)
	if console == 0 || console == win32.INVALID_HANDLE_VALUE {
		return nil, nil
	}
	var mode win32.DWORD
	if !win32.GetConsoleMode(console, &mode) {
		return nil, nil
	}

	context.PrintStderr("%s", label)
	if echo == NoEcho {
		promptMu.Lock()
		promptConsole = console
		promptMode = mode
		promptMu.Unlock()

		win32.SetConsoleCtrlHandler(ctrlHandlerCallback, true)
		noEchoMode := (mode | win32.ENABLE_LINE_INPUT | win32.ENABLE_PROCESSED_INPUT) &^ win32.ENABLE_ECHO_INPUT
		if !win32.SetConsoleMode(console, noEchoMode) {
			return nil, fmt.Errorf("turning off console echo (SetConsoleMode failed)")
		}
		defer func() {
			win32.SetConsoleMode(console, mode)
			win32.SetConsoleCtrlHandler(ctrlHandlerCallback, false)
			promptMu.Lock()
			promptConsole = 0
			promptMu.Unlock()
		}()
	}

	var u16Line []uint16
	complete := false
	for {
		// Check if we have a newline in what we read so far
		hasNewline := false
		for _, ch := range u16Line {
			if ch == '\n' {
				hasNewline = true
				break
			}
		}
		if hasNewline {
			complete = true
			break
		}

		var buf [256]uint16
		var charsRead win32.DWORD
		if !win32.ReadConsole(console, buf[:], &charsRead) || charsRead == 0 {
			break
		}
		u16Line = append(u16Line, buf[:charsRead]...)
	}

	if echo == NoEcho {
		context.PrintStderr("\n")
	}
	if !complete {
		return nil, ErrEndOfInput
	}

	// Find the end before newline
	end := len(u16Line)
	for i, ch := range u16Line {
		if ch == '\n' {
			end = i
			break
		}
	}
	textU16 := u16Line[:end]
	for len(textU16) > 0 && textU16[len(textU16)-1] == '\r' {
		textU16 = textU16[:len(textU16)-1]
	}
	text := win32.UTF16ToUTF8(textU16)
	return &text, nil
}

var (
	stdinMu     sync.Mutex
	stdinReader *bufio.Reader
)

func StdinLine() *string {
	stdinMu.Lock()
	defer stdinMu.Unlock()
	if stdinReader == nil {
		stdinReader = bufio.NewReader(os.Stdin)
	}
	line, err := stdinReader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return nil
	}
	line = strings.TrimRight(line, "\r\n")
	return &line
}

// AskYesNo asks a yes-or-no question, defaulting to yes.
func AskYesNo(ctx *context.Context, question string) (Answer, error) {
	label := fmt.Sprintf("%s [Y/n] ", question)
	for {
		cLine, err := ConsoleLine(label, EchoInput)
		if err != nil {
			if errors.Is(err, ErrEndOfInput) {
				context.PrintStderr("\n")
				return AnswerNone, nil
			}
			return AnswerNone, err
		}

		var line string
		if cLine != nil {
			line = *cLine
		} else {
			context.PrintStderr("%s", label)
			sLine := StdinLine()
			if sLine == nil {
				context.PrintStderr("\n")
				return AnswerNone, nil
			}
			context.PrintStderr("%s\n", *sLine)
			line = *sLine
		}

		if ans := ParseAnswer(line); ans != nil {
			return *ans, nil
		}
		context.Note("answer y or n")
	}
}
