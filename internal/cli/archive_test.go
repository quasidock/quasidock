package cli

import (
	"bytes"
	"compress/gzip"
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/regkey"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestArchiveOptions(t *testing.T) {
	for _, args := range [][]string{{"-o", "images.tar", "node", "go"}, {"--output=images.tar", "node", "go"}} {
		opts, err := parseSaveOptions(args)
		if err != nil || opts.output != "images.tar" || !reflect.DeepEqual(opts.images, []string{"node", "go"}) {
			t.Fatalf("save options: %+v %v", opts, err)
		}
	}
	for _, args := range [][]string{nil, {"-o"}, {"--output=", "app"}, {"--unknown", "app"}, {"app", "--output=x"}, {""}} {
		if _, err := parseSaveOptions(args); err == nil {
			t.Fatalf("accepted save %v", args)
		}
	}
	if opts, err := parseSaveOptions([]string{"--", "node"}); err != nil || opts.output != "-" {
		t.Fatalf("save stream default: %+v %v", opts, err)
	}
	for _, args := range [][]string{{"-i", "images.tar", "-q", "--force"}, {"--input=images.tar", "--quiet", "--force=true"}} {
		opts, err := parseLoadOptions(args)
		if err != nil || opts.input != "images.tar" || !opts.quiet || !opts.force {
			t.Fatalf("load options: %+v %v", opts, err)
		}
	}
	for _, args := range [][]string{{"file.tar"}, {"--input="}, {"-i"}, {"-q=yes"}, {"--force=maybe"}, {"--unknown"}} {
		if _, err := parseLoadOptions(args); err == nil {
			t.Fatalf("accepted load %v", args)
		}
	}
	if opts, err := parseLoadOptions(nil); err != nil || opts.input != "-" || opts.force || opts.quiet {
		t.Fatalf("load defaults: %+v %v", opts, err)
	}
}

