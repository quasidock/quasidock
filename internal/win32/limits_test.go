package win32_test

import (
	"testing"

	"github.com/quasidock/quasidock/internal/process"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestJobResourceLimits(t *testing.T) {
	job, err := process.NewJob()
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(job)
	for _, limited := range []bool{false, true, false} {
		info, err := win32.QueryJobLimits(job)
		if err != nil {
			t.Fatal(err)
		}
		if limited {
			info.SetResources(2500, 256<<20, 8)
		} else {
			info.SetResources(0, 0, 0)
		}
		if err := win32.SetJobLimits(job, info); err != nil {
			t.Fatalf("limited=%t: %v", limited, err)
		}
		after, err := win32.QueryJobLimits(job)
		if err != nil {
			t.Fatal(err)
		}
		if after.Extended.BasicLimitInformation.LimitFlags&win32.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
			t.Fatal("lost kill-on-close")
		}
		if limited && (after.Extended.JobMemoryLimit != 256<<20 || after.Extended.BasicLimitInformation.ActiveProcessLimit != 8) {
			t.Fatalf("limits not applied: %+v", after)
		}
		if !limited && after.Extended.BasicLimitInformation.LimitFlags != win32.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE {
			t.Fatalf("limits not cleared: %+v", after)
		}
	}
}
