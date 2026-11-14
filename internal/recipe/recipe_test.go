package recipe

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/format"
)

func TestParseMinimalRecipe(t *testing.T) {
	raw := `{
  "id": "busybox",
  "version": "1",
  "entrypoint": "busybox.exe",
  "stages": [{ "name": "busybox", "sources": [{ "url": "https://example.com/bb.exe?x=1", "dest": "busybox.exe" }] }]
}`
	r, err := Parse("test.json", []byte(raw))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if !reflect.DeepEqual([]string(r.Entrypoint), []string{"busybox.exe"}) {
		t.Fatalf("expected entrypoint ['busybox.exe'], got %v", r.Entrypoint)
	}
	if len(r.Stages) != 1 || len(r.Stages[0].Sources) != 1 {
		t.Fatalf("unexpected stages or sources: %+v", r.Stages)
	}
	src := r.Stages[0].Sources[0]
	if src.SourceKind() != KindFile {
		t.Fatalf("expected KindFile, got %v", src.SourceKind())
	}
	if src.FileName() != "bb.exe" {
		t.Fatalf("expected fileName 'bb.exe', got %q", src.FileName())
	}
	if r.Permissions.Network {
		t.Fatalf("expected network=false")
	}
	if r.HasBuildCommands() {
		t.Fatalf("expected hasBuildCommands=false")
	}

	cfg, err := r.AppConfig(nil, nil)
	if err != nil {
		t.Fatalf("AppConfig error: %v", err)
	}
	if !reflect.DeepEqual(cfg.Entrypoint, []string{"busybox.exe"}) {
		t.Fatalf("expected config entrypoint ['busybox.exe'], got %v", cfg.Entrypoint)
	}
	if r.Layer.Format() != format.FormatGzip {
		t.Fatalf("expected FormatGzip, got %v", r.Layer.Format())
	}
}

func TestLayerFormat(t *testing.T) {
	raw := `{ "id": "x", "version": "1", "entrypoint": "a.exe", "layer": "estargz",
  "stages": [{ "name": "m", "sources": [{ "url": "https://example.com/a.exe", "dest": "a.exe" }] }] }`
	r, err := Parse("t.json", []byte(raw))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if r.Layer.Format() != format.FormatEstargz {
		t.Fatalf("expected FormatEstargz, got %v", r.Layer.Format())
	}

	for _, name := range []string{"tar", "zstd"} {
		badJSON := fmt.Sprintf(`{ "id": "x", "version": "1", "entrypoint": "a.exe", "layer": "%s",
  "stages": [{ "name": "m", "sources": [{ "url": "https://example.com/a.exe", "dest": "a.exe" }] }] }`, name)
		if _, err := Parse("t.json", []byte(badJSON)); err == nil {
			t.Fatalf("expected error for layer format %q, got nil", name)
		}
	}
}

func TestPrefetch(t *testing.T) {
	okJSON := `{ "id": "x", "version": "1", "entrypoint": "a.exe", "layer": "estargz", "prefetch": ["a.exe", "lib/b.dll"],
  "stages": [{ "name": "m", "sources": [{ "url": "https://example.com/a.exe", "dest": "a.exe" }] }] }`
	ok, err := Parse("t.json", []byte(okJSON))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(ok.Prefetch) != 2 {
		t.Fatalf("expected 2 prefetch items, got %d", len(ok.Prefetch))
	}

	badFields := []string{
		`"prefetch": ["a.exe"]`,
		`"layer": "estargz", "prefetch": ["../a.exe"]`,
		`"layer": "estargz", "prefetch": ["/a.exe"]`,
		`"layer": "estargz", "prefetch": ["lib\\b.dll"]`,
		`"layer": "estargz", "prefetch": ["lib//b.dll"]`,
		`"layer": "estargz", "prefetch": ["C:/a.exe"]`,
		`"layer": "estargz", "prefetch": ["a.exe", "A.EXE"]`,
	}

	for _, fields := range badFields {
		raw := fmt.Sprintf(`{ "id": "x", "version": "1", "entrypoint": "a.exe", %s,
  "stages": [{ "name": "m", "sources": [{ "url": "https://example.com/a.exe", "dest": "a.exe" }] }] }`, fields)
		if _, err := Parse("t.json", []byte(raw)); err == nil {
			t.Fatalf("expected error for prefetch %s, got nil", fields)
		}
	}
}

