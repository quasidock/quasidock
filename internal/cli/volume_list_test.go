package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/store"
)

func volumeListFixture(t *testing.T) (*store.Store, func(...string) (string, uint32, error)) {
	t.Helper()
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	return ctx.Store, func(args ...string) (string, uint32, error) {
		out.Reset()
		code, err := Dispatch(ctx, append([]string{"volume"}, args...))
		return out.String(), code, err
	}
}

func TestVolumeListFiltersAndFormats(t *testing.T) {
	st, call := volumeListFixture(t)
	must := func(t *testing.T, args ...string) string {
		t.Helper()
		out, code, err := call(args...)
		if code != 0 || err != nil {
			t.Fatalf("%v returned %d, %v, %q", args, code, err, out)
		}
		return out
	}
	if _, err := st.CreateVolume("bare"); err != nil {
		t.Fatal(err)
	}
	must(t, "create", "Zed")
	must(t, "create", "--label=team=dev", "alpha-dev")
	must(t, "create", "--label", "team=old", "--label=team=build=ci", "--label", "cached", "Alpha-build")
	if got := must(t, "create", "--label", "team=changed", "ALPHA-BUILD"); got != "Alpha-build\n" {
		t.Fatalf("recreate printed %q", got)
	}
	if got := must(t, "inspect", "-f", "{{json .Labels}}", "alpha-BUILD"); got != "{\"cached\":\"\",\"team\":\"build=ci\"}\n" {
		t.Fatalf("inspect labels = %q", got)
	}

	const table = "DRIVER   VOLUME NAME\nlocal    Alpha-build\nlocal    alpha-dev\nlocal    bare\nlocal    Zed\n"
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"default table", []string{"ls"}, table},
		{"explicit table", []string{"list", "--format=table"}, table},
		{"quiet names", []string{"ls", "-q"}, "Alpha-build\nalpha-dev\nbare\nZed\n"},
		{"same key OR and different keys AND", []string{"list", "--quiet", "--filter=name=^Alpha-", "-f", "name=Zed$", "-f=driver=local", "--filter", "label=team=build=ci"}, "Alpha-build\n"},
		{"case sensitive name", []string{"ls", "-q", "-f=name=^alpha-"}, "alpha-dev\n"},
		{"name substring", []string{"ls", "-q", "-f=name=build"}, "Alpha-build\n"},
		{"driver exact", []string{"ls", "-q", "-f=driver=loc"}, ""},
		{"driver union", []string{"ls", "-q", "-f=driver=remote", "-f=driver=local", "-f=name=^Zed$"}, "Zed\n"},
		{"label presence", []string{"ls", "-q", "-f=label=team"}, "Alpha-build\nalpha-dev\n"},
		{"label union", []string{"ls", "-q", "-f=label=cached", "-f=label=team=dev"}, "Alpha-build\nalpha-dev\n"},
		{"label empty value", []string{"ls", "-q", "-f=label=cached="}, "Alpha-build\n"},
		{"case sensitive label key", []string{"ls", "-q", "-f=label=Team"}, ""},
		{"case sensitive label value", []string{"ls", "-q", "-f=label=team=DEV"}, ""},
		{"quiet ignores template", []string{"ls", "-q", "--format={{", "-f=label=team=dev"}, "alpha-dev\n"},
		{"plain template", []string{"ls", "--filter=label=team=dev", "--format", "{{.Name}}/{{.Scope}}/{{.Label \"team\"}}"}, "alpha-dev/local/dev\n"},
		{"custom table", []string{"ls", "--filter=name=build", "--format", "table {{.Name}}\\t{{.Label \"team\"}}"}, "VOLUME NAME   team\nAlpha-build   build=ci\n"},
		{"empty table", []string{"ls", "-f=name=^unknown$"}, "DRIVER   VOLUME NAME\n"},
		{"empty custom table", []string{"ls", "-f=name=^unknown$", "--format=table {{.Name}}\t{{.Driver}}"}, "VOLUME NAME   DRIVER\n"},
		{"empty JSON", []string{"ls", "-f=name=^unknown$", "--format=json"}, ""},
		{"empty template", []string{"ls", "-f=name=^unknown$", "--format={{.Name}}"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := must(t, test.args...); got != test.want {
				t.Fatalf("%v printed %q, want %q", test.args, got, test.want)
			}
		})
	}

	jsonLines := must(t, "list", "--format=json")
	lines := strings.Split(strings.TrimSuffix(jsonLines, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected four JSON lines, got %q", jsonLines)
	}
	if got := must(t, "ls", "--format={{json .}}"); got != jsonLines {
		t.Fatalf("JSON template printed %q, want %q", got, jsonLines)
	}
	var row volumeListRow
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatal(err)
	}
	build, err := st.ReadVolume("Alpha-build")
	if err != nil {
		t.Fatal(err)
	}
	if row.Name != build.Name || row.Driver != "local" || row.Scope != "local" || row.Mountpoint != build.Mountpoint ||
		row.CreatedAt != build.CreatedAt || row.Anonymous || row.Labels["team"] != "build=ci" {
		t.Fatalf("unexpected JSON row %+v", row)
	}
	for _, line := range lines {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &fields); err != nil || len(fields) != 7 || string(fields["Anonymous"]) != "false" {
			t.Fatalf("unexpected JSON fields %s, %v", line, err)
		}
	}
	if !strings.Contains(lines[2], `"Labels":{}`) {
		t.Fatalf("unlabeled volume printed %s", lines[2])
	}
}

