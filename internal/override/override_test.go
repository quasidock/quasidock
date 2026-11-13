package override

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

func boolPtr(b bool) *bool {
	return &b
}

func sandboxPtr(s oci.Sandbox) *oci.Sandbox {
	return &s
}

func TestSettingsMerge(t *testing.T) {
	saved := Settings{
		Sandbox:    sandboxPtr(oci.SandboxAppContainer),
		Ephemeral:  boolPtr(true),
		Filesystem: []string{"D:\\src", "cwd"},
		Env:        []string{"A=1", "B=1"},
	}
	cli := Settings{
		Network:    boolPtr(false),
		Ephemeral:  boolPtr(false),
		Filesystem: []string{"d:\\SRC:ro"},
		Env:        []string{"a=2", "C=3", "C=4"},
	}

	merged := Merge(saved, cli)
	if merged.Sandbox == nil || *merged.Sandbox != oci.SandboxAppContainer {
		t.Fatalf("expected sandbox appcontainer")
	}
	if merged.Network == nil || *merged.Network != false {
		t.Fatalf("expected network false")
	}
	if merged.Ephemeral == nil || *merged.Ephemeral != false {
		t.Fatalf("expected ephemeral false")
	}

	expectedFilesystem := []string{"cwd", "d:\\SRC:ro"}
	if len(merged.Filesystem) != len(expectedFilesystem) || merged.Filesystem[0] != "cwd" || merged.Filesystem[1] != "d:\\SRC:ro" {
		t.Fatalf("filesystem mismatch: %v", merged.Filesystem)
	}

	expectedEnv := []string{"B=1", "a=2", "C=4"}
	if len(merged.Env) != len(expectedEnv) || merged.Env[0] != "B=1" || merged.Env[1] != "a=2" || merged.Env[2] != "C=4" {
		t.Fatalf("env mismatch: %v", merged.Env)
	}

	fmtStr := merged.Format()
	wantFmt := "--sandbox=appcontainer --unshare=network --filesystem=cwd --filesystem=d:\\SRC:ro --env=B=1 --env=a=2 --env=C=4"
	if fmtStr != wantFmt {
		t.Fatalf("format mismatch:\ngot:  %s\nwant: %s", fmtStr, wantFmt)
	}

	if !(Settings{}).IsEmpty() {
		t.Fatal("expected empty settings to return true for IsEmpty")
	}
}

func TestSettingsNofilesystem(t *testing.T) {
	app := []string{"cwd", "documents", "downloads:ro"}

	// An override takes documents away, and makes downloads writable.
	saved := Settings{Filesystem: []string{"downloads"}, NoFilesystem: []string{"Documents"}}
	fs := saved.FilesystemFor(app)
	if len(fs) != 2 || fs[0] != "cwd" || fs[1] != "downloads" {
		t.Fatalf("unexpected filesystem: %v", fs)
	}

	// Command line gives documents back, read-only, and takes cwd away.
	cli := Settings{Filesystem: []string{"documents:ro"}, NoFilesystem: []string{"cwd"}}
	merged := Merge(saved, cli)
	if len(merged.Filesystem) != 2 || merged.Filesystem[0] != "downloads" || merged.Filesystem[1] != "documents:ro" {
		t.Fatalf("unexpected merged filesystem: %v", merged.Filesystem)
	}
	if len(merged.NoFilesystem) != 1 || merged.NoFilesystem[0] != "cwd" {
		t.Fatalf("unexpected nofilesystem: %v", merged.NoFilesystem)
	}
}

func TestSettingsNoextensionAndPorts(t *testing.T) {
	saved := Settings{NoExtension: []string{"net.php.php.Xdebug", "net.php.php.APCu"}}
	cli := Settings{Extension: []string{"NET.php.php.xdebug"}, NoExtension: []string{"x.y"}}
	merged := Merge(saved, cli)
	if len(merged.NoExtension) != 2 || merged.NoExtension[0] != "net.php.php.APCu" || merged.NoExtension[1] != "x.y" {
		t.Fatalf("unexpected noextension: %v", merged.NoExtension)
	}
	if len(merged.Extension) != 0 {
		t.Fatalf("expected 0 extension, got: %v", merged.Extension)
	}

	// Ports
	sPorts := Merge(Settings{Ports: []string{"http=9090", "admin=9091"}}, Settings{Ports: []string{"http=8080"}})
	if len(sPorts.Ports) != 2 || sPorts.Ports[0] != "admin=9091" || sPorts.Ports[1] != "http=8080" {
		t.Fatalf("unexpected ports: %v", sPorts.Ports)
	}

	name, port, ok := ParsePort("postgresql=5433")
	if !ok || name != "postgresql" || port != 5433 {
		t.Fatalf("unexpected ParsePort: %s %d %v", name, port, ok)
	}
	for _, bad := range []string{"postgresql", "=5433", "pg=0", "pg=65536", "pg=x"} {
		if _, _, ok := ParsePort(bad); ok {
			t.Fatalf("expected false for %s", bad)
		}
	}
}

func TestSettingsStorage(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := store.Open(tmpDir, false, "")
	if err != nil {
		t.Fatal(err)
	}

	appID := "com.example.test"
	saved := Settings{
		Sandbox:    sandboxPtr(oci.SandboxAppContainer),
		Network:    boolPtr(true),
		Filesystem: []string{"D:\\src:ro"},
	}

	if err := Save(st, appID, saved); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := Load(st, appID)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded.Sandbox == nil || *loaded.Sandbox != oci.SandboxAppContainer {
		t.Fatalf("unexpected sandbox: %v", loaded.Sandbox)
	}

	// Delete
	if err := Delete(st, appID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	loaded, err = Load(st, appID)
	if err != nil || !loaded.IsEmpty() {
		t.Fatalf("expected empty after delete, got: %+v (err %v)", loaded, err)
	}
}

func TestSettingsJSONRoundtrip(t *testing.T) {
	s := Settings{
		Sandbox:    sandboxPtr(oci.SandboxAppContainer),
		Network:    boolPtr(true),
		Filesystem: []string{"D:\\src:ro"},
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "extension") {
		t.Fatalf("expected empty extension lists omitted from JSON: %s", string(data))
	}

	var parsed Settings
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Sandbox == nil || *parsed.Sandbox != oci.SandboxAppContainer {
		t.Fatalf("parsed mismatch")
	}
}
