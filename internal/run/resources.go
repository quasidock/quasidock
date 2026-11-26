package run

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

// UpdateOptions is a patch: nil preserves a field and a pointer to zero clears it.
type UpdateOptions struct {
	NanoCPUs      *int64
	Memory        *int64
	PidsLimit     *int64
	RestartPolicy *store.RestartPolicy
}

func (o UpdateOptions) Empty() bool {
	return o.NanoCPUs == nil && o.Memory == nil && o.PidsLimit == nil && o.RestartPolicy == nil
}

var cpuPattern = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)$`)

func ParseCPUs(value string) (int64, error) {
	r, ok := new(big.Rat).SetString(value)
	if !ok || !cpuPattern.MatchString(value) || r.Sign() < 0 {
		return 0, errors.New("--cpus expects a non-negative decimal number")
	}
	r.Mul(r, big.NewRat(1_000_000_000, 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return 0, errors.New("--cpus supports at most nine decimal places within the supported range")
	}
	n := r.Num().Int64()
	if _, err := cpuRate(n); err != nil {
		return 0, err
	}
	return n, nil
}

var memoryPattern = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*([kmgtp]?)(?:i?b)?$`)

func ParseMemory(value string) (int64, error) {
	m := memoryPattern.FindStringSubmatch(value)
	invalid := func() (int64, error) {
		return 0, fmt.Errorf("invalid --memory %q: expected bytes or a size such as 512m within the supported range", value)
	}
	if m == nil {
		return invalid()
	}
	r, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return invalid()
	}
	power := strings.Index(" kmgtp", strings.ToLower(m[2]))
	if m[2] == "" {
		power = 0
	}
	multiplier := new(big.Int).Lsh(big.NewInt(1), uint(power*10))
	r.Mul(r, new(big.Rat).SetInt(multiplier))
	n := new(big.Int).Quo(r.Num(), r.Denom())
	if !n.IsInt64() || (r.Sign() > 0 && n.Sign() == 0) {
		return invalid()
	}
	return n.Int64(), nil
}

func ParsePidsLimit(value string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < -1 || n > math.MaxUint32 {
		return 0, errors.New("--pids-limit expects -1, 0, or a positive 32-bit process count")
	}
	if n == -1 {
		n = 0
	}
	return n, nil
}

func ParseRestartPolicy(value string) (store.RestartPolicy, error) {
	name, retries, hasRetries := strings.Cut(value, ":")
	p := store.RestartPolicy{Name: name}
	switch name {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return p, errors.New("--restart expects no, on-failure[:N], always, or unless-stopped")
	}
	if hasRetries {
		n, err := strconv.Atoi(retries)
		if name != "on-failure" || err != nil || n < 0 {
			return p, errors.New("restart retry count must be a non-negative integer and is only valid with on-failure")
		}
		p.MaximumRetryCount = n
	}
	return p, nil
}

func cpuRate(nano int64) (win32.DWORD, error) {
	if nano == 0 {
		return 0, nil
	}
	n, err := win32.ActiveProcessorCount()
	if err != nil {
		return 0, err
	}
	if nano < 0 || nano > int64(n)*1_000_000_000 {
		return 0, fmt.Errorf("--cpus must be between 0 and the host's %d logical CPUs", n)
	}
	// Division first avoids overflow for hosts with many processor groups.
	rate := nano / (int64(n) * 100_000)
	if rate == 0 {
		return 0, errors.New("--cpus is below Windows' minimum CPU rate of 1/10000 host capacity")
	}
	return win32.DWORD(rate), nil
}

func validateHostConfig(h store.ContainerHostConfig) error {
	if _, err := cpuRate(h.NanoCPUs); err != nil {
		return err
	}
	if h.Memory < 0 || uint64(h.Memory) > uint64(^uintptr(0)) {
		return errors.New("memory limit exceeds the supported address range")
	}
	if h.PidsLimit < 0 || h.PidsLimit > math.MaxUint32 {
		return errors.New("invalid process limit")
	}
	name := h.RestartPolicy.Name
	if name == "" {
		name = "no"
	}
	if _, err := ParseRestartPolicy(name); err != nil {
		return err
	}
	if h.RestartPolicy.MaximumRetryCount < 0 || (h.RestartPolicy.MaximumRetryCount != 0 && name != "on-failure") {
		return errors.New("invalid restart retry count")
	}
	if h.AutoRemove && h.RestartPolicy.Enabled() {
		return errors.New("--rm and a non-no --restart policy cannot be combined")
	}
	return nil
}

