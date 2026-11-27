package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

func TestParseContainerFileOptions(t *testing.T) {
	dir := t.TempDir()
	env := writeOptionFile(t, dir, "first.env", "\ufeff \t# comment\r\n\r\n  KEY=first\r\nINHERIT\r\nLITERAL= \"quotes\" # inline = literal \r\nEMPTY=\r\n")
	secondEnv := writeOptionFile(t, dir, "second.env", "KEY=second\n")
	labels := writeOptionFile(t, dir, "first.labels", "# comment\nrole=first\nempty\nCase=upper\ncase=lower\n")
	secondLabels := writeOptionFile(t, dir, "second.labels", "role=second\nkept=x=y\n")
	for _, command := range []string{"create", "run"} {
		t.Run(command, func(t *testing.T) {
			ctx := context.New()
			args := []string{"-w=${data}", "--workdir=relative", "-u", `LAPTOP\me`, "--user=S-1-5-21-123",
				"-eKEY=direct", "--env-file", env, "--env-file=" + secondEnv,
				"-lrole=direct", "--label", "bare", "--label-file=" + labels, "--label-file", secondLabels,
				"--", "app", "--workdir", "literal", "--env-file", "ignored"}
			opts, err := parseContainerOptions(ctx, args, command)
			if err != nil {
				t.Fatal(err)
			}
			if opts.Workdir != "relative" || opts.User != "S-1-5-21-123" || opts.ID != "app" || !reflect.DeepEqual(opts.Args, args[len(args)-4:]) {
				t.Fatalf("unexpected options %+v", opts)
			}
			wantEnv := []string{"KEY=first", "INHERIT", `LITERAL= "quotes" # inline = literal `, "EMPTY=", "KEY=second", "KEY=direct"}
			wantLabels := map[string]string{"role": "direct", "empty": "", "bare": "", "kept": "x=y", "Case": "upper", "case": "lower"}
			if !reflect.DeepEqual(opts.Settings.Env, wantEnv) || !reflect.DeepEqual(opts.Labels, wantLabels) {
				t.Fatalf("env %q labels %v", opts.Settings.Env, opts.Labels)
			}
			for _, args := range [][]string{
				{"--workdir"}, {"-w=", "app"}, {"--workdir=a\x00b", "app"}, {"--user=", "app"},
				{"--label", "=value", "app"}, {"--label=bad key=value", "app"}, {"--env-file=", "app"},
				{"--label-file", filepath.Join(dir, "missing"), "app"},
			} {
				if _, err := parseContainerOptions(ctx, args, command); err == nil {
					t.Errorf("accepted %q", args)
				}
			}
		})
	}

	for _, content := range []string{"=hidden", "bad key=hidden", "KEY=hidden\x00", "KEY=hidden\xff"} {
		bad := writeOptionFile(t, dir, "bad.env", "# first line\n"+content)
		_, err := parseRunOptions(context.New(), []string{"--env-file=" + bad, "app"})
		if err == nil || !strings.Contains(err.Error(), `bad.env" line 2`) || strings.Contains(err.Error(), "hidden") {
			t.Errorf("malformed file error %v", err)
		}
	}
}

func writeOptionFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestContainerOptionsLifecycle(t *testing.T) {
	st, call, reg, payload := mountCLI(t)
	fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) {
		c.FormatVersion = 4
		c.Workdir = "${data}"
		c.Labels = map[string]string{"role": "image", "image": "kept"}
		c.Cmd = []string{"read-path", "marker.txt"}
	})
	app := reg.Host() + "/app:1"
	must := func(args ...string) string {
		t.Helper()
		out, code := call(st.Root, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		return strings.TrimSpace(out)
	}
	inspect := func(name string) store.ContainerConfig {
		t.Helper()
		var cfg store.ContainerConfig
		if err := json.Unmarshal([]byte(must("inspect", "--format={{json .Config}}", name)), &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := writeOptionFile(t, dir, "worker.env", "VALUE=file\nINHERITED\nREMOVE_ME\n")
	labels := writeOptionFile(t, dir, "worker.labels", "role=file\nfile=kept\n")
	relative := func(path string) string {
		t.Helper()
		rel, err := filepath.Rel(filepath.Dir(st.Root), path)
		if err != nil {
			t.Fatal(err)
		}
		return rel
	}
	workspace := t.TempDir()
	writeOptionFile(t, workspace, "marker.txt", "mounted workdir")
	const mountedDir = "${data}/workspace"
	mount := "type=bind,source=" + workspace + ",target=" + mountedDir
	t.Setenv("INHERITED", "saved")
	t.Setenv("REMOVE_ME", "")
	if err := os.Unsetenv("REMOVE_ME"); err != nil {
		t.Fatal(err)
	}
	id := must("container", "create", "--name=worker", "--mount", mount, "--workdir="+mountedDir, "--user", account.Username,
		"-evalue=direct", "--env-file", relative(env), "--label", "role=direct", "--label-file", relative(labels),
		app, "fixture.exe", "env", "VALUE", "INHERITED", "REMOVE_ME")
	before, err := st.ReadContainer(id)
	if err != nil || before.RunSpec.UserSID != account.Uid || before.RunSpec.Workdir != mountedDir {
		t.Fatalf("saved launch inputs %+v %v", before, err)
	}
	wantLabels := map[string]string{"role": "direct", "file": "kept", "image": "kept"}
	if cfg := inspect(id); cfg.WorkingDir != mountedDir || cfg.User != account.Username || !reflect.DeepEqual(cfg.Labels, wantLabels) {
		t.Fatalf("created config %+v", cfg)
	}
	for _, path := range []string{env, labels} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("INHERITED", "changed")
	t.Setenv("REMOVE_ME", "now present")
	for _, command := range []string{"start", "restart"} {
		must(command, id)
		if code := must("wait", id); code != "0" {
			t.Fatalf("%s exit code %q", command, code)
		}
		if cfg := inspect(id); filepath.Clean(cfg.WorkingDir) != filepath.Join(before.DataDir, "workspace") || !reflect.DeepEqual(cfg.Labels, wantLabels) {
			t.Fatalf("started config %+v", cfg)
		}
	}
	logs := must("logs", id)
	for _, want := range []string{"VALUE=direct:true", "INHERITED=saved:true", "REMOVE_ME=:false"} {
		if strings.Count(logs, want) != 2 {
			t.Fatalf("missing replayed %q in %q", want, logs)
		}
	}

	if out := must("run", "--rm", "-w="+workspace, "-u"+account.Uid, app); out != "mounted workdir" {
		t.Fatalf("foreground run or image command: %q", out)
	}
	detached := must("run", "-d", "--sandbox=low", "--mount", mount, "-wworkspace", "-u="+account.Uid, app)
	if code := must("wait", detached); code != "0" || must("logs", detached) != "mounted workdir" {
		t.Fatalf("detached run exit code %q", code)
	}

	if out, code := call(st.Root, "create", "--name=wrong-user", "--user=S-1-1-0", app); code == 0 || !strings.Contains(out, "--user requires the current Windows account") {
		t.Fatalf("mismatched user: %d %s", code, out)
	}
	if _, err := st.ResolveContainer("wrong-user"); !errors.Is(err, store.ErrContainerNotFound) {
		t.Fatalf("mismatched user created a record: %v", err)
	}
	if _, err := st.UpdateContainer(id, func(c *store.ContainerRecord) error {
		c.RunSpec.UserSID = "S-1-1-0"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if out, code := call(st.Root, "start", id); code == 0 || !strings.Contains(out, "--user requires the current Windows account") {
		t.Fatalf("mismatched saved user: %d %s", code, out)
	}
	for _, workdir := range []string{"${data}/missing", filepath.Join(workspace, "marker.txt"), `C:relative`, `\rooted`} {
		if out, code := call(st.Root, "run", "--rm", "--workdir="+workdir, app); code == 0 || !strings.Contains(out, "--workdir") {
			t.Fatalf("invalid workdir %q: %d %s", workdir, code, out)
		}
	}

	source := must("create", "--workdir=${data}", app, `.\local.exe`, "env", "CAPTURED")
	record, err := st.ReadContainer(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(record.DataDir, "local.exe"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"created", "exited"} {
		digest := must("commit", source)
		if out := must("run", "--rm", "-eCAPTURED="+phase, digest); out != "CAPTURED="+phase+":true" {
			t.Fatalf("commit from %s container: %q", phase, out)
		}
		if phase == "created" {
			must("start", source)
			if code := must("wait", source); code != "0" {
				t.Fatalf("relative executable exit code %q", code)
			}
		}
	}
}
