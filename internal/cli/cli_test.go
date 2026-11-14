package cli

import (
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/context"
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
		"--filesystem=cwd",
		"--filesystem=C:\\Work:ro",
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
	if len(opts.Settings.Filesystem) != 2 || opts.Settings.Filesystem[0] != "cwd" || opts.Settings.Filesystem[1] != "C:\\Work:ro" {
		t.Fatalf("unexpected filesystem grants: %v", opts.Settings.Filesystem)
	}
	if len(opts.Args) != 2 || opts.Args[0] != "arg1" || opts.Args[1] != "arg2" {
		t.Fatalf("expected args [arg1 arg2], got %v", opts.Args)
	}
	if opts.Settings.Network == nil || !*opts.Settings.Network {
		t.Fatalf("expected network share to be true")
	}
}

func TestParseRunOptionsRejectsRemovedFlags(t *testing.T) {
	cases := [][]string{
		{"--file=C:\\foo.txt", "org.example.app"},
		{"--filesystem=documents", "org.example.app"},
		{"--nofilesystem=downloads", "org.example.app"},
		{"--extension=org.example.ext", "org.example.app"},
		{"--noextension=org.example.ext", "org.example.app"},
	}
	for _, tc := range cases {
		ctx := context.New()
		if _, err := parseRunOptions(ctx, tc); err == nil {
			t.Fatalf("expected error for %v, got nil", tc)
		}
	}
}

func TestUsageError(t *testing.T) {
	for _, cmd := range []string{"nonexistent-cmd", "override"} {
		ctx := context.New()
		code, err := Dispatch(ctx, []string{cmd})
		if err != nil {
			t.Fatalf("expected nil error on usage error for %q, got %v", cmd, err)
		}
		if code != 1 {
			t.Fatalf("expected code 1 for %q, got %d", cmd, code)
		}
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
	if !strings.Contains(usage, "--filesystem=<location>[:ro]") {
		t.Fatal("usage text missing --filesystem description")
	}
}

func TestParsePullOptions(t *testing.T) {
	opts, err := parsePullOptions([]string{"--yes", "--lazy", "net.frippery.busybox"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Image != "net.frippery.busybox" || !opts.Lazy {
		t.Fatalf("unexpected options: %+v", opts)
	}

	optsNoYes, err := parsePullOptions([]string{"net.frippery.busybox"})
	if err != nil {
		t.Fatalf("unexpected error without --yes: %v", err)
	}
	if optsNoYes.Image != "net.frippery.busybox" || optsNoYes.Lazy {
		t.Fatalf("unexpected options without --yes: %+v", optsNoYes)
	}

	for _, bad := range [][]string{
		{},
		{"--unknown", "net.frippery.busybox"},
		{"app1", "app2"},
	} {
		if _, err := parsePullOptions(bad); err == nil {
			t.Fatalf("expected error for %v, got nil", bad)
		}
	}

	if !strings.Contains(usage, "pull [--lazy] <image>") {
		t.Fatal("usage text missing updated pull description")
	}
}
