package run

import (
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

// Monitor the owned process handle rather than a reusable PID. A completed
// monitor cannot alter a replacement run's endpoints.
func monitorPortRelay(ctx *context.Context, rec *store.ContainerRecord, relay, primary win32.HANDLE, direct map[string][]store.PortBinding, note func(string, ...any)) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if win32.WaitForSingleObject(relay, 0) == win32.WAIT_TIMEOUT {
					continue
				}
				changed := false
				_, _ = ctx.Store.UpdateContainer(rec.ID, func(c *store.ContainerRecord) error {
					if c.State.RunID == rec.State.RunID && c.State.Running && c.NetworkSettings != nil {
						c.NetworkSettings.Ports = direct
						changed = true
					}
					return nil
				})
				if changed && win32.WaitForSingleObject(primary, 0) == win32.WAIT_TIMEOUT {
					note("note: port relay exited; forwarded ports are no longer published for %s", rec.ShortID())
				}
				return
			}
		}
	}()
	return func() { close(stop); <-done }
}
