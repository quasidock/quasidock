package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

func TestHelp(t *testing.T) {
	ctx := context.New()
	code, err := Dispatch(ctx, []string{"help"})
	if err != nil || code != 0 {
		t.Fatalf("expected code 0, got %d, err %v", code, err)
	}
}

func TestVersion(t *testing.T) {
	ctx := context.New()
	code, err := Dispatch(ctx, []string{"version"})
	if err != nil || code != 0 {
		t.Fatalf("expected code 0, got %d, err %v", code, err)
	}
}

func TestParseRunOptions(t *testing.T) {
	ctx := context.New()
	opts, err := parseRunOptions(ctx, []string{
		"--command=test.exe",
		"--file=C:\\foo.txt",
		"--share=network",
		"--sandbox=soft",
		"org.example.app",
		"arg1",
		"arg2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.ID != "org.example.app" {
		t.Fatalf("expected ID org.example.app, got %q", opts.ID)
	}
	if opts.Command == nil || *opts.Command != "test.exe" {
		t.Fatalf("expected command test.exe, got %v", opts.Command)
	}
	if len(opts.Files) != 1 || opts.Files[0] != "C:\\foo.txt" {
		t.Fatalf("expected 1 file C:\\foo.txt, got %v", opts.Files)
	}
	if len(opts.Args) != 2 || opts.Args[0] != "arg1" || opts.Args[1] != "arg2" {
		t.Fatalf("expected args [arg1 arg2], got %v", opts.Args)
	}
	if opts.Settings.Network == nil || !*opts.Settings.Network {
		t.Fatalf("expected network share to be true")
	}
}

func TestUsageError(t *testing.T) {
	ctx := context.New()
	code, err := Dispatch(ctx, []string{"nonexistent-cmd"})
	if err != nil {
		t.Fatalf("expected nil error on usage error, got %v", err)
	}
	if code != 1 {
		t.Fatalf("expected code 1, got %d", code)
	}
}

func TestIsFlag(t *testing.T) {
	if !isFlag("-v", "verbose") {
		t.Fatal("expected -v to match verbose")
	}
	if !isFlag("--verbose", "verbose") {
		t.Fatal("expected --verbose to match verbose")
	}
	if !isFlag("--rebuild", "rebuild") {
		t.Fatal("expected --rebuild to match rebuild")
	}
	if isFlag("-r", "rebuild") {
		t.Fatal("expected -r not to match rebuild")
	}
}

func TestOverrideWarnsUnsupported(t *testing.T) {
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	why := "the test says it can't"
	cfg := oci.AppConfig{
		ID:      "test.override.app",
		Version: "1",
		Command: "app.exe",
		Permissions: oci.Permissions{
			Sandbox:     oci.SandboxSoft,
			Unsupported: &oci.Unsupported{AppContainer: &why},
		},
	}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgDesc, err := st.PutBlob(cfgBytes, oci.MediaTypeConfig)
	if err != nil {
		t.Fatal(err)
	}
	layerDesc, err := st.PutBlob([]byte("layer"), oci.MediaTypeLayerTar)
	if err != nil {
		t.Fatal(err)
	}
	manifest := oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeManifest,
		Config:        cfgDesc,
		Layers:        []oci.Descriptor{layerDesc},
	}
	manBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manDesc, err := st.PutBlob(manBytes, oci.MediaTypeManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadImage(cfg.ID, manDesc.Digest); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteRef(store.Ref{ID: cfg.ID, Version: cfg.Version, Manifest: manDesc.Digest}); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	var outBuf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&outBuf, r)
		close(done)
	}()

	ctx := context.New()
	ctx.Store = st
	err = overrideApp(ctx, []string{"--sandbox=appcontainer", cfg.ID})

	_ = w.Close()
	os.Stderr = oldStderr
	<-done
	_ = r.Close()

	if err != nil {
		t.Fatalf("overrideApp returned error: %v", err)
	}
	got := outBuf.String()
	want := "warning: test.override.app can't run under the appcontainer sandbox: the test says it can't"
	if !strings.Contains(got, want) {
		t.Fatalf("expected stderr to contain %q, got %q", want, got)
	}
}
