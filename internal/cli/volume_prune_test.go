package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/grants"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

func expectVolumePrune(t *testing.T, call func(...string) (string, uint32, error), want string, flags ...string) {
	t.Helper()
	out, code, err := call(append([]string{"prune"}, flags...)...)
	if code != 0 || err != nil || out != want {
		t.Fatalf("prune %v returned %d, %v, %q, want %q", flags, code, err, out, want)
	}
}

func TestVolumePruneScope(t *testing.T) {
	st, call := volumeCLIFixture(t)
	named, err := st.CreateVolume(strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := st.CreateContainer("owner", store.ContainerRecord{HostConfig: store.ContainerHostConfig{
		Mounts: []mounts.Spec{{Type: "volume", Target: "${data}", Anonymous: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	name := owner.Mounts[0].Name
	const empty = "Would reclaim 0 B in 0 volume(s)\n"
	expectVolumePrune(t, call, empty, "--dry-run")
	if err := st.RemoveContainer(owner.ID); err != nil {
		t.Fatal(err)
	}
	writePruneFixture(t, filepath.Join(owner.Mounts[0].Source, "data.txt"), "anonymous payload")
	anonBytes := mounts.TreeSize(filepath.Dir(owner.Mounts[0].Source))
	preview := fmt.Sprintf("%s\nWould reclaim %s in 1 volume(s)\n", name, lifecycle.FormatBytes(anonBytes))
	expectVolumePrune(t, call, preview, "--dry-run")
	expectVolumePrune(t, call, preview, "--dry-run", "--filter", "label!=keep")
	expectVolumePrune(t, call, empty, "--dry-run", "--filter", "label=team")
	names := []string{name, named.Name}
	slices.Sort(names)
	allBytes := anonBytes + mounts.TreeSize(filepath.Dir(named.Mountpoint))
	allPreview := fmt.Sprintf("%s\nWould reclaim %s in 2 volume(s)\n", strings.Join(names, "\n"), lifecycle.FormatBytes(allBytes))
	for _, all := range []string{"-a", "--all"} {
		expectVolumePrune(t, call, allPreview, all, "--dry-run")
	}
	if _, err := st.ReadVolume(name); err != nil {
		t.Fatalf("preview removed anonymous volume: %v", err)
	}
	expectVolumePrune(t, call, strings.Replace(preview, "Would reclaim", "Reclaimed", 1), "-f")
	if _, err := st.ReadVolume(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("anonymous volume still readable or unexpected error: %v", err)
	}
	if _, err := st.ReadVolume(named.Name); err != nil {
		t.Fatalf("default prune removed named volume: %v", err)
	}
}

func TestVolumePruneFilters(t *testing.T) {
	st, call := volumeCLIFixture(t)
	volumes := make(map[string]*store.Volume)
	for _, fixture := range []struct {
		name   string
		labels []string
	}{
		{"eligible", []string{"team=ops=west", "cached"}},
		{"weekly", []string{"team=ops=west", "cached", "Keep", "backup=weekly"}},
		{"keep", []string{"team=ops=west", "cached", "keep=always"}},
		{"backup", []string{"team=ops=west", "cached", "backup=daily"}},
		{"missing-cache", []string{"team=ops=west"}},
		{"nonempty-cache", []string{"team=ops=west", "cached=yes"}},
		{"wrong-key", []string{"Team=ops=west", "cached"}},
		{"wrong-value", []string{"team=OPS=west", "cached"}},
		{"unlabeled", nil},
		{"referenced", []string{"team=ops=west", "cached"}},
		{"leased", []string{"team=ops=west", "cached"}},
	} {
		args := []string{"create"}
		for _, label := range fixture.labels {
			args = append(args, "--label", label)
		}
		if out, code, err := call(append(args, fixture.name)...); code != 0 || err != nil {
			t.Fatalf("create %s returned %d, %v, %q", fixture.name, code, err, out)
		}
		v, err := st.ReadVolume(fixture.name)
		if err != nil {
			t.Fatal(err)
		}
		volumes[fixture.name] = v
	}
	owner, err := st.CreateContainer("owner", store.ContainerRecord{HostConfig: store.ContainerHostConfig{
		Mounts: []mounts.Spec{{Type: "volume", Source: "referenced", Target: "${data}"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	runLock, err := store.OpenLock(st.ContainerRunLockPath(owner.ID), true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer runLock.Release()
	session, err := grants.BeginMounts(st, owner, oci.SandboxSoft, []grants.MountRequest{{Path: volumes["leased"].Mountpoint}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	writePruneFixture(t, filepath.Join(volumes["eligible"].Mountpoint, "payload"), "volume payload")
	wantBytes := mounts.TreeSize(filepath.Dir(volumes["eligible"].Mountpoint)) + mounts.TreeSize(filepath.Dir(volumes["weekly"].Mountpoint))
	preview := fmt.Sprintf("eligible\nweekly\nWould reclaim %s in 2 volume(s)\n", lifecycle.FormatBytes(wantBytes))
	filters := []string{"--filter=label=team=ops=west", "--filter", "label=cached=", "--filter", "label!=keep", "--filter=label!=backup=daily"}
	expectVolumePrune(t, call, "Would reclaim 0 B in 0 volume(s)\n", append([]string{"--dry-run"}, filters...)...)
	expectVolumePrune(t, call, preview, append([]string{"--all", "--dry-run"}, filters...)...)
	for name := range volumes {
		if _, err := st.ReadVolume(name); err != nil {
			t.Fatalf("preview removed %s: %v", name, err)
		}
	}
	expectVolumePrune(t, call, strings.Replace(preview, "Would reclaim", "Reclaimed", 1), append([]string{"--all", "--force"}, filters...)...)
	for name := range volumes {
		_, err := st.ReadVolume(name)
		if name == "eligible" || name == "weekly" {
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("pruned volume %s still readable or unexpected error: %v", name, err)
			}
		} else if err != nil {
			t.Errorf("prune removed %s: %v", name, err)
		}
	}
}

func TestVolumePruneArgumentErrorsAndHelp(t *testing.T) {
	st, call := volumeCLIFixture(t)
	if _, err := st.CreateVolume("kept"); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{
		{"--filter"}, {"--filter="}, {"--filter=label"}, {"--filter=label="},
		{"--filter=label!="}, {"--filter=label==value"}, {"--filter=label=two words"},
		{"--filter=label=key=bad\x00value"}, {"--filter=name=kept"}, {"--filter=until=1h"},
		{"--bogus"}, {"kept"},
	} {
		out, code, err := call(append([]string{"prune", "--all"}, flags...)...)
		if code != 1 || err == nil || out != "" {
			t.Errorf("prune %v returned %d, %v, %q", flags, code, err, out)
		}
	}
	for _, help := range []string{"-h", "--help"} {
		expectVolumePrune(t, call, volumePruneHelp+"\n", "--all", help)
	}
	if _, err := st.ReadVolume("kept"); err != nil {
		t.Fatalf("argument validation or help removed volume: %v", err)
	}
}
