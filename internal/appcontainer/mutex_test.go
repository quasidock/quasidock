package appcontainer

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestMutexOwnershipAcrossScheduling(t *testing.T) {
	name := fmt.Sprintf("Local\\quasidock-mutex-test-%d", os.Getpid())
	errs := make(chan error, 16)
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for range 20 {
				h, err := acquireMutex(name)
				if err != nil {
					errs <- err
					return
				}
				runtime.Gosched()
				time.Sleep(time.Millisecond)
				if !releaseMutex(h) {
					errs <- fmt.Errorf("mutex released from a thread that does not own it")
					return
				}
			}
		})
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case err := <-errs:
		t.Fatal(err)
	case <-done:
		select {
		case err := <-errs:
			t.Fatal(err)
		default:
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mutex remained locked after its owner yielded")
	}
}
