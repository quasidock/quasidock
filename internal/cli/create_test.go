package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
)

func TestParseCreateOptions(t *testing.T) {
	ctx := context.New()
	opts, err := parseCreateOptions(ctx, []string{
		"--name", "saved", "--rm", "-it", "--pull=never", "--entrypoint=tool.exe",
		"-e", "VALUE=one", "--env", "INHERITED", "--sandbox=appcontainer",
		"--filesystem=cwd", "--nofilesystem=C:\\unused", "--share=network", "--unshare=host",
		"--port=http=8081", "-v", "saved-data:${data}/volume",
		"--mount", "type=bind,source=./work,target=${data}/work", "--verbose",
		"app", "arg", "--detach", "-d",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Name != "saved" || !opts.AutoRemove || !opts.Interactive || !opts.TTY || opts.Detach || opts.Pull != "never" || !ctx.Verbose {
		t.Fatalf("unexpected options: %+v", opts)
	}
	if opts.ID != "app" || !reflect.DeepEqual(opts.Args, []string{"arg", "--detach", "-d"}) || opts.Entrypoint == nil || *opts.Entrypoint != "tool.exe" {
		t.Fatalf("lost image or application arguments: %+v", opts)
	}
	if len(opts.Mounts) != 2 || len(opts.Settings.Env) != 2 || opts.Settings.Sandbox == nil || *opts.Settings.Sandbox != oci.SandboxAppContainer || opts.Settings.Network == nil || !*opts.Settings.Network || opts.Settings.Host == nil || *opts.Settings.Host {
		t.Fatalf("lost container configuration: %+v", opts)
	}
	for _, args := range [][]string{
		nil, {"--rm"}, {"--name"}, {"--pull=invalid", "app"},
		{"-d", "app"}, {"-dit", "app"}, {"-itd", "app"}, {"--detach", "app"}, {"--detach=false", "app"},
		{"--unknown=value", "app"}, {"--sandbox=invalid", "app"},
		{"-v", "a:${data}", "-v", "b:${data}/x", "app"},
	} {
		if _, err := parseCreateOptions(context.New(), args); err == nil {
			t.Errorf("accepted invalid options: %v", args)
		}
	}
	opts, err = parseCreateOptions(context.New(), []string{"--", "app", "--rm"})
	if err != nil || opts.Pull != "missing" || !reflect.DeepEqual(opts.Args, []string{"--rm"}) || opts.AutoRemove {
		t.Fatalf("option terminator: %+v %v", opts, err)
	}
}

func TestCreateMetadataAndImagePolicies(t *testing.T) {
	ctx, out := fixtureCLI(t)
	reg := testregistry.New()
	defer reg.Close()
	first, _ := fixtureImage(t, reg, "1", []byte("not an executable"), func(c *oci.AppConfig) {
		c.Registry = []string{`Software\QuasidockCreateTest`}
	})
	app := reg.Host() + "/app:latest"
	ctx.Env["CREATE_INHERITED"] = "saved"
	delete(ctx.Env, "CREATE_ABSENT")
	bind := t.TempDir()
	create := func(args ...string) *store.ContainerRecord {
		t.Helper()
		out.Reset()
		if code, err := createCommand(ctx, args); code != 0 || err != nil {
			t.Fatalf("create %v: %d %v", args, code, err)
		}
		id := strings.TrimSpace(out.String())
		if !oci.IsSha256Hex(id) {
			t.Fatalf("expected only full container ID, got %q", out.String())
		}
		rec, err := ctx.Store.ReadContainer(id)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	rec := create("--name", "saved", "--rm", "-it", "--sandbox=appcontainer",
		"--entrypoint=missing.exe", "-e", "CREATE_INHERITED", "-e", "CREATE_ABSENT",
		"-v", "saved-data:${data}/volume", "--mount", "type=bind,source="+bind+",target=${data}/bind", app, "argument")
	if rec.Image != first || rec.ImageRef != app || rec.State.Status != "created" || rec.State.Running || rec.State.Launched || rec.State.RunID != "" || rec.State.Pid != 0 || rec.State.StartedAt != "" || rec.State.FinishedAt != "" {
		t.Fatalf("unexpected created state: %+v", rec)
	}
	if !rec.HostConfig.AutoRemove || !rec.Config.OpenStdin || !rec.Config.Tty || rec.Config.AttachStdin || len(rec.Mounts) != 2 || rec.RunSpec.Entrypoint == nil || *rec.RunSpec.Entrypoint != "missing.exe" {
		t.Fatalf("lost saved configuration: %+v", rec)
	}
	if value := rec.RunSpec.InheritedEnv["create_inherited"]; value == nil || *value != "saved" {
		t.Fatal("inherited environment was not snapshotted")
	}
	if value, saved := rec.RunSpec.InheritedEnv["create_absent"]; !saved || value != nil {
		t.Fatal("absent environment was not snapshotted")
	}
	entries, err := os.ReadDir(rec.DataDir)
	if err != nil || len(entries) != 0 || store.Exists(rec.LogPath) || ctx.Store.IsContainerRunning(rec.ID) {
		t.Fatalf("creation prepared runtime resources: %v %v", entries, err)
	}
	for _, m := range rec.Mounts {
		if store.Exists(m.Destination) {
			t.Fatalf("creation attached mount %s", m.Destination)
		}
	}
	for _, all := range []bool{false, true} {
		listed, err := ctx.Store.ListContainers(all)
		if err != nil || len(listed) != map[bool]int{false: 0, true: 1}[all] {
			t.Fatalf("list all=%t: %v %v", all, listed, err)
		}
	}
	out.Reset()
	if err := containerLogs(ctx, []string{"saved"}); err != nil || out.Len() != 0 {
		t.Fatalf("unstarted logs: %q %v", out.String(), err)
	}
	if _, _, err := ctx.Store.RemoveImage("", first); err == nil {
		t.Fatal("unstarted container did not pin its image")
	}
	if err := ctx.Store.RemoveVolume("saved-data"); err == nil {
		t.Fatal("unstarted container did not pin its volume")
	}
	for _, args := range [][]string{
		{"--name", "saved", app}, {"--name", "bad/name", app},
		{"--mount", "type=bind,source=" + filepath.Join(bind, "missing") + ",target=${data}", app},
	} {
		if _, err := createCommand(ctx, args); err == nil {
			t.Fatalf("accepted invalid creation: %v", args)
		}
	}
	second, layer := fixtureImage(t, reg, "2", []byte("still not executable"))
	before := reg.Count("/manifests/")
	if got := create(app); got.Image != first || reg.Count("/manifests/") != before {
		t.Fatal("default policy refreshed a cached image")
	}
	if _, err := createCommand(ctx, []string{"--pull=never", reg.Host() + "/app:missing"}); !errors.Is(err, store.ErrImageNotFound) || reg.Count("/manifests/") != before {
		t.Fatalf("never contacted registry or lost missing-image error: %v", err)
	}
	if got := create("--pull=always", app); got.Image != second {
		t.Fatal("always did not refresh the image")
	}
	reg.Fail(true)
	if _, err := createCommand(ctx, []string{"--name", "failed-pull", "--pull=always", app}); err == nil {
		t.Fatal("failed pull fell back to cached image")
	}
	if _, err := ctx.Store.ResolveContainer("failed-pull"); !errors.Is(err, store.ErrContainerNotFound) {
		t.Fatal("failed pull created a container")
	}
	if got := create("--pull=never", first); got.Image != first {
		t.Fatal("offline digest selection changed the image")
	}
	deploy, err := ctx.Store.DeployPath(layer.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctx.Store.DeleteDeployment(layer.Digest); err != nil {
		t.Fatal(err)
	}
	create("--pull=never", app)
	if store.Exists(deploy) {
		t.Fatal("creation unnecessarily deployed image layers")
	}
	if err := ctx.Store.RemoveContainer(rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := ctx.Store.RemoveVolume("saved-data"); err != nil {
		t.Fatalf("removed container still pins volume: %v", err)
	}
}

func TestCreateContainerLifecycle(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	first, _ := fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) { c.Env["IMAGE_VERSION"] = "one" })
	app := reg.Host() + "/app:latest"
	must := func(args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return out
	}
	wait := func(name string, removed bool) *store.ContainerRecord {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			rec, err := st.ResolveContainer(name)
			if removed && errors.Is(err, store.ErrContainerNotFound) {
				return nil
			}
			if err == nil && !removed && rec.State.Status == "exited" && !st.IsContainerRunning(rec.ID) {
				return rec
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("container %s did not finish (removed=%t)", name, removed)
		return nil
	}
	t.Setenv("CREATE_VALUE", "original")
	t.Setenv("CREATE_ABSENT", "")
	if err := os.Unsetenv("CREATE_ABSENT"); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(must("create", "--name", "replay", "-it", "-e", "CREATE_VALUE", "-e", "CREATE_ABSENT", app, "fixture.exe", "env", "CREATE_VALUE", "CREATE_ABSENT", "IMAGE_VERSION"))
	before, err := st.ReadContainer(id)
	if err != nil || !oci.IsSha256Hex(id) {
		t.Fatalf("created ID: %q %v", id, err)
	}
	if out := must("ps", "-a"); !strings.Contains(out, "replay") || !strings.Contains(out, "Created") {
		t.Fatalf("created container missing from ps -a: %s", out)
	}
	if out := must("ps"); strings.Contains(out, "replay") {
		t.Fatalf("unstarted container listed as running: %s", out)
	}
	if out := must("container", "inspect", "-f", "{{.State.Status}} {{.State.Running}}", "replay"); strings.TrimSpace(out) != "created false" {
		t.Fatal(out)
	}
	if out := must("logs", "replay"); out != "" {
		t.Fatalf("created container has logs: %q", out)
	}
	sentinel := filepath.Join(before.DataDir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREATE_VALUE", "changed")
	t.Setenv("CREATE_ABSENT", "now-present")
	fixtureImage(t, reg, "2", payload, func(c *oci.AppConfig) { c.Env["IMAGE_VERSION"] = "two" })
	must("pull", app)
	reg.Fail(true)
	requests := reg.Count("/manifests/")
	for range 2 {
		must("start", "replay")
		after := wait("replay", false)
		if after.ID != id || after.Created != before.Created || after.Image != first || after.DataDir != before.DataDir || !reflect.DeepEqual(after.RunSpec, before.RunSpec) || !reflect.DeepEqual(after.HostConfig, before.HostConfig) {
			t.Fatal("start replaced saved identity or configuration")
		}
	}
	if reg.Count("/manifests/") != requests {
		t.Fatal("start contacted the registry")
	}
	logs := must("logs", "replay")
	for _, want := range []string{"CREATE_VALUE=original:true", "CREATE_ABSENT=:false", "IMAGE_VERSION=one:true"} {
		if strings.Count(logs, want) != 2 {
			t.Fatalf("saved environment or image changed: %q", logs)
		}
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "retained" {
		t.Fatalf("private data lost: %s %v", data, err)
	}

	// A missing executable is checked by start, retaining even --rm records.
	must("container", "create", "--name", "bad-command", "--rm", "--pull=never", "--entrypoint=missing.exe", first)
	if out, code := call(st.Root, "start", "bad-command"); code == 0 {
		t.Fatalf("missing executable started: %s", out)
	}
	if rec, err := st.ResolveContainer("bad-command"); err != nil || rec.State.Error == "" || rec.State.Launched {
		t.Fatalf("failed start did not retain record: %+v %v", rec, err)
	}
	must("rm", "bad-command")

	// A disappearing bind source fails startup; restoring it permits retry.
	bind := t.TempDir()
	must("container", "create", "--name", "retry", "--rm", "--pull=never", "-v", bind+":${data}/workspace", first, "fixture.exe", "write-path", `$HOME\..\workspace\done`, "persisted")
	if err := os.Remove(bind); err != nil {
		t.Fatal(err)
	}
	if out, code := call(st.Root, "start", "retry"); code == 0 {
		t.Fatalf("missing bind source started: %s", out)
	}
	if rec, err := st.ResolveContainer("retry"); err != nil || rec.State.Error == "" || !rec.HostConfig.AutoRemove {
		t.Fatalf("failed --rm start lost record: %+v %v", rec, err)
	}
	if err := os.Mkdir(bind, 0o755); err != nil {
		t.Fatal(err)
	}
	must("start", "retry")
	wait("retry", true)
	if data, err := os.ReadFile(filepath.Join(bind, "done")); err != nil || string(data) != "persisted" {
		t.Fatalf("automatic removal lost mounted contents: %s %v", data, err)
	}
	must("create", "--name", "prunable", "--pull=never", first)
	if out := must("container", "prune"); !strings.Contains(out, id) {
		t.Fatalf("prune did not remove stopped containers: %s", out)
	}
	if _, err := st.ResolveContainer("prunable"); !errors.Is(err, store.ErrContainerNotFound) {
		t.Fatal("new unstarted container was not immediately pruned")
	}
}
