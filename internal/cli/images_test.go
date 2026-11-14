package cli

import (
	"archive/tar"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/lifecycle"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/run"
	"github.com/quasidock/quasidock/internal/signature"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
	"github.com/quasidock/quasidock/internal/trust"
)

func fixtureImage(t *testing.T, reg *testregistry.Registry, version string, payload []byte) (string, oci.Descriptor) {
	t.Helper()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	files := map[string][]byte{"fixture.exe": payload, "args.cmd": []byte("@echo off\r\n\"%~dp0fixture.exe\" %*\r\n")}
	for _, name := range []string{"fixture.exe", "args.cmd"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer := oci.Descriptor{MediaType: oci.MediaTypeLayerTar, Digest: reg.PutBlob(archive.Bytes()), Size: int64(archive.Len())}
	cfg := oci.AppConfig{ID: "test.cli.fixture", Version: version, Command: "fixture.exe", Args: []string{"default", version}, Path: []string{"."}, Env: map[string]string{"REMOVE_ME": "image"}, Permissions: oci.Permissions{Sandbox: oci.SandboxSoft}}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	config := oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: reg.PutBlob(b), Size: int64(len(b))}
	b, err = json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Config: config, Layers: []oci.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	d := reg.PutManifest("app", version, b)
	reg.PutManifest("app", "latest", b)
	return d, layer
}

func fixtureCLI(t *testing.T) (*context.Context, *bytes.Buffer) {
	t.Helper()
	ctx := context.New()
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx.Store = st
	t.Cleanup(func() {
		entries, err := os.ReadDir(st.Path("deploy"))
		if err != nil {
			t.Error(err)
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() || !oci.IsSha256Hex(entry.Name()) {
				continue
			}
			deleted, err := st.DeleteDeployment("sha256:" + entry.Name())
			if err != nil || deleted == store.DeletionInUse {
				t.Errorf("cleaning test deployment %s: %v (in use: %t)", entry.Name(), err, deleted == store.DeletionInUse)
			}
		}
	})
	var out bytes.Buffer
	ctx.Stdout = &out
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	return ctx, &out
}

