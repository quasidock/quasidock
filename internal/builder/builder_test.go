package builder

import (
	"strings"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/environment"
)

func TestIsBuildRoot(t *testing.T) {
	if !IsBuildRoot(`\??\C:\Users\me\AppData\Local\zigsaw\tmp\zigsaw-build-0a1b2c3d4e5f6a7b`) {
		t.Fatalf("expected isBuildRoot true")
	}
	if !IsBuildRoot(`\??\D:\z\tmp\zigsaw-build-0a1b\`) {
		t.Fatalf("expected isBuildRoot true")
	}
	if IsBuildRoot(`\Device\HarddiskVolume3`) {
		t.Fatalf("expected isBuildRoot false")
	}
	if IsBuildRoot(`\??\C:\work`) {
		t.Fatalf("expected isBuildRoot false")
	}
}

func TestFromSetOutput(t *testing.T) {
	host := map[string]string{
		"Path": `C:\Windows\System32;C:\Tools`,
		"TEMP": `C:\Temp`,
	}
	out := "INCLUDE=C:\\VS\\include;C:\\SDK\\include\r\n" +
		"Path=C:\\VS\\bin;C:\\SDK\\bin\\;C:\\Windows\\System32;C:\\Tools\\\r\n" +
		"TEMP=C:\\Temp\r\n" +
		"VCToolsVersion=14.51.36231\r\n" +
		"WindowsSDKVersion=10.0.26100.0\\\r\n"

	tc, err := FromSetOutput(host, out)
	if err != nil {
		t.Fatalf("FromSetOutput failed: %v", err)
	}
	if tc == nil {
		t.Fatalf("expected non-nil toolchain")
	}
	if len(tc.Path) != 2 || tc.Path[0] != `C:\VS\bin` || tc.Path[1] != `C:\SDK\bin\` {
		t.Fatalf("unexpected path: %v", tc.Path)
	}
	if tc.ToolsVersion != "14.51.36231" {
		t.Fatalf("expected tools version 14.51.36231, got %s", tc.ToolsVersion)
	}
	if tc.SDKVersion != "10.0.26100.0" {
		t.Fatalf("expected sdk version 10.0.26100.0, got %s", tc.SDKVersion)
	}

	none, err := FromSetOutput(host, "TEMP=C:\\Temp\r\n")
	if err != nil || none != nil {
		t.Fatalf("expected nil toolchain when no VCToolsVersion")
	}
}

func TestGitignoreWildmatch(t *testing.T) {
	cases := []struct {
		pattern string
		text    string
		match   bool
	}{
		{"*.a", "libz.a", true},
		{"*.a", "libz.a.txt", false},
		{"*.a", "dir/libz.a", false},
		{"foo/**/bar", "foo/bar", true},
		{"foo/**/bar", "foo/a/bar", true},
		{"foo/**/bar", "foo/a/b/bar", true},
		{"**/foo", "foo", true},
		{"**/foo", "a/foo", true},
		{"[a-z]", "k", true},
		{"[a-z]", "K", true}, // case-insensitive
		{"[!a-z]", "1", true},
		{"[!a-z]", "a", false},
	}

	for _, c := range cases {
		got := Wildmatch(c.pattern, c.text)
		if got != c.match {
			t.Fatalf("Wildmatch(%q, %q) = %v, want %v", c.pattern, c.text, got, c.match)
		}
	}
}

func TestGitignoreRules(t *testing.T) {
	rules := GitignoreRules{}
	pats := ParseGitignore([]byte("target/\n*.log\n!important.log\n/root_only.txt\n"))
	rules.Push("", pats)

	if !rules.Ignored("target", true) {
		t.Fatalf("expected target/ directory ignored")
	}
	if rules.Ignored("target", false) {
		t.Fatalf("target file should not match dir-only target/")
	}
	if !rules.Ignored("app.log", false) {
		t.Fatalf("expected app.log ignored")
	}
	if !rules.Ignored("sub/deep/app.log", false) {
		t.Fatalf("expected sub/deep/app.log ignored")
	}
	if rules.Ignored("important.log", false) {
		t.Fatalf("expected important.log not ignored due to negation")
	}
	if !rules.Ignored("root_only.txt", false) {
		t.Fatalf("expected /root_only.txt ignored at root")
	}
	if rules.Ignored("sub/root_only.txt", false) {
		t.Fatalf("expected anchored /root_only.txt not ignored in subfolder")
	}
}

func TestToolsAdd(t *testing.T) {
	a := "sha256:" + strings.Repeat("a", 64)
	b := "sha256:" + strings.Repeat("b", 64)

	makeTool := func(what, image, id string) Tool {
		return Tool{
			Image:   image,
			ID:      id,
			Version: "1",
			What:    what,
			Dir:     `C:\d`,
		}
	}

	ts := &Tools{recipePath: "r.json"}
	idx, err := ts.Add(makeTool("sdk x", a, "org.x"))
	if err != nil || idx == nil || *idx != 0 {
		t.Fatalf("expected index 0, got %v, err %v", idx, err)
	}

	// Adding same image again should return nil
	idx, err = ts.Add(makeTool("runtime x", a, "org.x"))
	if err != nil || idx != nil {
		t.Fatalf("expected nil for duplicate image, got %v", idx)
	}

	// Adding same id with different image should fail
	_, err = ts.Add(makeTool("sdk y", b, "ORG.X"))
	if err == nil {
		t.Fatalf("expected error for same ID different image")
	}

	// Adding different id should succeed
	idx, err = ts.Add(makeTool("sdk y", b, "org.y"))
	if err != nil || idx == nil || *idx != 1 {
		t.Fatalf("expected index 1, got %v, err %v", idx, err)
	}
	if len(ts.list) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(ts.list))
	}
}

func TestToolVars(t *testing.T) {
	makeTool := func(id, val string, brings int) Tool {
		env := map[string]string{
			"X": val,
			id:  "${app}",
		}
		return Tool{
			Image:   id,
			ID:      id,
			Version: "1",
			What:    id,
			Dir:     id,
			Env:     env,
			Brings:  brings,
		}
	}

	getVar := func(vars []environment.Var, name string) string {
		for _, v := range vars {
			if v.Name == name {
				return v.Value
			}
		}
		return ""
	}

	// Two plain tools: later wins
	v1, err := computeToolVars([]Tool{makeTool("a", "a", 0), makeTool("b", "b", 0)}, "")
	if err != nil {
		t.Fatalf("computeToolVars failed: %v", err)
	}
	if getVar(v1, "X") != "b" {
		t.Fatalf("expected X='b', got %q", getVar(v1, "X"))
	}

	// SDK and runtime it brings: SDK wins over its brought runtime
	v2, err := computeToolVars([]Tool{makeTool("sdk", "sdk", 1), makeTool("rt", "rt", 0)}, "")
	if err != nil {
		t.Fatalf("computeToolVars failed: %v", err)
	}
	if getVar(v2, "X") != "sdk" {
		t.Fatalf("expected X='sdk', got %q", getVar(v2, "X"))
	}
	if getVar(v2, "rt") != "rt" {
		t.Fatalf("expected rt='rt', got %q", getVar(v2, "rt"))
	}

	// SDK, brought runtime, then another tool: later tool wins
	v3, err := computeToolVars([]Tool{makeTool("sdk", "sdk", 1), makeTool("rt", "rt", 0), makeTool("c", "c", 0)}, "")
	if err != nil {
		t.Fatalf("computeToolVars failed: %v", err)
	}
	if getVar(v3, "X") != "c" {
		t.Fatalf("expected X='c', got %q", getVar(v3, "X"))
	}
}

func TestShellCommandLines(t *testing.T) {
	bb := Shell{exe: `C:\z\busybox.exe`, kind: ShellBusybox}
	expectedBB := `C:\z\busybox.exe sh -c "cp a \"$PREFIX/b c\""`
	if bb.CommandLine(`cp a "$PREFIX/b c"`) != expectedBB {
		t.Fatalf("expected %q, got %q", expectedBB, bb.CommandLine(`cp a "$PREFIX/b c"`))
	}

	cmd := Shell{exe: `C:\Windows\System32\cmd.exe`, kind: ShellCmd}
	expectedCmd := `C:\Windows\System32\cmd.exe /d /s /c "cl /nologo "a b.c" && copy x %PREFIX%"`
	if cmd.CommandLine(`cl /nologo "a b.c" && copy x %PREFIX%`) != expectedCmd {
		t.Fatalf("expected %q, got %q", expectedCmd, cmd.CommandLine(`cl /nologo "a b.c" && copy x %PREFIX%`))
	}
}
