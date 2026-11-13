package aliases

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/sidecar"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

func TestResolve(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "zigsaw-aliases-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	appDir := filepath.Join(tmpDir, "app")
	rtDir := filepath.Join(tmpDir, "rt")
	_ = os.MkdirAll(appDir, 0755)
	_ = os.MkdirAll(rtDir, 0755)

	_ = os.WriteFile(filepath.Join(appDir, "tool.exe"), []byte(""), 0755)
	_ = os.WriteFile(filepath.Join(rtDir, "zig.exe"), []byte(""), 0755)

	st, err := store.Open(tmpDir, false, "")
	if err != nil {
		t.Fatal(err)
	}

	appAliases := map[string]oci.Export{
		"CC": {Command: "tool.exe"},
	}
	rtAliases := map[string]oci.Export{
		"cc":   {Command: "zig.exe", Args: []string{"cc"}},
		"ar":   {Command: "zig.exe", Args: []string{"ar", "${app}\\lib"}, Drop: []string{"--64"}},
		"gone": {Command: "bin/gone.exe"},
	}

	providers := []Provider{
		{ID: "app", Dir: appDir, Aliases: appAliases},
		{ID: "rt", Dir: rtDir, Aliases: rtAliases},
		{ID: "none", Dir: rtDir, Aliases: nil},
	}

	_, err = Resolve(st, providers, MissingFail, true)
	if err == nil {
		t.Error("expected error for missing bin/gone.exe")
	}

	resolved, err := Resolve(st, providers, MissingSkip, true)
	if err != nil {
		t.Fatalf("Resolve with skip failed: %v", err)
	}

	if len(resolved) != 2 {
		t.Fatalf("len(resolved) = %d; want 2", len(resolved))
	}
	if resolved[0].Name != "CC" {
		t.Errorf("resolved[0].Name = %q", resolved[0].Name)
	}
	if resolved[0].Alias.Exe != filepath.Join(appDir, "tool.exe") {
		t.Errorf("resolved[0].Alias.Exe = %q", resolved[0].Alias.Exe)
	}
	if resolved[1].Name != "ar" {
		t.Errorf("resolved[1].Name = %q", resolved[1].Name)
	}

	zig := filepath.Join(rtDir, "zig.exe")
	wantCmd := zig + ` ar ` + rtDir + `\lib`
	if resolved[1].Alias.CommandLine != wantCmd {
		t.Errorf("resolved[1].CommandLine = %q, want %q", resolved[1].Alias.CommandLine, wantCmd)
	}
	if len(resolved[1].Alias.Drop) == 0 || resolved[1].Alias.Drop[0] != "--64" {
		t.Errorf("drop = %v", resolved[1].Alias.Drop)
	}
	if Names(resolved) != "CC ar" {
		t.Errorf("Names = %q, want 'CC ar'", Names(resolved))
	}

	// Runtime interpolation in command
	uses := map[string]oci.Export{
		"zc": {Command: "${z}\\zig.exe", Args: []string{"cc", "${app}\\x.c", "${z}\\lib"}},
	}
	user := []Provider{
		{
			ID:       "user",
			Dir:      appDir,
			Aliases:  uses,
			Runtimes: []oci.DirPlaceholder{{Alias: "z", Path: rtDir}},
		},
	}
	zc, err := Resolve(st, user, MissingFail, true)
	if err != nil {
		t.Fatal(err)
	}
	if zc[0].Alias.Exe != zig {
		t.Errorf("zc Exe = %q, want %q", zc[0].Alias.Exe, zig)
	}
	wantZcCmd := zig + ` cc ` + appDir + `\x.c ` + rtDir + `\lib`
	if zc[0].Alias.CommandLine != wantZcCmd {
		t.Errorf("zc CommandLine = %q, want %q", zc[0].Alias.CommandLine, wantZcCmd)
	}
}

func TestSync(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "zigsaw-sync-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := filepath.Join(tmpDir, "f.shim")
	w, err := ReplaceIfDifferent(p, []byte("one"), false)
	if err != nil || !w {
		t.Fatalf("first write failed: %v", err)
	}
	w, err = ReplaceIfDifferent(p, []byte("one"), false)
	if err != nil || w {
		t.Fatalf("second write should not write: %v", err)
	}
	w, err = ReplaceIfDifferent(p, []byte("two"), false)
	if err != nil || !w {
		t.Fatalf("third write failed: %v", err)
	}

	aliasesDir := filepath.Join(tmpDir, "aliases")
	_ = os.MkdirAll(aliasesDir, 0755)
	_ = os.WriteFile(filepath.Join(aliasesDir, "old.exe"), []byte("x"), 0644)
	_ = os.WriteFile(filepath.Join(aliasesDir, "old.shim"), []byte("x"), 0644)
	_ = os.WriteFile(filepath.Join(aliasesDir, "cc.shim.1234abcd.tmp"), []byte("x"), 0644)

	written, err := Sync(aliasesDir, nil, nil, false)
	if err != nil || written != 0 {
		t.Fatalf("Sync empty failed: %v, written: %d", err, written)
	}

	entries, _ := os.ReadDir(aliasesDir)
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}

	cc := Resolved{
		Name: "cc",
		Alias: sidecar.Alias{
			Exe:         `C:\z\zig.exe`,
			CommandLine: `C:\z\zig.exe cc`,
		},
	}
	res := []Resolved{cc}
	if !IsWanted(res, "cc.exe") {
		t.Error("expected true")
	}
	if !IsWanted(res, "cc.SHIM") {
		t.Error("expected true")
	}
	if !IsWanted(res, "CC.exe") {
		t.Error("expected true")
	}
	if IsWanted(res, "c.exe") {
		t.Error("expected false")
	}
	if IsWanted(res, "cc.exe.0a1b2c3d.tmp") {
		t.Error("expected false")
	}
	if IsWanted(res, ".exe") {
		t.Error("expected false")
	}
}
