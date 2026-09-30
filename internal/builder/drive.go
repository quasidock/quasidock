package builder

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/win32"
)

const (
	DriveLetter = "B:"
	DirPrefix   = "quasidock-build-"
	mutexName   = "Local\\quasidock-build-drive"
	removeFlags = win32.DDD_REMOVE_DEFINITION | win32.DDD_EXACT_MATCH_ON_REMOVE | win32.DDD_RAW_TARGET_PATH | win32.DDD_NO_BROADCAST_SYSTEM
)

type Drive struct {
	Mutex     syscall.Handle
	RawTarget string
}

func (d *Drive) Release() {
	if d.RawTarget != "" {
		_ = win32.DefineDosDevice(removeFlags, DriveLetter, d.RawTarget)
		d.RawTarget = ""
	}
	if d.Mutex != 0 && d.Mutex != syscall.InvalidHandle {
		_ = win32.ReleaseMutex(win32.HANDLE(d.Mutex))
		_ = win32.CloseHandle(win32.HANDLE(d.Mutex))
		d.Mutex = 0
	}
}

func AcquireDrive(ctx *context.Context, dir string) (*Drive, error) {
	mutex, err := win32.CreateMutex(mutexName)
	if err != nil {
		return nil, fmt.Errorf("CreateMutexW: %w", err)
	}

	waitRes := win32.WaitForSingleObject(mutex, 0)
	if waitRes == win32.WAIT_TIMEOUT {
		if ctx != nil {
			ctx.Note("waiting for another quasidock build to finish with %s...", DriveLetter)
		}
		waitRes = win32.WaitForSingleObject(mutex, win32.INFINITE)
	}
	if waitRes != win32.WAIT_OBJECT_0 && waitRes != win32.WAIT_ABANDONED {
		_ = win32.CloseHandle(mutex)
		return nil, fmt.Errorf("WaitForSingleObject: failed with code %d", waitRes)
	}

	existing, err := queryDosDevice(DriveLetter)
	if err == nil && existing != "" {
		if !IsBuildRoot(existing) {
			_ = win32.ReleaseMutex(mutex)
			_ = win32.CloseHandle(mutex)
			return nil, fmt.Errorf("%s is in use (%s). quasidock builds on %s, so that paths in what it builds are the same on every machine; free it to build", DriveLetter, existing, DriveLetter)
		}
		if err := win32.DefineDosDevice(removeFlags, DriveLetter, existing); err != nil {
			_ = win32.ReleaseMutex(mutex)
			_ = win32.CloseHandle(mutex)
			return nil, fmt.Errorf("removing a stale build drive mapping: %w", err)
		}
	}

	if err := win32.DefineDosDevice(win32.DDD_NO_BROADCAST_SYSTEM, DriveLetter, dir); err != nil {
		_ = win32.ReleaseMutex(mutex)
		_ = win32.CloseHandle(mutex)
		return nil, fmt.Errorf("mapping the build drive: %w", err)
	}

	rawTarget, err := queryDosDevice(DriveLetter)
	if err != nil || rawTarget == "" {
		_ = win32.DefineDosDevice(removeFlags, DriveLetter, dir)
		_ = win32.ReleaseMutex(mutex)
		_ = win32.CloseHandle(mutex)
		return nil, fmt.Errorf("%s vanished right after it was mapped", DriveLetter)
	}

	return &Drive{
		Mutex:     syscall.Handle(mutex),
		RawTarget: rawTarget,
	}, nil
}

func queryDosDevice(device string) (string, error) {
	target, err := win32.QueryDosDevice(device)
	if err != nil {
		if err == syscall.ERROR_FILE_NOT_FOUND {
			return "", nil
		}
		return "", err
	}
	return target, nil
}

func IsBuildRoot(target string) bool {
	trimmed := strings.TrimRight(target, "\\")
	lastSlash := strings.LastIndexByte(trimmed, '\\')
	name := trimmed
	if lastSlash >= 0 {
		name = trimmed[lastSlash+1:]
	}
	return strings.HasPrefix(name, DirPrefix)
}