func TestRecipeChecks(t *testing.T) {
	okJSON := `{ "id": "tsc", "version": "1", "entrypoint": ["${node}\\node.exe", "${app}\\tsc.js"], "cmd": ["--help"],
  "from": { "node": "node@sha256:0000000000000000000000000000000000000000000000000000000000000000" },
  "cleanup": ["/include", "*.pdb"],
  "stages": [{ "name": "tsc", "sources": [{ "path": "tsc.tgz", "strip": 1 }] }] }`
	ok, err := Parse("t.json", []byte(okJSON))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if !reflect.DeepEqual([]string(ok.Entrypoint), []string{"${node}\\node.exe", "${app}\\tsc.js"}) {
		t.Fatalf("unexpected entrypoint %v", ok.Entrypoint)
	}
	if !reflect.DeepEqual(ok.Cmd, []string{"--help"}) {
		t.Fatalf("unexpected cmd %v", ok.Cmd)
	}
	if ok.Stages[0].Sources[0].SourceKind() != KindTarGz {
		t.Fatalf("expected KindTarGz, got %v", ok.Stages[0].Sources[0].SourceKind())
	}

	vendoredJSON := `{ "id": "x", "version": "1", "entrypoint": "a.exe",
  "stages": [{ "name": "m", "build": ["x"], "vendor": { "commands": ["cargo vendor"], "dir": "vendor/crates" } }] }`
	vendored, err := Parse("t.json", []byte(vendoredJSON))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if vendored.Stages[0].Vendor == nil || vendored.Stages[0].Vendor.Dir != "vendor/crates" {
		t.Fatalf("expected vendor dir 'vendor/crates', got %+v", vendored.Stages[0].Vendor)
	}

	fromImageJSON := `{ "id": "x", "version": "1", "entrypoint": "bin/a.exe", "cleanup": ["/include", "*.a"],
  "stages": [{ "name": "m", "sources": [{ "image": "x-sdk@sha256:0000000000000000000000000000000000000000000000000000000000000000", "dest": "sdk" }] }] }`
	fromImage, err := Parse("t.json", []byte(fromImageJSON))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if fromImage.Stages[0].Sources[0].Dest == nil || *fromImage.Stages[0].Sources[0].Dest != "sdk" {
		t.Fatalf("expected dest 'sdk', got %v", fromImage.Stages[0].Sources[0].Dest)
	}

	projectJSON := `{ "id": "x", "version": "1", "entrypoint": ["bin/a.exe"],
  "stages": [{ "name": "m", "sources": [{ "path": ".", "skip": ["/docs", "*.log"] }, { "path": "src/" }], "build": ["x"] }] }`
	project, err := Parse("quasidock.json", []byte(projectJSON))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(project.Stages[0].Sources[0].Skip) != 2 || project.Stages[0].Sources[0].Skip[0] != "/docs" {
		t.Fatalf("expected skip '/docs', got %v", project.Stages[0].Sources[0].Skip)
	}

	bad := []string{
		// Empty entrypoint.
		`{ "id": "x", "version": "1", "entrypoint": "", "stages": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "entrypoint": [], "stages": [{ "name": "m", "build": ["x"] }] }`,
		// An unknown alias.
		`{ "id": "x", "version": "1", "entrypoint": "${node}\\node.exe", "stages": [{ "name": "m", "build": ["x"] }] }`,
		// A reserved alias.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "from": { "data": "d" }, "stages": [{ "name": "m", "build": ["x"] }] }`,
		// The same alias twice.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "sdk": { "zig": "z" }, "from": { "zig": "z" }, "stages": [{ "name": "m", "build": ["x"] }] }`,
		// No stages, or an empty one.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m" }] }`,
		// Two stages with one name.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "build": ["x"] }, { "name": "M", "build": ["y"] }] }`,
		// Network for a stage that builds nothing.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "network": true, "sources": [{ "path": "a.exe" }] }] }`,
		// strip on a file.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "sources": [{ "path": "a.exe", "strip": 1 }] }] }`,
		// An image source: on its own, pinned by digest only, inside the app.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "sources": [{ "image": "x.sdk@sha256:00", "path": "a.exe" }] }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "sources": [{ "image": "x.sdk@sha256:00", "sha256": "00" }] }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "sources": [{ "image": "x.sdk@sha256:00", "strip": 1 }] }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "sources": [{ "image": "x.sdk@sha256:00", "dest": "../up" }] }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "sources": [{ "image": "x.sdk@sha256:00", "skip": ["/a"] }] }] }`,
		// skip: for a directory path, with cleanup's patterns.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "sources": [{ "url": "https://example.com/a.exe", "skip": ["/a"] }] }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "sources": [{ "path": ".", "skip": ["docs/*.md"] }] }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "sources": [{ "path": ".", "skip": ["/../x"] }] }] }`,
		// A URL with no file name, and none given.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "sources": [{ "url": "https://example.com/" }] }] }`,
		// Cleanup patterns.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "cleanup": ["/../x"], "stages": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "cleanup": ["lib/*.a"], "stages": [{ "name": "m", "build": ["x"] }] }`,
		// Host toolchains: only msvc, and only for build commands.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "host": ["gcc"], "stages": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "host": ["msvc"], "stages": [{ "name": "m", "sources": [{ "path": "a.exe" }] }] }`,
		// Vendor steps: only with build commands, with commands, into a directory inside the stage's, pinned by a well-formed hash.
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "sources": [{ "path": "a.exe" }], "vendor": { "commands": ["x"], "dir": "v" } }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "build": ["x"], "vendor": { "commands": [], "dir": "v" } }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "build": ["x"], "vendor": { "commands": ["x"], "dir": "." } }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "build": ["x"], "vendor": { "commands": ["x"], "dir": "../v" } }] }`,
		`{ "id": "x", "version": "1", "entrypoint": "a.exe", "stages": [{ "name": "m", "build": ["x"], "vendor": { "commands": ["x"], "dir": "v", "sha256": "ABC" } }] }`,
	}

	for _, text := range bad {
		if _, err := Parse("t.json", []byte(text)); err == nil {
			t.Fatalf("expected error for %s, got nil", text)
		}
	}
}

func TestArchiveKindsAreInferred(t *testing.T) {
	cases := []struct {
		url  string
		kind Kind
	}{
		{"https://example.com/node-v22-win-x64.ZIP", KindZip},
		{"https://registry.npmjs.org/prettier/-/prettier-3.9.9.tgz", KindTarGz},
		{"zlib-1.3.1.tar.gz", KindTarGz},
		{"x.tar.xz", KindTarXz},
		{"x.tar", KindTar},
		{"https://static.crates.io/crates/ripgrep/ripgrep-15.2.0.crate", KindTarGz},
		{"tool.exe", KindFile},
	}

	for _, c := range cases {
		s := Source{URL: &c.url}
		if s.SourceKind() != c.kind {
			t.Fatalf("for %s: expected %v, got %v", c.url, c.kind, s.SourceKind())
		}
	}
}

func TestCleanupMatches(t *testing.T) {
	patterns := []string{"/include", "/lib/pkgconfig", "*.pdb", "__pycache__"}
	if !CleanupMatches(patterns, "include") {
		t.Fatalf("expected match for 'include'")
	}
	if !CleanupMatches(patterns, "Include/zlib.h") {
		t.Fatalf("expected match for 'Include/zlib.h'")
	}
	if CleanupMatches(patterns, "includes/x.h") {
		t.Fatalf("unexpected match for 'includes/x.h'")
	}
	if !CleanupMatches(patterns, "lib/pkgconfig/zlib.pc") {
		t.Fatalf("expected match for 'lib/pkgconfig/zlib.pc'")
	}
	if CleanupMatches(patterns, "lib/libz.a") {
		t.Fatalf("unexpected match for 'lib/libz.a'")
	}
	if !CleanupMatches(patterns, "bin/sqlite3.PDB") {
		t.Fatalf("expected match for 'bin/sqlite3.PDB'")
	}
	if !CleanupMatches(patterns, "lib/x/__pycache__/a.pyc") {
		t.Fatalf("expected match for 'lib/x/__pycache__/a.pyc'")
	}
	if CleanupMatches(patterns, "bin/sqlite3.exe") {
		t.Fatalf("unexpected match for 'bin/sqlite3.exe'")
	}
}

func TestGlobMatch(t *testing.T) {
	if !GlobMatch("*.a", "libz.a") {
		t.Fatalf("expected match *.a libz.a")
	}
	if GlobMatch("*.a", "libz.a.txt") {
		t.Fatalf("unexpected match *.a libz.a.txt")
	}
	if !GlobMatch("lib*.dll", "LIBZ.DLL") {
		t.Fatalf("expected match lib*.dll LIBZ.DLL")
	}
	if !GlobMatch("a?c", "abc") {
		t.Fatalf("expected match a?c abc")
	}
	if GlobMatch("a?c", "ac") {
		t.Fatalf("unexpected match a?c ac")
	}
	if !GlobMatch("*", "") {
		t.Fatalf("expected match * ''")
	}
	if !GlobMatch("*x*y*", "axbyc") {
		t.Fatalf("expected match *x*y* axbyc")
	}
	if GlobMatch("*x*y", "axbyc") {
		t.Fatalf("unexpected match *x*y axbyc")
	}
}

func TestSourceMirrorsRequirePinnedRegistryImages(t *testing.T) {
	for _, tc := range []struct {
		mirror string
		valid  bool
	}{
		{"ghcr.io/old/app@sha256:" + strings.Repeat("a", 64), true},
		{"ghcr.io/old/app:latest", false},
		{"app@sha256:" + strings.Repeat("a", 64), false},
		{"https://example.com/archive.tar.xz", false},
		{"ghcr.io/old/app@sha256:bad", false},
	} {
		data := `{"id":"app","version":"1","entrypoint":"app.exe","source_mirrors":["` + tc.mirror + `"],"stages":[{"name":"main","sources":[{"path":"app.exe"}]}]}`
		_, err := Parse("app.json", []byte(data))
		if (err == nil) != tc.valid {
			t.Fatalf("source mirror %q: %v", tc.mirror, err)
		}
	}
}

func TestAllRecipesParse(t *testing.T) {
	var files []string
	for _, pattern := range []string{"../../recipes/*.json", "../../recipes/no-publish/*.json"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		files = append(files, matches...)
	}
	if len(files) != 73 {
		t.Fatalf("expected 73 recipes, got %d", len(files))
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		r, err := Parse(path, data)
		if err != nil {
			t.Fatalf("Parse(%s): %v", path, err)
		}
		stem := strings.TrimSuffix(filepath.Base(path), ".json")
		if r.ID != stem {
			t.Fatalf("%s: id = %q, want %q", path, r.ID, stem)
		}
	}
}
