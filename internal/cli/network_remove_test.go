package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func testNetworkMutationLock() (func(), error) { return func() {}, nil }

func privateTestNetwork(n int, name string) win32.HCNNetwork {
	return win32.HCNNetwork{ID: fmt.Sprintf("%08x-1234-5678-90ab-bcdef0123456", n), Name: name, Driver: "private"}
}

func TestNetworkRemove(t *testing.T) {
	ctx, out := fixtureCLI(t)
	legacy := privateTestNetwork(1, "Legacy")
	other := privateTestNetwork(2, "-external")
	saved := privateTestNetwork(3, "Saved")
	busy := privateTestNetwork(4, "Busy")
	failing := privateTestNetwork(5, "Failing")
	ics := win32.HCNNetwork{ID: privateTestNetwork(6, "").ID, Name: "Default Switch", Driver: "ics"}
	shared := privateTestNetwork(7, "Shared")
	shadow := privateTestNetwork(8, legacy.ID)
	rec, err := ctx.Store.CreateContainer("stopped", store.ContainerRecord{
		HostConfig: store.ContainerHostConfig{Networks: map[string]string{"{" + strings.ToUpper(saved.ID) + "}": saved.Name}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ctx.Store.TrackNetwork(other.ID); err != nil {
		t.Fatal(err)
	}
	backendErr := errors.New("test deletion failed")
	var queried, attempted []string
	backend := networkBackend{
		lockMutation: testNetworkMutationLock,
		list: func() ([]win32.HCNNetwork, error) {
			return []win32.HCNNetwork{shadow, legacy, other, saved, busy, failing, ics, shared, privateTestNetwork(9, "shared")}, nil
		},
		hasEndpoints: func(id string) (bool, error) {
			queried = append(queried, id)
			return id == busy.ID, nil
		},
		deleteNetwork: func(id string) error {
			attempted = append(attempted, id)
			if id == failing.ID {
				return backendErr
			}
			return nil
		},
	}
	args := []string{"rm", "Missing", "Shared", "Default Switch", "Saved", "Busy", "Failing", "{" + strings.ToUpper(legacy.ID) + "}", "LEGACY", "--", "-external"}
	code, err := networkCommand(ctx, args, backend)
	if code != 1 || !errors.Is(err, backendErr) || out.String() != legacy.ID+"\n"+other.ID+"\n" {
		t.Fatalf("rm: exit %d, error %v, output %q", code, err, out.String())
	}
	for _, want := range []string{`network "Missing" not found`, "multiple networks", "driver ics is protected", rec.ShortID(), "has Windows network endpoints"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if !slices.Equal(queried, []string{busy.ID, failing.ID, legacy.ID, other.ID}) || !slices.Equal(attempted, []string{failing.ID, legacy.ID, other.ID}) {
		t.Errorf("queried %v, attempted %v", queried, attempted)
	}
	if tracked, err := ctx.Store.TrackedNetworkIDs(); err != nil || len(tracked) != 0 {
		t.Errorf("tracked networks after rm: %v, %v", tracked, err)
	}
}

func TestNetworkPrune(t *testing.T) {
	ctx, out := fixtureCLI(t)
	first := privateTestNetwork(1, "Zulu")
	next := privateTestNetwork(2, "Alpha")
	gone := privateTestNetwork(3, "Gone")
	busy := privateTestNetwork(4, "Busy")
	stale := privateTestNetwork(5, "Stale")
	foreign := privateTestNetwork(6, "Foreign")
	legacy := privateTestNetwork(7, "Legacy")
	ics := win32.HCNNetwork{ID: privateTestNetwork(8, "").ID, Name: "Default Switch", Driver: "ics"}
	for _, network := range []win32.HCNNetwork{first, next, gone, busy, stale, ics} {
		if err := ctx.Store.TrackNetwork(network.ID); err != nil {
			t.Fatal(err)
		}
	}
	otherStore, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := otherStore.TrackNetwork(foreign.ID); err != nil {
		t.Fatal(err)
	}
	backendErr := errors.New("test deletion failed")
	var deleted []string
	var queried []string
	live := []win32.HCNNetwork{next, legacy, foreign, ics, gone, busy, first}
	fail := true
	backend := networkBackend{
		lockMutation: testNetworkMutationLock,
		list:         func() ([]win32.HCNNetwork, error) { return live, nil },
		hasEndpoints: func(id string) (bool, error) {
			queried = append(queried, id)
			return id == busy.ID, nil
		},
		deleteNetwork: func(id string) error {
			if id == next.ID && fail {
				return backendErr
			}
			live = slices.DeleteFunc(live, func(network win32.HCNNetwork) bool { return network.ID == id })
			if id == gone.ID {
				return win32.ErrHCNNetworkNotFound
			}
			deleted = append(deleted, id)
			return nil
		},
	}
	assertTracked := func(want ...string) {
		t.Helper()
		if tracked, err := ctx.Store.TrackedNetworkIDs(); err != nil || !slices.Equal(tracked, want) {
			t.Fatalf("tracked networks %v, want %v, error %v", tracked, want, err)
		}
	}
	if code, err := networkCommand(ctx, []string{"prune", "--dry-run"}, backend); code != 0 || err != nil || out.String() != first.ID+"\n"+next.ID+"\n"+gone.ID+"\n" || len(deleted) != 0 {
		t.Fatalf("preview: exit %d, error %v, output %q, deleted %v", code, err, out.String(), deleted)
	}
	if !slices.Equal(queried, []string{first.ID, next.ID, gone.ID, busy.ID}) {
		t.Errorf("preview checked endpoints on %v", queried)
	}
	assertTracked(first.ID, next.ID, gone.ID, busy.ID, stale.ID, ics.ID)
	out.Reset()
	if code, err := networkCommand(ctx, []string{"prune"}, backend); code != 1 || !errors.Is(err, backendErr) || out.String() != first.ID+"\n" {
		t.Fatalf("prune: exit %d, error %v, output %q", code, err, out.String())
	}
	assertTracked(next.ID, busy.ID, ics.ID)
	out.Reset()
	fail = false
	if code, err := networkCommand(ctx, []string{"prune"}, backend); code != 0 || err != nil || out.String() != next.ID+"\n" {
		t.Fatalf("retry prune: exit %d, error %v, output %q", code, err, out.String())
	}
	assertTracked(busy.ID, ics.ID)
	out.Reset()
	if code, err := networkCommand(ctx, []string{"prune"}, backend); code != 0 || err != nil || out.Len() != 0 || !slices.Equal(deleted, []string{first.ID, next.ID}) {
		t.Fatalf("empty prune: exit %d, error %v, output %q, deleted %v", code, err, out.String(), deleted)
	}
}

func TestNetworkRemovalFailsClosed(t *testing.T) {
	ctx, out := fixtureCLI(t)
	network := privateTestNetwork(1, "Example")
	if err := ctx.Store.TrackNetwork(network.ID); err != nil {
		t.Fatal(err)
	}
	backend := networkBackend{
		lockMutation: testNetworkMutationLock,
		list:         func() ([]win32.HCNNetwork, error) { return []win32.HCNNetwork{network}, nil },
		hasEndpoints: func(string) (bool, error) { return false, fs.ErrPermission },
		deleteNetwork: func(string) error {
			t.Fatal("deleted without verifying endpoints")
			return nil
		},
	}
	if code, err := networkCommand(ctx, []string{"prune"}, backend); code != 1 || !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "elevated PowerShell") || out.Len() != 0 {
		t.Fatalf("endpoint query failure: exit %d, error %v, output %q", code, err, out.String())
	}
	rec, err := ctx.Store.CreateContainer("corrupt", store.ContainerRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctx.Store.ContainerStatePath(rec.ID), []byte("invalid JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	backend.hasEndpoints = nil
	if code, err := networkCommand(ctx, []string{"prune"}, backend); code != 1 || err == nil || !strings.Contains(err.Error(), "read saved network memberships") || out.Len() != 0 {
		t.Fatalf("saved record failure: exit %d, error %v, output %q", code, err, out.String())
	}
	if tracked, err := ctx.Store.TrackedNetworkIDs(); err != nil || !slices.Equal(tracked, []string{network.ID}) {
		t.Fatalf("tracked networks after failures: %v, %v", tracked, err)
	}
}

func TestNetworkPruneWaitsForStoppedConnect(t *testing.T) {
	ctx, out := fixtureCLI(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	fixtureImage(t, reg, "1", []byte("configuration only"))
	if code, err := createCommand(ctx, []string{"--name", "app", reg.Host() + "/app:1"}); code != 0 || err != nil {
		t.Fatalf("create: exit %d, error %v", code, err)
	}
	out.Reset()
	network := privateTestNetwork(1, "Example")
	if err := ctx.Store.TrackNetwork(network.ID); err != nil {
		t.Fatal(err)
	}
	validate := make(chan error, 1)
	resume := make(chan struct{})
	list := func() ([]win32.HCNNetwork, error) { return []win32.HCNNetwork{network}, nil }
	connectCtx := context.New()
	connectCtx.Store = ctx.Store
	connected := make(chan error, 1)
	go func() {
		_, err := networkCommand(connectCtx, []string{"connect", network.ID, "app"}, networkBackend{
			list: list,
			privateSubnet: func(string) error {
				lock, err := store.TryLock(ctx.Store.Path("containers", "index.lock"), true)
				if lock != nil {
					lock.Release()
					err = errors.New("stopped connect validates outside the container index lock")
				}
				validate <- err
				<-resume
				return nil
			},
		})
		connected <- err
	}()
	select {
	case err := <-validate:
		if err != nil {
			close(resume)
			<-connected
			t.Fatal(err)
		}
	case err := <-connected:
		t.Fatalf("connect returned before validation: %v", err)
	}
	listed := make(chan struct{})
	pruned := make(chan error, 1)
	backend := networkBackend{
		lockMutation: testNetworkMutationLock,
		list: func() ([]win32.HCNNetwork, error) {
			close(listed)
			return list()
		},
		hasEndpoints: func(string) (bool, error) { return false, errors.New("prune did not see the saved membership") },
	}
	go func() {
		_, err := networkCommand(ctx, []string{"prune"}, backend)
		pruned <- err
	}()
	select {
	case <-listed:
	case err := <-pruned:
		close(resume)
		<-connected
		t.Fatalf("prune returned before listing networks: %v", err)
	}
	close(resume)
	if err := <-connected; err != nil {
		t.Fatal(err)
	}
	if err := <-pruned; err != nil || out.Len() != 0 {
		t.Fatalf("prune: %v, output %q", err, out.String())
	}
}