func TestImageCommandsAndCachePolicies(t *testing.T) {
	ctx, out := fixtureCLI(t)
	reg := testregistry.New()
	defer reg.Close()
	a, _ := fixtureImage(t, reg, "1", []byte("fixture"))
	b, layer := fixtureImage(t, reg, "2", []byte("fixture"))
	name := func(tag string) string { return reg.Host() + "/app:" + tag }
	for _, tag := range []string{"1", "2"} {
		if _, err := imageCommand(ctx, []string{"pull", name(tag)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{a, b} {
		if _, err := ctx.Store.ReadImage("test", d); err != nil {
			t.Fatal("pull did not persist manifest/config:", err)
		}
	}
	before := reg.Count("/manifests/")
	// never must not contact the registry, including a missing explicit tag.
	if _, err := runImage(ctx, runOptionsForTest(t, []string{"--pull=never", name("missing")})); !errors.Is(err, store.ErrImageNotFound) {
		t.Fatalf("never missing: %v", err)
	}
	if reg.Count("/manifests/") != before {
		t.Fatal("never contacted registry")
	}
	if _, err := imageCommand(ctx, []string{"tag", a, name("copy")}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"tag", a, b}, {"push", a}, {"pull", a}} {
		if _, err := imageCommand(ctx, args); err == nil {
			t.Fatalf("used a local manifest ID as a remote name: %v", args)
		}
	}
	def, _ := ctx.Store.ReadRef("test.cli.fixture")
	if def.Manifest != b {
		t.Fatal("tag changed app default")
	}
	out.Reset()
	if _, err := imageCommand(ctx, []string{"inspect", "-f", "{{.Id}} {{.Quasidock.Version}}", name("copy")}); err != nil || strings.TrimSpace(out.String()) != a+" 1" {
		t.Fatalf("formatted inspect: %q %v", out.String(), err)
	}
	out.Reset()
	if _, err := imageCommand(ctx, []string{"inspect", name("1"), name("2")}); err != nil {
		t.Fatal(err)
	}
	var inspected []inspectedImage
	if err := json.Unmarshal(out.Bytes(), &inspected); err != nil || len(inspected) != 2 || inspected[0].Id != a || inspected[1].Id != b {
		t.Fatalf("inspect: %s %v", out.String(), err)
	}
	out.Reset()
	if _, err := imageCommand(ctx, []string{"ls", "-q"}); err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(out.String())) != 2 {
		t.Fatalf("quiet list duplicated aliases: %s", out.String())
	}
	// Publish an alias, then fetch it into a completely separate store.
	if _, err := imageCommand(ctx, []string{"push", name("copy")}); err != nil {
		t.Fatal(err)
	}
	other, _ := fixtureCLI(t)
	if _, err := pullImage(other, name("copy"), false); err != nil {
		t.Fatal(err)
	}
	got, err := other.Store.ResolveLocalImage("", name("copy"))
	if err != nil || got.Manifest != a {
		t.Fatalf("push selected wrong image: %v %v", got, err)
	}
	// An interrupted refresh never publishes the candidate's name/default.
	c, broken := fixtureImage(t, reg, "3", []byte("different layer"))
	_ = c
	reg.BreakBlob(broken.Digest)
	if _, err := pullImage(ctx, name("3"), false); err == nil {
		t.Fatal("broken layer pull succeeded")
	}
	if _, err := ctx.Store.ResolveLocalImage("", name("3")); !errors.Is(err, store.ErrImageNotFound) {
		t.Fatalf("failed pull published reference: %v", err)
	}
	def, _ = ctx.Store.ReadRef("test.cli.fixture")
	if def.Manifest != b {
		t.Fatal("failed pull changed default")
	}
	if !ctx.Store.HasBlob(layer.Digest) {
		t.Fatal("failed pull damaged old layer")
	}
	// Removing one version must preserve another version's shared deployment and
	// data, including after collection of the removed manifest/config blobs.
	dataDir := ctx.Store.Path("data", "test.cli.fixture")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "saved"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := imageCommand(ctx, []string{"rm", a}); err != nil {
		t.Fatal(err)
	}
	lock, err := ctx.Store.Lock(true)
	if err != nil {
		t.Fatal(err)
	}
	err = lifecycle.Prune(ctx, lifecycle.PruneOptions{Data: true})
	lock.Release()
	if err != nil {
		t.Fatal(err)
	}
	deploy, err := ctx.Store.DeployPath(layer.Digest)
	if err != nil || !store.Exists(deploy) {
		t.Fatal("removing one version lost shared deployment", err)
	}
	if !ctx.Store.HasBlob(layer.Digest) || !store.Exists(filepath.Join(dataDir, "saved")) {
		t.Fatal("prune lost another version's layer or app data")
	}
	reg.Fail(true)
	if _, err := runImage(ctx, runOptionsForTest(t, []string{"--pull=always", name("2")})); err == nil {
		t.Fatal("failed always pull ran cached image")
	}
}

func runOptionsForTest(t *testing.T, args []string) run.Options {
	t.Helper()
	opts, err := parseRunOptions(context.New(), args)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

func TestForegroundImages(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	buildDir := t.TempDir()
	fixture := filepath.Join(buildDir, "fixture.exe")
	cmd := exec.Command("go", "build", "-o", fixture, "../../tests/cmd/clifixture")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %s %v", out, err)
	}
	payload, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	reg := testregistry.New()
	defer reg.Close()
	a, _ := fixtureImage(t, reg, "1", payload)
	fixtureImage(t, reg, "2", payload)
	// Run actual CLI children so stdout, exit status and abrupt owner death are
	// tested through the same entrypoint as the shipped program.
	binary := filepath.Join(buildDir, "quasidock.exe")
	cmd = exec.Command("go", "build", "-o", binary, "../../cmd/quasidock")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %s %v", out, err)
	}
	call := func(args ...string) (string, int) {
		t.Helper()
		c := exec.Command(binary, args...)
		c.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
		var out, errout bytes.Buffer
		c.Stdout = &out
		c.Stderr = &errout
		err := c.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if code != 0 {
			return out.String() + errout.String(), code
		}
		return out.String(), code
	}
	name := reg.Host() + "/app:1"
	assertArgs := func(args []string, want []string) {
		t.Helper()
		out, code := call(args...)
		var got []string
		if code != 0 || json.Unmarshal([]byte(out), &got) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%v -> %s (exit %d), want %v", args, out, code, want)
		}
	}
	assertArgs([]string{"run", name}, []string{"default", "1"})
	requests := reg.Count("/manifests/")
	reg.Fail(true)
	assertArgs([]string{"run", name, "fixture.exe", "replacement", "--pull=always"}, []string{"replacement", "--pull=always"})
	assertArgs([]string{"run", "--pull=never", name, "fixture.exe", "offline"}, []string{"offline"})
	assertArgs([]string{"run", "--entrypoint=fixture.exe", a, "entrypoint"}, []string{"entrypoint"})
	assertArgs([]string{"run", "--entrypoint=args.cmd", name, "a b", "", `say "hello"`, `C:\a\`}, []string{"a b", "", `say "hello"`, `C:\a\`})
	if reg.Count("/manifests/") != requests {
		t.Fatal("cache hits contacted registry")
	}
	t.Setenv("CLI_INHERIT", "host")
	out, code := call("run", "-e", "CLI_INHERIT", "--env=REMOVE_ME", "-e", "EXPLICIT=one", "-eEXPLICIT=two", name, "fixture.exe", "env", "CLI_INHERIT", "REMOVE_ME", "EXPLICIT")
	if code != 0 || !strings.Contains(out, "CLI_INHERIT=host:true") || !strings.Contains(out, "REMOVE_ME=:false") || !strings.Contains(out, "EXPLICIT=two:true") {
		t.Fatalf("environment: %s %d", out, code)
	}
	if out, code := call("run", name, "fixture.exe", "exit", "37"); code != 37 {
		t.Fatalf("child exit: %s %d", out, code)
	}
	if out, code := call("run", name, "fixture.exe", "write", "shared"); code != 0 {
		t.Fatalf("write data: %s", out)
	}
	reg.Fail(false)
	if out, code := call("pull", reg.Host()+"/app:2"); code != 0 {
		t.Fatal(out)
	}
	if out, code := call("run", reg.Host()+"/app:2", "fixture.exe", "read"); code != 0 || out != "shared" {
		t.Fatalf("version data: %q %d", out, code)
	}
	if _, code := call("run", "--ephemeral", name, "fixture.exe", "read"); code != 3 {
		t.Fatalf("ephemeral reused app data: %d", code)
	}
	if _, code := call("run", name, "missing.exe"); code == 0 {
		t.Fatal("missing executable succeeded")
	}
	// A failed launch must release the app lease before another version starts.
	assertArgs([]string{"run", reg.Host() + "/app:2"}, []string{"default", "2"})
	ready := filepath.Join(buildDir, "ready")
	hold := exec.Command(binary, "run", name, "fixture.exe", "hold", ready)
	hold.Env = append(os.Environ(), "QUASIDOCK_HOME="+ctx.Store.Root)
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { hold.Process.Kill(); hold.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for !store.Exists(ready) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !store.Exists(ready) {
		t.Fatal("held child did not become ready")
	}
	if out, code := call("run", reg.Host()+"/app:2"); code == 0 || !strings.Contains(out, "another version") {
		t.Fatalf("conflicting run: %s %d", out, code)
	}
	assertArgs([]string{"run", name}, []string{"default", "1"})
	if out, code := call("rmi", a); code == 0 || !strings.Contains(out, "in use") {
		t.Fatalf("running image removal: %s %d", out, code)
	}
	if err := hold.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	hold.Wait()
	assertArgs([]string{"run", reg.Host() + "/app:2"}, []string{"default", "2"})
	if out, code := call("rmi", a); code != 0 {
		t.Fatal(out)
	}
	if out, code := call("run", reg.Host()+"/app:2", "fixture.exe", "read"); code != 0 || out != "shared" {
		t.Fatalf("removal lost shared data: %q %d", out, code)
	}
}

func TestRunParserCoreFlagsAndRetiredCommands(t *testing.T) {
	for _, args := range [][]string{{"--pull=bad", "app"}, {"--command=x", "app"}, {"-v", "app"}, {"-it", "app"}, {"--rm", "app"}, {"-e", "=bad", "app"}} {
		if _, err := parseRunOptions(context.New(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	opts := runOptionsForTest(t, []string{"--pull", "never", "-e", "NAME", "app", "--entrypoint=x"})
	if opts.Pull != "never" || opts.ID != "app" || !reflect.DeepEqual(opts.Args, []string{"--entrypoint=x"}) || !reflect.DeepEqual(opts.Settings.Env, []string{"NAME"}) {
		t.Fatalf("options: %+v", opts)
	}
	for old, replacement := range map[string]string{"list": "images", "rm": "rmi", "info": "image info", "ls": "image files", "cat": "image cat", "-v": "--verbose"} {
		_, err := Dispatch(context.New(), []string{old})
		if err == nil || !strings.Contains(err.Error(), replacement) {
			t.Fatalf("retired %s: %v", old, err)
		}
	}
}

func TestPullSignatureGatePreservesReferences(t *testing.T) {
	ctx, _ := fixtureCLI(t)
	reg := testregistry.New()
	defer reg.Close()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	public := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})
	trustBytes, err := json.Marshal(trust.File{Keys: []trust.Entry{{Prefix: reg.Host() + "/app", Key: string(public)}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctx.Store.Path("trust.json"), trustBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	name := reg.Host() + "/app:latest"
	a, _ := fixtureImage(t, reg, "1", []byte("first"))
	// Existing policy permits a missing signature, with a warning.
	if _, err := pullImage(ctx, name, false); err != nil {
		t.Fatal("missing signature should warn and install:", err)
	}
	sign := func(digest string, valid bool) {
		t.Helper()
		payload := []byte(signature.MakePayload(reg.Host()+"/app", digest))
		sum := sha256.Sum256(payload)
		sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		if !valid {
			sig[len(sig)-1] ^= 1
		}
		p := oci.Descriptor{MediaType: signature.PayloadMediaType, Digest: reg.PutBlob(payload), Size: int64(len(payload)), Annotations: map[string]string{signature.SignatureAnnotation: base64.StdEncoding.EncodeToString(sig)}}
		b, err := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Layers: []oci.Descriptor{p}})
		if err != nil {
			t.Fatal(err)
		}
		tag, err := signature.TagFor(digest)
		if err != nil {
			t.Fatal(err)
		}
		reg.PutManifest("app", tag, b)
	}
	b, layer := fixtureImage(t, reg, "2", []byte("second"))
	sign(b, false)
	before := reg.Count("/blobs/" + layer.Digest)
	if _, err := pullImage(ctx, name, false); err == nil || !strings.Contains(err.Error(), "Nothing was installed") {
		t.Fatalf("invalid signature: %v", err)
	}
	if reg.Count("/blobs/"+layer.Digest) != before {
		t.Fatal("rejected image downloaded a layer")
	}
	got, err := ctx.Store.ResolveLocalImage("", name)
	if err != nil || got.Manifest != a {
		t.Fatalf("rejected refresh moved reference: %v %v", got, err)
	}
	sign(b, true)
	if _, err := pullImage(ctx, name, false); err != nil {
		t.Fatal("valid signature refused:", err)
	}
	got, err = ctx.Store.ResolveLocalImage("", name)
	if err != nil || got.Manifest != b {
		t.Fatalf("signed refresh: %v %v", got, err)
	}
}
