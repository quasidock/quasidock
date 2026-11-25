package win32_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestJobProcessesNested(t *testing.T) {
	job, err := process.NewJob()
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(job)
	if rows, err := win32.QueryJobProcesses(context.Background(), job); err != nil || len(rows) != 0 {
		t.Fatalf("empty job: %+v %v", rows, err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("QUASIDOCK_STATS_HELPER", ready)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-test.run=^TestJobStatsChild$", "-test.bench=quoted marker 雪"}
	line := process.BuildCommandLine(exe, args)
	// CreateProcess permits 32,767 UTF-16 characters including the terminator.
	// Exercise that boundary as well as quoting and Unicode preservation.
	args[1] += strings.Repeat("x", 32766-len(utf16.Encode([]rune(line))))
	line = process.BuildCommandLine(exe, args)
	child, err := process.Start(process.SpawnSpec{Exe: exe, CommandLine: line, ParentJobs: []win32.HANDLE{job}})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("process helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, err := win32.QueryJobProcesses(context.Background(), job)
	if err != nil || len(rows) != 1 {
		t.Fatalf("parent job: %+v %v", rows, err)
	}
	row := rows[0]
	if row.PID != win32.GetProcessId(child.Process) || row.PPID == nil || *row.PPID != uint32(os.Getpid()) || row.CPUTime == nil || row.Command != line || !strings.Contains(row.Command, `"-test.bench=quoted marker 雪`) {
		t.Fatalf("process identity: PID %d PPID %v CPU %v; command bytes %d, want %d", row.PID, row.PPID, row.CPUTime, len(row.Command), len(line))
	}
	nested, err := win32.QueryJobProcesses(context.Background(), child.Job)
	if err != nil || len(nested) != 1 || nested[0].PID != row.PID {
		t.Fatalf("nested job: %+v %v", nested, err)
	}
	if err := os.WriteFile(ready+".stop", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Observe the process as it exits; churn must not fail enumeration.
	for range 20 {
		if _, err := win32.QueryJobProcesses(context.Background(), job); err != nil {
			t.Fatalf("exit race: %v", err)
		}
	}
	if _, finished, err := child.WaitFor(5000); err != nil || !finished {
		t.Fatalf("helper exit: %t %v", finished, err)
	}
	if rows, err := win32.QueryJobProcesses(context.Background(), job); err != nil || len(rows) != 0 {
		t.Fatalf("exited job: %+v %v", rows, err)
	}
	if _, err := win32.QueryJobProcesses(context.Background(), win32.INVALID_HANDLE_VALUE); err == nil {
		t.Fatal("invalid job accepted")
	}
	cancel, stop := context.WithCancel(context.Background())
	stop()
	if _, err := win32.QueryJobProcesses(cancel, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
