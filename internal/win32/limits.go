package win32

import (
	"fmt"
	"unsafe"
)

const (
	jobObjectCPURateControlInformation int32 = 15
	jobObjectLimitActiveProcess        DWORD = 0x8
	jobObjectLimitJobMemory            DWORD = 0x200
)

type jobCPURate struct{ Flags, Rate DWORD }

// JobLimits is a snapshot so changes preserve kill-on-close and other limits.
type JobLimits struct {
	Extended JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	CPU      jobCPURate
}

func QueryJobLimits(job HANDLE) (JobLimits, error) {
	var info JobLimits
	if err := QueryInformationJobObject(job, JobObjectExtendedLimitInformation, unsafe.Pointer(&info.Extended), DWORD(unsafe.Sizeof(info.Extended))); err != nil {
		return info, fmt.Errorf("query job extended limits: %w", err)
	}
	err := QueryInformationJobObject(job, jobObjectCPURateControlInformation, unsafe.Pointer(&info.CPU), DWORD(unsafe.Sizeof(info.CPU)))
	if err != nil {
		return info, fmt.Errorf("query job CPU limits: %w", err)
	}
	return info, nil
}

func (l *JobLimits) SetResources(cpuRate DWORD, memory uintptr, processes DWORD) {
	l.CPU = jobCPURate{}
	if cpuRate != 0 {
		l.CPU = jobCPURate{Flags: 1 | 4, Rate: cpuRate}
	}
	l.Extended.BasicLimitInformation.LimitFlags &^= jobObjectLimitActiveProcess | jobObjectLimitJobMemory
	l.Extended.BasicLimitInformation.ActiveProcessLimit = processes
	l.Extended.JobMemoryLimit = memory
	if memory != 0 {
		l.Extended.BasicLimitInformation.LimitFlags |= jobObjectLimitJobMemory
	}
	if processes != 0 {
		l.Extended.BasicLimitInformation.LimitFlags |= jobObjectLimitActiveProcess
	}
}

// SetJobLimits can partially apply on error; callers must restore their snapshot.
func SetJobLimits(job HANDLE, info JobLimits) error {
	// Disabling an already-disabled job returns ERROR_INVALID_PARAMETER.
	var current jobCPURate
	if err := QueryInformationJobObject(job, jobObjectCPURateControlInformation, unsafe.Pointer(&current), DWORD(unsafe.Sizeof(current))); err != nil {
		return err
	}
	if info.CPU.Flags != 0 || current.Flags != 0 {
		if err := SetInformationJobObject(job, jobObjectCPURateControlInformation, unsafe.Pointer(&info.CPU), DWORD(unsafe.Sizeof(info.CPU))); err != nil {
			return fmt.Errorf("set job CPU limits: %w", err)
		}
	}
	if err := SetJobExtendedLimits(job, &info.Extended); err != nil {
		return fmt.Errorf("set job extended limits: %w", err)
	}
	return nil
}
