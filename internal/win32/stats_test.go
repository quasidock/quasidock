package win32_test

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestJobStatsChild(t *testing.T) {
	ready := os.Getenv("QUASIDOCK_STATS_HELPER")
	if ready == "" {
		return
	}
	memory := make([]byte, 32<<20)
	for i := range memory {
		memory[i] = byte(i)
	}
	if err := os.WriteFile(ready+".io", memory[:1<<20], 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(ready + ".io")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(ready + ".stop"); err == nil {
			break
		}
		for i := range memory {
			memory[i]++
		}
		time.Sleep(time.Millisecond)
	}
	runtime.KeepAlive(memory)
}

func TestJobStatsNestedAccounting(t *testing.T) {
	job, err := process.NewJob()
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(job)
	empty, err := win32.QueryJobStats(job)
	if err != nil || empty != (win32.JobStats{}) {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("QUASIDOCK_STATS_HELPER", ready)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child, err := process.Start(process.SpawnSpec{Exe: exe, CommandLine: process.BuildCommandLine(exe, []string{"-test.run=^TestJobStatsChild$"}), ParentJobs: []win32.HANDLE{job}})
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
			t.Fatal("memory helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stats, err := win32.QueryJobStats(job)
	if err != nil || stats.PrivateMemory < 32<<20 || stats.ReadBytes < 1<<20 || stats.WriteBytes < 1<<20 || stats.PIDs != 1 || stats.CPUTime == 0 {
		t.Fatalf("parent: %+v %v", stats, err)
	}
	nested, err := win32.QueryJobStats(child.Job)
	if err != nil || nested.PIDs != 1 || nested.PrivateMemory < 32<<20 {
		t.Fatalf("nested: %+v %v", nested, err)
	}
	ids, err := win32.QueryJobProcessIDs(job)
	if err != nil || len(ids) != 1 || ids[0] != win32.GetProcessId(child.Process) {
		t.Fatalf("process list: %v %v", ids, err)
	}
	if err := os.WriteFile(ready+".stop", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, finished, err := child.WaitFor(5000); err != nil || !finished {
		t.Fatalf("helper exit: %t %v", finished, err)
	}
	child.Close()
	after, err := win32.QueryJobStats(job)
	if err != nil || after.PIDs != 0 || after.PrivateMemory != 0 || after.CPUTime < stats.CPUTime || after.ReadBytes < stats.ReadBytes || after.WriteBytes < stats.WriteBytes {
		t.Fatalf("exited process accounting: %+v %v", after, err)
	}
	if _, err := win32.QueryJobStats(win32.INVALID_HANDLE_VALUE); err == nil {
		t.Fatal("invalid job succeeded")
	}
	if processors, err := win32.ActiveProcessorCount(); err != nil || processors == 0 {
		t.Fatalf("processor count: %d %v", processors, err)
	}
}