func TestVolumeListDanglingReadsSavedReferences(t *testing.T) {
	st, call := volumeListFixture(t)
	idle, err := st.CreateVolume("Idle")
	if err != nil {
		t.Fatal(err)
	}
	create := func(name string, mount mounts.Spec) string {
		t.Helper()
		mount.Target = "${data}"
		rec, err := st.CreateContainer(name, store.ContainerRecord{HostConfig: store.ContainerHostConfig{Mounts: []mounts.Spec{mount}}})
		if err != nil {
			t.Fatal(err)
		}
		return rec.ID
	}
	create("created", mounts.Spec{Type: "volume", Source: "CreatedVolume"})
	exited := create("exited", mounts.Spec{Type: "volume", Source: "ExitedVolume"})
	stale := create("stale", mounts.Spec{Type: "volume", Source: "StaleVolume"})
	create("bind", mounts.Spec{Type: "bind", Source: idle.Mountpoint})
	anonymous, err := st.CreateContainer("anonymous", store.ContainerRecord{HostConfig: store.ContainerHostConfig{
		Mounts: []mounts.Spec{{Type: "volume", Target: "${data}", Anonymous: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateContainer(exited, func(rec *store.ContainerRecord) error {
		rec.State.Status = "exited"
		rec.State.Launched = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateContainer(stale, func(rec *store.ContainerRecord) error {
		rec.State.Running = true
		rec.State.Launched = true
		rec.HostConfig.AutoRemove = true
		rec.HostConfig.Mounts[0].Source = "STALEVOLUME"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(st.ContainerStatePath(stale))
	if err != nil {
		t.Fatal(err)
	}
	quiet := func(want string, args ...string) {
		t.Helper()
		out, code, err := call(append([]string{"ls", "-q"}, args...)...)
		if code != 0 || err != nil || out != want {
			t.Fatalf("%v returned %d, %v, %q, want %q", args, code, err, out, want)
		}
	}
	quiet("Idle\n", "-f=dangling=true")
	quiet("CreatedVolume\nExitedVolume\nStaleVolume\n", "-f=dangling=0", "--filter", "name=Volume$")
	quiet("CreatedVolume\nIdle\n", "-f=dangling=1", "-f=dangling=false", "-f=name=^(CreatedVolume|Idle)$")
	anonName := anonymous.Mounts[0].Name
	out, code, err := call("list", "-f=dangling=false", "-f=name=^"+anonName+"$", "--format=json")
	var row volumeListRow
	if decodeErr := json.Unmarshal([]byte(out), &row); code != 0 || err != nil || decodeErr != nil || row.Name != anonName || !row.Anonymous {
		t.Fatalf("anonymous JSON row returned %d, %v, %v, %q", code, err, decodeErr, out)
	}
	after, err := os.ReadFile(st.ContainerStatePath(stale))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("list changed stale container state, read error %v", err)
	}

	if err := os.WriteFile(st.ContainerStatePath(stale), []byte("broken record"), 0o644); err != nil {
		t.Fatal(err)
	}
	quiet("Idle\n", "-f=name=^Idle$")
	if out, code, err := call("ls", "-f=dangling=true"); code != 1 || err == nil || out != "" {
		t.Fatalf("listing corrupt references returned %d, %v, %q", code, err, out)
	}
}

func TestVolumeListAndCreateArgumentErrors(t *testing.T) {
	st, call := volumeListFixture(t)
	for _, name := range []string{"alpha", "zed"} {
		if _, err := st.CreateVolume(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"ls", "alpha"}, {"ls", "--unknown"},
		{"ls", "-f"}, {"ls", "--filter="}, {"ls", "-f=name"}, {"ls", "-f=name="},
		{"ls", "-f=name=["}, {"ls", "-f=dangling=yes"}, {"ls", "-f=label==value"},
		{"ls", "-f=label=bad key"}, {"ls", "-f=scope=local"},
		{"ls", "--format"}, {"ls", "--format="}, {"ls", "--format=table "},
		{"ls", "--format={{", "-f=name=absent"}, {"ls", "--format=table {{.Unknown}}"},
		{"ls", "--format={{if eq .Name \"zed\"}}{{.Unknown}}{{end}}{{.Name}}"},
		{"create", "--label"}, {"create", "--label=", "invalid"}, {"create", "--label", "=value", "invalid"},
		{"create", "--label=bad key=value", "invalid"}, {"create", "--label=key=bad\x00value", "invalid"},
		{"create", "--label", "valid=yes"}, {"create", "invalid", "--label=valid=yes"},
	} {
		if out, code, err := call(args...); code != 1 || err == nil || out != "" {
			t.Errorf("%v returned %d, %v, %q", args, code, err, out)
		}
	}
	for _, subcommand := range []string{"create", "ls", "list"} {
		if out, code, err := call(subcommand, "--help"); code != 0 || err != nil || !strings.Contains(out, "usage: quasidock volume") {
			t.Errorf("%s --help returned %d, %v, %q", subcommand, code, err, out)
		}
	}
	if out, code, err := call("create", "help"); code != 0 || err != nil || out != "help\n" {
		t.Errorf("creating volume named help returned %d, %v, %q", code, err, out)
	}
	if volumes, err := st.ListVolumes(); err != nil || len(volumes) != 3 {
		t.Fatalf("unexpected volumes after failed create attempts: %v, %v", volumes, err)
	}
}
