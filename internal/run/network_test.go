package run

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestNetworkChangeRollsBackOnSaveFailure(t *testing.T) {
	const networkID = "96a332e7-9e89-4bd1-922e-26010d43d249"
	for _, test := range []struct {
		name    string
		connect bool
		want    []string
	}{
		{"connect", true, []string{"connect", "disconnect", "discard"}},
		{"disconnect", false, []string{"disconnect", "restore"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.New()
			var err error
			ctx.Store, err = store.Open(t.TempDir(), false, "")
			if err != nil {
				t.Fatal(err)
			}
			var saved map[string]string
			if !test.connect {
				saved = map[string]string{networkID: "Example"}
			}
			// The record is stopped so the live update fails after the helper's
			// mutation. The original membership must survive the rollback.
			rec, err := ctx.Store.CreateContainer("target", store.ContainerRecord{HostConfig: store.ContainerHostConfig{Networks: saved}})
			if err != nil {
				t.Fatal(err)
			}
			key, err := newRunID()
			if err != nil {
				t.Fatal(err)
			}
			listener, err := win32.ListenPipe(networkPipePrefix + key)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			replies := make(chan error, 1)
			go func() {
				replies <- recordNetworkRequests(listener, networkID, test.want)
			}()
			file, err := win32.DialPipe(networkPipePrefix+key, time.Now().Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			run := &execRuntime{network: &networkHelper{stream: &framedStream{File: file}}}
			if err := run.changeNetworkLocked(ctx, rec, networkChange{Target: "Example", Connect: test.connect}); err == nil || !strings.Contains(err.Error(), "run changed") {
				t.Errorf("unexpected save failure: %v", err)
			}
			if err := <-replies; err != nil {
				t.Fatal(err)
			}
			actual, err := ctx.Store.ReadContainer(rec.ID)
			if err != nil || len(actual.HostConfig.Networks) != len(saved) || run.closing {
				t.Fatalf("rollback changed the container: %+v, %v", actual, err)
			}
		})
	}
}

func recordNetworkRequests(listener *win32.PipeListener, networkID string, want []string) error {
	file, err := listener.Accept()
	if err != nil {
		return err
	}
	defer file.Close()
	stream := &framedStream{File: file}
	if err := stream.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	for _, action := range want {
		kind, data, err := stream.receive()
		if err != nil {
			return err
		}
		var request networkRequest
		if kind != 1 || json.Unmarshal(data, &request) != nil || request.Action != action {
			return fmt.Errorf("got %s, want action %s", data, action)
		}
		reply, err := json.Marshal(networkReply{ID: networkID, Endpoint: store.NetworkAttachment{Name: "Example"}})
		if err != nil {
			return err
		}
		if err := stream.send(2, reply); err != nil {
			return err
		}
	}
	return nil
}