func (o UpdateOptions) apply(h *store.ContainerHostConfig) error {
	if o.NanoCPUs != nil {
		h.NanoCPUs = *o.NanoCPUs
	}
	if o.Memory != nil {
		h.Memory = *o.Memory
	}
	if o.PidsLimit != nil {
		h.PidsLimit = *o.PidsLimit
	}
	if o.RestartPolicy != nil {
		h.RestartPolicy = *o.RestartPolicy
	}
	return validateHostConfig(*h)
}

func setJobResources(job win32.HANDLE, resources store.ContainerResources) (win32.JobLimits, bool, error) {
	old, err := win32.QueryJobLimits(job)
	if err != nil {
		return old, false, err
	}
	rate, err := cpuRate(resources.NanoCPUs)
	if err != nil {
		return old, false, err
	}
	if resources.PidsLimit > 0 {
		accounting, err := win32.QueryJobBasicAccounting(job)
		if err != nil {
			return old, false, err
		}
		if int64(accounting.ActiveProcesses) > resources.PidsLimit {
			return old, false, errors.New("process limit is below the current active process count")
		}
	}
	next := old
	next.SetResources(rate, uintptr(resources.Memory), win32.DWORD(resources.PidsLimit))
	return old, true, win32.SetJobLimits(job, next)
}

// updateLocked holds the runtime mutex, serializing kernel changes with exec
// admission. Persistence follows kernel success; any failure restores the job.
func (r *execRuntime) updateLocked(ctx *context.Context, rec *store.ContainerRecord, opts UpdateOptions) error {
	current, err := ctx.Store.ReadContainer(rec.ID)
	if err != nil {
		return err
	}
	if current.State.RunID != r.runID {
		return errors.New("container run changed during update")
	}
	next := current.HostConfig
	if err := opts.apply(&next); err != nil {
		return err
	}
	if next.ContainerResources == current.HostConfig.ContainerResources {
		_, err := saveContainerUpdate(ctx, current, opts)
		return err
	}
	old, changed, err := setJobResources(r.job, next.ContainerResources)
	if err == nil {
		_, err = saveContainerUpdate(ctx, current, opts)
	}
	if err != nil && changed {
		if rollbackErr := win32.SetJobLimits(r.job, old); rollbackErr != nil {
			_, suppressErr := suppressRestart(ctx, rec)
			r.stopLocked()
			var killErr error
			if !win32.TerminateJobObject(r.job, 1) {
				killErr = errors.New("terminating job after resource rollback failed")
			}
			r.updateFailure = errors.Join(err, fmt.Errorf("rolling back resource limits: %w", rollbackErr), suppressErr, killErr)
			return r.updateFailure
		}
	}
	return err
}

func RenameContainer(ctx *context.Context, target, name string) (*store.ContainerRecord, error) {
	_, op, err := lockTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	return op.Rename(name)
}

func UpdateContainer(ctx *context.Context, target string, opts UpdateOptions) (*store.ContainerRecord, error) {
	if opts.Empty() {
		return nil, errors.New("update requires at least one setting")
	}
	rec, op, err := lockTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	defer op.Release()
	h := rec.HostConfig
	if err := opts.apply(&h); err != nil {
		return nil, err
	}
	if ctx.Store.IsContainerRunning(rec.ID) {
		if rec.State.ControlProtocol < 2 {
			return nil, errors.New("container's supervisor does not support update; stop and start it with this binary")
		}
		if err := sendControlRequest(ctx, rec, controlRequest{Action: actionUpdate, Update: &opts}); err != nil {
			return nil, err
		}
		return ctx.Store.ReadContainer(rec.ID)
	}
	return saveContainerUpdate(ctx, rec, opts)
}

func saveContainerUpdate(ctx *context.Context, rec *store.ContainerRecord, opts UpdateOptions) (*store.ContainerRecord, error) {
	changed := false
	updated, err := ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
		if c.State.SupervisorID != rec.State.SupervisorID {
			return errors.New("container supervisor changed during update")
		}
		before := c.HostConfig
		if err := opts.apply(&c.HostConfig); err != nil {
			return err
		}
		changed = before.ContainerResources != c.HostConfig.ContainerResources || before.RestartPolicy != c.HostConfig.RestartPolicy
		return nil
	})
	if err == nil && changed {
		ctx.Store.RecordContainerEvent(updated, "update", nil)
	}
	return updated, err
}