func TestSaveAtomicOutput(t *testing.T) {
	ctx, out := fixtureCLI(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	digest, layer := fixtureImage(t, reg, "1", []byte("fixture"))
	if _, err := imageCommand(ctx, []string{"pull", reg.Host() + "/app:1"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "images.tar")
	if err := os.WriteFile(path, []byte("old archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := imageSave(ctx, []string{"-o", path, digest}); err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(path)
	if err != nil || len(archive) < 1024 {
		t.Fatal("archive was not written", err)
	}
	if err := os.WriteFile(ctx.Store.BlobPath(oci.DigestHex(layer.Digest)), bytes.Repeat([]byte{'x'}, int(layer.Size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := imageSave(ctx, []string{"--output=" + path, digest}); err == nil {
		t.Fatal("saved corrupt source blob")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, archive) {
		t.Fatal("failed save replaced existing archive", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary output was not cleaned up", err)
	}
	writeErr := errors.New("closed output")
	ctx.Stdout = diffErrorWriter{writeErr}
	if err := imageSave(ctx, []string{digest}); !errors.Is(err, writeErr) {
		t.Fatal("stdout error not propagated", err)
	}
	ctx.Stdout = out
	if err := imageLoad(ctx, []string{"-i", path}); err == nil {
		t.Fatal("load reused corrupt existing blob")
	}
	if err := imageSave(ctx, []string{"--help"}); err != nil || !strings.Contains(out.String(), saveUsage) {
		t.Fatal("save help missing", err)
	}
	if err := imageLoad(ctx, []string{"--help"}); err != nil || !strings.Contains(out.String(), loadUsage) {
		t.Fatal("load help missing", err)
	}
}

func TestArchiveCLI(t *testing.T) {
	source, _ := fixtureCLI(t)
	dest, _ := fixtureCLI(t)
	binary, payload, _ := buildContainerTestBinaries(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	key := `Software\QuasidockArchiveTest` + fmt.Sprint(time.Now().UnixNano())
	ledger := `Software\Quasidock\Tests\Archive` + fmt.Sprint(time.Now().UnixNano())
	host, err := regkey.Create(win32.HKEY_CURRENT_USER, key)
	if err != nil {
		t.Fatal(err)
	}
	valueName := "Value"
	if err := host.SetString(&valueName, "host"); err != nil {
		t.Fatal(err)
	}
	host.Close()
	call := func(home string, input []byte, args ...string) ([]byte, string, int) {
		t.Helper()
		deadline, cancel := stdcontext.WithTimeout(stdcontext.Background(), 35*time.Second)
		defer cancel()
		cmd := exec.CommandContext(deadline, binary, args...)
		cmd.Env = append(os.Environ(), "QUASIDOCK_HOME="+home, "QUASIDOCK_GRANTS_KEY="+ledger)
		cmd.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err == nil {
			return stdout.Bytes(), stderr.String(), 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return stdout.Bytes(), stderr.String(), exit.ExitCode()
		}
		t.Fatalf("CLI %v: %v (%s)", args, err, stderr.String())
		return nil, "", 1
	}
	must := func(home string, input []byte, args ...string) []byte {
		t.Helper()
		out, stderr, code := call(home, input, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out, stderr)
		}
		return out
	}
	t.Cleanup(func() {
		for _, home := range []string{source.Store.Root, dest.Store.Root} {
			_, _, _ = call(home, nil, "rm", "-f", "state-source", "state-restored")
			_, _, _ = call(home, nil, "repair")
		}
		root := regkey.Key{Handle: win32.HKEY_CURRENT_USER}
		_ = root.DeleteTree(key)
		_ = root.DeleteTree(ledger)
	})
	first, _ := fixtureImage(t, reg, "1", payload, func(c *oci.AppConfig) { c.Registry = []string{key} })
	second, _ := fixtureImage(t, reg, "2", payload, func(c *oci.AppConfig) { c.Registry = []string{key} })
	app := reg.Host() + "/app:1"
	alias := "example.test/offline:alias"
	must(source.Store.Root, nil, "pull", app)
	must(source.Store.Root, nil, "pull", reg.Host()+"/app:2")
	must(source.Store.Root, nil, "tag", first, alias)
	requests := reg.Count("")
	reg.Fail(true)
	archive := must(source.Store.Root, nil, "--verbose", "save", first, second)
	file := filepath.Join(t.TempDir(), "images.tar")
	if out := must(source.Store.Root, nil, "image", "save", "-o", file, first, second); len(out) != 0 {
		t.Fatal("save wrote text to stdout")
	}
	if data, err := os.ReadFile(file); err != nil || !bytes.Equal(data, archive) {
		t.Fatal("file and stdout archives differ", err)
	}
	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	if _, err := gw.Write(archive); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if out := must(dest.Store.Root, compressed.Bytes(), "image", "load", "-q"); len(out) != 0 {
		t.Fatalf("quiet load printed %q", out)
	}
	out := string(must(dest.Store.Root, nil, "load", "-i", file))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !slices.IsSorted(lines) || !strings.Contains(out, "Loaded image: "+alias) || len(lines) != 3 {
		t.Fatalf("loaded names: %q", out)
	}
	if out := string(must(dest.Store.Root, nil, "run", "--rm", "--pull=never", alias, "fixture.exe", "log", "offline")); out != "offline\n" {
		t.Fatalf("offline run: %q", out)
	}
	idx, err := dest.Store.ImageCatalog()
	if err != nil || idx.Defaults["test.cli.fixture"] != second {
		t.Fatal("source default was not restored", err)
	}
	// Capture real private files and HKCU state, then transfer them without
	// involving the original container record or an online registry.
	must(source.Store.Root, nil, "run", "--pull=never", "-d", "--name", "state-source", app, "fixture.exe", "state-hold", `$HOME\count`, key)
	rec, err := source.Store.ResolveContainer("state-source")
	if err != nil {
		t.Fatal(err)
	}
	awaitPauseCondition(t, "source state was not initialized", func() bool {
		return strings.Contains(string(must(source.Store.Root, nil, "logs", "state-source")), "state file=1 registry=1")
	})
	committedName := "example.test/captured:saved"
	digest := strings.TrimSpace(string(must(source.Store.Root, nil, "commit", "state-source", committedName)))
	must(source.Store.Root, nil, "kill", rec.ID)
	committed := must(source.Store.Root, nil, "image", "save", digest)
	must(dest.Store.Root, committed, "load", "--quiet")
	// Loading must not activate captured registry state in the host.
	checkHost := func() {
		t.Helper()
		k, err := regkey.Open(win32.HKEY_CURRENT_USER, key, win32.KEY_READ)
		if err != nil || k == nil {
			t.Fatal("host registry key missing", err)
		}
		defer k.Close()
		value, err := k.GetString(valueName)
		if err != nil || value == nil || *value != "host" {
			t.Fatalf("host registry state changed: %v %v", value, err)
		}
	}
	checkHost()
	must(dest.Store.Root, nil, "run", "--pull=never", "-d", "--name", "state-restored", committedName)
	awaitPauseCondition(t, "captured files or HKCU state were not restored", func() bool {
		return strings.Contains(string(must(dest.Store.Root, nil, "logs", "state-restored")), "state file=2 registry=2")
	})
	must(dest.Store.Root, nil, "kill", "state-restored")
	checkHost()
	if reg.Count("") != requests {
		t.Fatal("save/load or --pull=never contacted the registry")
	}
}
