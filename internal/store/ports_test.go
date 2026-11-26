package store

import "testing"

func TestContainerPortMappingLifecycle(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	requests := map[string][]PortBinding{"80/tcp": {{}}}
	rec, err := st.CreateContainer("ports", ContainerRecord{HostConfig: ContainerHostConfig{PortBindings: requests}})
	if err != nil {
		t.Fatal(err)
	}
	lk, err := OpenLock(st.ContainerRunLockPath(rec.ID), true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	for _, state := range []ContainerState{
		{Status: "running", Running: true, Paused: true, RunID: "one"},
		{Status: "exited"}, {Status: "restarting", Restarting: true}, {Status: "created"},
	} {
		_, err := st.UpdateContainer(rec.ID, func(c *ContainerRecord) error {
			c.State = state
			c.NetworkSettings = &ContainerNetworkSettings{Ports: map[string][]PortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: "8080"}}}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.ReadContainer(rec.ID)
		if err != nil || (len(got.NetworkSettings.Ports) > 0) != state.Running || len(got.HostConfig.PortBindings) != 1 {
			t.Fatalf("%s: %+v %v", state.Status, got, err)
		}
	}
	_, err = st.UpdateContainer(rec.ID, func(c *ContainerRecord) error {
		c.State = ContainerState{Status: "running", Running: true, Launched: true}
		c.NetworkSettings.Ports = map[string][]PortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: "8080"}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	lk.Release()
	got, err := st.ReadContainer(rec.ID)
	if err != nil || got.State.Running || len(got.NetworkSettings.Ports) != 0 || len(got.HostConfig.PortBindings) == 0 {
		t.Fatalf("crash reconciliation: %+v %v", got, err)
	}
}
