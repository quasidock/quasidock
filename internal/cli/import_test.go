package cli

import (
	"bytes"
	"compress/gzip"
	stdcontext "context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestImportOptions(t *testing.T) {
	opts, err := parseImportOptions([]string{"--config", "app.json", "-c", `ENTRYPOINT ["app.exe"]`, "--change=ENV X=value", "--force", "input.tar", "example.test/alias:v1"})
	if err != nil || opts.config != "app.json" || opts.input != "input.tar" || opts.Target != "example.test/alias:v1" || !opts.Force || !reflect.DeepEqual(opts.Changes, []string{`ENTRYPOINT ["app.exe"]`, "ENV X=value"}) {
		t.Fatalf("options: %+v %v", opts, err)
	}
	if opts, err := parseImportOptions([]string{"--config=app.json", "--", "-"}); err != nil || opts.input != "-" || opts.Target != "" || opts.Force {
		t.Fatalf("stdin options: %+v %v", opts, err)
	}
	for _, args := range [][]string{nil, {"input.tar"}, {"--config", "app.json"}, {"--config=", "-"}, {"--config=-", "-"}, {"--config=x", ""}, {"--config=x", "-", ""}, {"--config=x", "-", "alias", "extra"}, {"--config=x", "-", "--force"}, {"--config=x", "--unknown", "-"}, {"--config=x", "-c"}} {
		if _, err := parseImportOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, arg := range []string{"-h", "--help"} {
		if _, err := parseImportOptions([]string{arg}); !errors.Is(err, flag.ErrHelp) {
			t.Fatal("missing help", err)
		}
	}
}

func TestImportCommands(t *testing.T) {
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	digest, _ := fixtureImage(t, reg, "1", []byte("\x00payload\xff"))
	if _, err := imageCommand(ctx, []string{"pull", reg.Host() + "/app:1"}); err != nil {
		t.Fatal(err)
	}
	c, err := ctx.Store.CreateContainer("import-source", store.ContainerRecord{Image: digest})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	input, config := filepath.Join(dir, "export.tar"), filepath.Join(dir, "config.json")
	if code, err := exportCommand(ctx, []string{"-o", input, c.ID}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	out.Reset()
	if err := imageInspect(ctx, []string{"-f", "{{json .Quasidock}}", digest}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code, err := Dispatch(ctx, []string{"--verbose", "import", "--config", config, input, "example.test/alias:1"}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	imported := strings.TrimSpace(out.String())
	if oci.DigestHex(imported) == "" || out.String() != imported+"\n" {
		t.Fatalf("stdout is not just a manifest digest: %q", out.String())
	}
	b, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	if _, err := gw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	stdin := filepath.Join(dir, "input.gz")
	if err := os.WriteFile(stdin, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(stdin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	previous := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = previous }()
	out.Reset()
	if code, err := Dispatch(ctx, []string{"image", "import", "--config", config, "-"}); code != 0 || err != nil || strings.TrimSpace(out.String()) != imported {
		t.Fatal("gzip stdin import differs", code, err, out.String())
	}
	for _, args := range [][]string{{"import", "--help"}, {"image", "import", "-h"}} {
		out.Reset()
		if code, err := Dispatch(ctx, args); code != 0 || err != nil || !strings.Contains(out.String(), importUsage) {
			t.Fatal("help missing", code, err, out.String())
		}
	}
}

func TestImportExportRoundTripOffline(t *testing.T) {
	source, _ := fixtureCLI(t)
	dest, _ := fixtureCLI(t)
	loaded, _ := fixtureCLI(t)
	binary, payload, _ := buildContainerTestBinaries(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	key := `Software\QuasidockImportTest` + fmt.Sprint(time.Now().UnixNano())
	ledger := `Software\Quasidock\Tests\Import` + fmt.Sprint(time.Now().UnixNano())
	host, err := regkey.Create(win32.HKEY_CURRENT_USER, key)
	if err != nil {
		t.Fatal(err)
	}
	valueName := "Value"
	if err := host.SetString(&valueName, "host"); err != nil {
		t.Fatal(err)
	}
	host.Close()
	call := func(home string, args ...string) (string, string, int) {
		t.Helper()
		deadline, cancel := stdcontext.WithTimeout(stdcontext.Background(), 35*time.Second)
		defer cancel()
		cmd := exec.CommandContext(deadline, binary, args...)
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+home, "QUASIDOCK_GRANTS_KEY="+ledger)
		var out, errout bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errout
		err := cmd.Run()
		if err == nil {
			return out.String(), errout.String(), 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out.String(), errout.String(), exit.ExitCode()
		}
		t.Fatalf("CLI %v: %v (%s)", args, err, errout.String())
		return "", "", 1
	}
	must := func(home string, args ...string) string {
		t.Helper()
		out, stderr, code := call(home, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out, stderr)
		}
		return strings.TrimSpace(out)
	}
	t.Cleanup(func() {
		for _, home := range []string{source.Store.Root, dest.Store.Root, loaded.Store.Root} {
			_, _, _ = call(home, "rm", "-f", "import-source", "import-restored", "copy-one", "copy-two")
			_, _, _ = call(home, "repair")
		}
		root := regkey.Key{Handle: win32.HKEY_CURRENT_USER}
		_ = root.DeleteTree(key)
		_ = root.DeleteTree(ledger)
	})
	parent, appLayer := fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) { c.Registry = []string{key} })
	must(source.Store.Root, "pull", reg.Host()+"/app:1")
	img, err := source.Store.ReadImage("test", parent)
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the fixture executable as a runtime to exercise runtime expansion
	// and shared app/runtime blobs through export, import, and launch.
	cfg := img.Config
	cfg.Entrypoint = []string{"${tools}/fixture.exe"}
	cfg.Cmd = []string{"state-hold", `$HOME\count`, key}
	cfg.From = map[string]oci.Runtime{"tools": {ID: "import-runtime", Version: "1", Image: parent, Layer: appLayer.Digest, Env: map[string]string{"RUNTIME_ROOT": "${app}"}}}
	jsonBlob := func(value any, media string) oci.Descriptor {
		t.Helper()
		b, err := oci.ToJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		d, err := source.Store.PutBlob(b, media)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	configDesc := jsonBlob(cfg, oci.MediaTypeConfig)
	manifest := jsonBlob(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: configDesc, Layers: []oci.Descriptor{appLayer, appLayer}}, oci.MediaTypeManifest)
	if err := source.Store.RegisterImage(store.Ref{ID: cfg.ID, Version: cfg.Version, Manifest: manifest.Digest}, "example.test/source:1", true); err != nil {
		t.Fatal(err)
	}
	requests := reg.Count("")
	reg.Fail(true)
	must(source.Store.Root, "run", "-d", "--pull=never", "--name", "import-source", manifest.Digest)
	awaitPauseCondition(t, "source state was not initialized", func() bool {
		return strings.Contains(must(source.Store.Root, "logs", "import-source"), "state file=1 registry=1")
	})
	c, _ := source.Store.ResolveContainer("import-source")
	if err := os.Mkdir(filepath.Join(c.DataDir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	input, config := filepath.Join(dir, "export.tar"), filepath.Join(dir, "config.json")
	must(source.Store.Root, "export", "-o", input, "import-source")
	must(source.Store.Root, "kill", "import-source")
	configText := must(source.Store.Root, "image", "inspect", "-f", "{{json .Quasidock}}", manifest.Digest)
	if err := os.WriteFile(config, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := "example.test/different:restored"
	digest := must(dest.Store.Root, "image", "import", "--config", config, input, alias)
	if oci.DigestHex(digest) == "" {
		t.Fatalf("invalid digest output %q", digest)
	}
	imported, err := dest.Store.ReadImage(cfg.ID, digest)
	if err != nil || imported.Config.ID != cfg.ID || imported.Config.Version != cfg.Version {
		t.Fatal("alias changed imported identity", imported, err)
	}
	if deployed, err := os.ReadDir(dest.Store.Path("deploy")); err != nil || len(deployed) != 0 {
		t.Fatal("import deployed application files", deployed, err)
	}
	checkHost := func() {
		t.Helper()
		k, err := regkey.Open(win32.HKEY_CURRENT_USER, key, win32.KEY_READ)
		if err != nil || k == nil {
			t.Fatal("host registry key missing", err)
		}
		defer k.Close()
		value, err := k.GetString(valueName)
		if err != nil || value == nil || *value != "host" {
			t.Fatal("host registry changed", value, err)
		}
	}
	checkHost()
	for _, name := range []string{"copy-one", "copy-two"} {
		must(dest.Store.Root, "create", "--pull=never", "--name", name, alias)
	}
	one, _ := dest.Store.ResolveContainer("copy-one")
	two, _ := dest.Store.ResolveContainer("copy-two")
	if err := os.WriteFile(filepath.Join(one.DataDir, "home", "count"), []byte("99"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(two.DataDir, "home", "count")); err != nil || string(b) != "1" {
		t.Fatal("imported containers share writable state", b, err)
	}
	if info, err := os.Stat(filepath.Join(two.DataDir, "empty")); err != nil || !info.IsDir() {
		t.Fatal("empty directory was lost", err)
	}
	must(dest.Store.Root, "run", "-d", "--pull=never", "--name", "import-restored", alias)
	awaitPauseCondition(t, "private files or registry were not restored", func() bool {
		return strings.Contains(must(dest.Store.Root, "logs", "import-restored"), "state file=2 registry=2")
	})
	must(dest.Store.Root, "kill", "import-restored")
	checkHost()
	runtimeEnv := must(dest.Store.Root, "run", "--rm", "--pull=never", "--entrypoint=${tools}/fixture.exe", alias, "env", "RUNTIME_ROOT")
	if !strings.Contains(runtimeEnv, oci.DigestHex(imported.Config.From["tools"].Layer)) {
		t.Fatal("runtime environment was not expanded", runtimeEnv)
	}
	transfer := filepath.Join(dir, "images.tar")
	must(dest.Store.Root, "save", "-o", transfer, digest)
	must(loaded.Store.Root, "load", "-i", transfer)
	if out := must(loaded.Store.Root, "run", "--rm", "--pull=never", "--entrypoint=${tools}/fixture.exe", alias, "log", "loaded"); out != "loaded" {
		t.Fatal("imported image did not survive save/load", out)
	}
	if out, stderr, code := call(dest.Store.Root, "import", "--config", config, config); code != 1 || out != "" || stderr == "" {
		t.Fatal("failure diagnostics did not stay on stderr", code, out, stderr)
	}
	if reg.Count("") != requests {
		t.Fatal("offline export/import/run contacted the registry")
	}
}

func TestImportInterruptBlockedInput(t *testing.T) {
	binary, _, buildDir := buildContainerTestBinaries(t)
	for _, mode := range []string{"console-run-check-mode", "console-run-check-mode-import-pipe"} {
		t.Run(mode, func(t *testing.T) {
			ctx, _ := fixtureCLI(t)
			t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
			config := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(config, []byte(`{"id":"import-interrupt","version":"1","entrypoint":["program.exe"]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			var output pauseOutput
			console, input := startLifecycleConsole(t, filepath.Join(buildDir, "fixture.exe"), []string{mode, binary, "import", "--config", config, "-"}, &output)
			awaitPauseCondition(t, "import did not begin staging stdin", func() bool {
				staged, err := filepath.Glob(ctx.Store.Path("tmp", "image-import-*"))
				if err != nil || len(staged) != 1 {
					return false
				}
				return mode == "console-run-check-mode" || store.Exists(filepath.Join(staged[0], "tree", "app", "pending"))
			})
			if _, err := win32.WriteFile(input, []byte{3}); err != nil {
				t.Fatal(err)
			}
			if win32.WaitForSingleObject(console, 5000) != win32.WAIT_OBJECT_0 {
				t.Fatal("Ctrl+C did not interrupt stalled import input")
			}
			if code, err := win32.GetExitCodeProcess(console); err != nil || code != 130 {
				output.mu.Lock()
				defer output.mu.Unlock()
				t.Fatalf("interrupted exit: %d %v; output: %s", code, err, output.b.String())
			}
			if store.IsLocked(ctx.Store.Path("lock")) {
				t.Fatal("interrupted import retained its lock")
			}
			if staged, err := filepath.Glob(ctx.Store.Path("tmp", "image-import-*")); err != nil || len(staged) != 0 {
				t.Fatal("interrupted import leaked staging", staged, err)
			}
			if idx, err := ctx.Store.ImageCatalog(); err != nil || len(idx.Images) != 0 {
				t.Fatal("interrupted import published an image", idx, err)
			}
		})
	}
}
