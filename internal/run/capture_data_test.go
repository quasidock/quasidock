package run

import (
	stdcontext "context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/win32"
)

func TestCaptureDataOwnershipFailureRestoresPause(t *testing.T) {
	ctx, rec, r := pauseTestRuntime(t)
	rec.State.CommitProtocol = commitProtocol
	r.commitContext, r.commitRecord = ctx, rec
	if err := r.start(rec.ID, rec.State.RunID); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.MkdirAll(filepath.Join(rec.DataDir, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	lk, err := ctx.Store.Lock(false)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	op, err := ctx.Store.LockContainer(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Release()
	err = CaptureData(stdcontext.Background(), ctx, rec, []string{`Software\QuasidockUnownedCaptureTest` + rec.ID}, t.TempDir(), true)
	if err == nil || !strings.Contains(err.Error(), "ownership changed") {
		t.Fatal("captured unowned HKCU state", err)
	}
	state, err := ctx.Store.ReadContainer(rec.ID)
	if err != nil || !state.State.Running || state.State.Paused {
		t.Fatal("ownership failure changed pause state", state, err)
	}
	if frozen, err := win32.IsJobFrozen(r.job); err != nil || frozen {
		t.Fatal("ownership failure left the job frozen", frozen, err)
	}
}
