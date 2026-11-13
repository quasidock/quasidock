package cli

import (
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/context"
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
