package recipe

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/format"
)

func TestParseMinimalRecipe(t *testing.T) {
	raw := `{
  "id": "net.frippery.busybox",
  "version": "1",
  "command": "busybox.exe",
  "modules": [{ "name": "busybox", "sources": [{ "url": "https://example.com/bb.exe?x=1", "dest": "busybox.exe" }] }]
}`
	r, err := Parse("test.json", []byte(raw))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if r.Command != "busybox.exe" {
		t.Fatalf("expected command 'busybox.exe', got %q", r.Command)
	}
	if len(r.Modules) != 1 || len(r.Modules[0].Sources) != 1 {
		t.Fatalf("unexpected modules or sources: %+v", r.Modules)
	}
	src := r.Modules[0].Sources[0]
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
	if len(cfg.Exports) != 1 {
		t.Fatalf("expected 1 export, got %d", len(cfg.Exports))
	}
	exp, ok := cfg.Exports["busybox"]
	if !ok || exp.Command != "busybox.exe" {
		t.Fatalf("expected export 'busybox' -> 'busybox.exe', got %+v", exp)
	}
	if r.Layer.Format() != format.FormatGzip {
		t.Fatalf("expected FormatGzip, got %v", r.Layer.Format())
	}
}

func TestLayerFormat(t *testing.T) {
	raw := `{ "id": "x", "version": "1", "command": "a.exe", "layer": "estargz",
  "modules": [{ "name": "m", "sources": [{ "url": "https://example.com/a.exe", "dest": "a.exe" }] }] }`
	r, err := Parse("t.json", []byte(raw))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if r.Layer.Format() != format.FormatEstargz {
		t.Fatalf("expected FormatEstargz, got %v", r.Layer.Format())
	}

	for _, name := range []string{"tar", "zstd"} {
		badJSON := fmt.Sprintf(`{ "id": "x", "version": "1", "command": "a.exe", "layer": "%s",
  "modules": [{ "name": "m", "sources": [{ "url": "https://example.com/a.exe", "dest": "a.exe" }] }] }`, name)
		if _, err := Parse("t.json", []byte(badJSON)); err == nil {
			t.Fatalf("expected error for layer format %q, got nil", name)
		}
	}
}

func TestPrefetch(t *testing.T) {
	okJSON := `{ "id": "x", "version": "1", "command": "a.exe", "layer": "estargz", "prefetch": ["a.exe", "lib/b.dll"],
  "modules": [{ "name": "m", "sources": [{ "url": "https://example.com/a.exe", "dest": "a.exe" }] }] }`
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
		raw := fmt.Sprintf(`{ "id": "x", "version": "1", "command": "a.exe", %s,
  "modules": [{ "name": "m", "sources": [{ "url": "https://example.com/a.exe", "dest": "a.exe" }] }] }`, fields)
		if _, err := Parse("t.json", []byte(raw)); err == nil {
			t.Fatalf("expected error for prefetch %s, got nil", fields)
		}
	}
}

func TestRecipeChecks(t *testing.T) {
	okJSON := `{ "id": "x.tsc", "version": "1", "command": "${node}\\node.exe", "args": ["${app}\\tsc.js"],
  "runtimes": { "node": "org.nodejs.node@sha256:0000000000000000000000000000000000000000000000000000000000000000" },
  "exports": { "tsc": { "command": "${node}\\node.exe", "args": ["${app}\\tsc.js"] } },
  "cleanup": ["/include", "*.pdb"],
  "modules": [{ "name": "tsc", "sources": [{ "path": "tsc.tgz", "strip": 1 }] }] }`
	ok, err := Parse("t.json", []byte(okJSON))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if ok.Modules[0].Sources[0].SourceKind() != KindTarGz {
		t.Fatalf("expected KindTarGz, got %v", ok.Modules[0].Sources[0].SourceKind())
	}

	vendoredJSON := `{ "id": "x", "version": "1", "command": "a.exe",
  "modules": [{ "name": "m", "build": ["x"], "vendor": { "commands": ["cargo vendor"], "dir": "vendor/crates" } }] }`
	vendored, err := Parse("t.json", []byte(vendoredJSON))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if vendored.Modules[0].Vendor == nil || vendored.Modules[0].Vendor.Dir != "vendor/crates" {
		t.Fatalf("expected vendor dir 'vendor/crates', got %+v", vendored.Modules[0].Vendor)
	}

	fromImageJSON := `{ "id": "x", "version": "1", "command": "bin/a.exe", "cleanup": ["/include", "*.a"],
  "modules": [{ "name": "m", "sources": [{ "image": "x.sdk@sha256:0000000000000000000000000000000000000000000000000000000000000000", "dest": "sdk" }] }] }`
	fromImage, err := Parse("t.json", []byte(fromImageJSON))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if fromImage.Modules[0].Sources[0].Dest == nil || *fromImage.Modules[0].Sources[0].Dest != "sdk" {
		t.Fatalf("expected dest 'sdk', got %v", fromImage.Modules[0].Sources[0].Dest)
	}

	projectJSON := `{ "id": "x", "version": "1", "command": "bin/a.exe", "exports": {},
  "modules": [{ "name": "m", "sources": [{ "path": ".", "skip": ["/docs", "*.log"] }, { "path": "src/" }], "build": ["x"] }] }`
	project, err := Parse("zigsaw.json", []byte(projectJSON))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(project.Modules[0].Sources[0].Skip) != 2 || project.Modules[0].Sources[0].Skip[0] != "/docs" {
		t.Fatalf("expected skip '/docs', got %v", project.Modules[0].Sources[0].Skip)
	}

	bad := []string{
		// A command in a runtime needs exports.
		`{ "id": "x", "version": "1", "command": "${node}\\node.exe", "runtimes": { "node": "n" }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		// An unknown alias.
		`{ "id": "x", "version": "1", "command": "${node}\\node.exe", "exports": {}, "modules": [{ "name": "m", "build": ["x"] }] }`,
		// A reserved alias.
		`{ "id": "x", "version": "1", "command": "a.exe", "runtimes": { "data": "d" }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		// The same alias twice.
		`{ "id": "x", "version": "1", "command": "a.exe", "sdk": { "zig": "z" }, "runtimes": { "zig": "z" }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		// No modules, or an empty one.
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m" }] }`,
		// Two modules with one name.
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "build": ["x"] }, { "name": "M", "build": ["y"] }] }`,
		// Network for a module that builds nothing.
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "network": true, "sources": [{ "path": "a.exe" }] }] }`,
		// strip on a file.
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "sources": [{ "path": "a.exe", "strip": 1 }] }] }`,
		// An image source: on its own, pinned by digest only, inside the app.
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "sources": [{ "image": "x.sdk@sha256:00", "path": "a.exe" }] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "sources": [{ "image": "x.sdk@sha256:00", "sha256": "00" }] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "sources": [{ "image": "x.sdk@sha256:00", "strip": 1 }] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "sources": [{ "image": "x.sdk@sha256:00", "dest": "../up" }] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "sources": [{ "image": "x.sdk@sha256:00", "skip": ["/a"] }] }] }`,
		// skip: for a directory path, with cleanup's patterns.
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "sources": [{ "url": "https://example.com/a.exe", "skip": ["/a"] }] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "sources": [{ "path": ".", "skip": ["docs/*.md"] }] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "sources": [{ "path": ".", "skip": ["/../x"] }] }] }`,
		// A URL with no file name, and none given.
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "sources": [{ "url": "https://example.com/" }] }] }`,
		// Cleanup patterns.
		`{ "id": "x", "version": "1", "command": "a.exe", "cleanup": ["/../x"], "modules": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "cleanup": ["lib/*.a"], "modules": [{ "name": "m", "build": ["x"] }] }`,
		// Host toolchains: only msvc, and only for build commands.
		`{ "id": "x", "version": "1", "command": "a.exe", "host": ["gcc"], "modules": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "host": ["msvc"], "modules": [{ "name": "m", "sources": [{ "path": "a.exe" }] }] }`,
		// Aliases: plain names, for commands in the image itself or in one of its runtimes.
		`{ "id": "x", "version": "1", "command": "a.exe", "aliases": { "a/b": { "command": "a.exe" } }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "aliases": { "ab": { "command": "${go}\\go.exe" } }, "runtimes": { "node": "n" }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		// Dropped arguments: only for aliases, one trimmed line each.
		`{ "id": "x", "version": "1", "command": "a.exe", "exports": { "a": { "command": "a.exe", "drop": ["-x"] } }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "aliases": { "a": { "command": "a.exe", "drop": [" -x"] } }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "aliases": { "a": { "command": "a.exe", "drop": [""] } }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		// Vendor steps: only with build commands, with commands, into a directory inside the module's, pinned by a well-formed hash.
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "sources": [{ "path": "a.exe" }], "vendor": { "commands": ["x"], "dir": "v" } }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "build": ["x"], "vendor": { "commands": [], "dir": "v" } }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "build": ["x"], "vendor": { "commands": ["x"], "dir": "." } }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "build": ["x"], "vendor": { "commands": ["x"], "dir": "../v" } }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "modules": [{ "name": "m", "build": ["x"], "vendor": { "commands": ["x"], "dir": "v", "sha256": "ABC" } }] }`,
		// Shortcuts: to an export, named so they can be file names, with an icon inside the app.
		`{ "id": "x", "version": "1", "command": "a.exe", "shortcuts": { "A": { "command": "b" } }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "shortcuts": { "A/B": { "command": "a" } }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "shortcuts": { "A": { "command": "a" }, "a": { "command": "a" } }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "shortcuts": { "A": { "command": "a", "icon": "../a.ico" } }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		// Associations: an extension, opened by an export, once, with an icon inside the app.
		`{ "id": "x", "version": "1", "command": "a.exe", "associations": { ".a": { "command": "b" } }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "associations": { "a": { "command": "a" } }, "modules": [{ "name": "m", "build": ["x"] }] }`,
		`{ "id": "x", "version": "1", "command": "a.exe", "associations": { ".a": { "command": "a", "icon": "C:\\a.ico" } }, "modules": [{ "name": "m", "build": ["x"] }] }`,
	}

	for _, text := range bad {
		if _, err := Parse("t.json", []byte(text)); err == nil {
			t.Fatalf("expected error for %s, got nil", text)
		}
	}

	with, err := Parse("t.json", []byte(`{ "id": "x", "version": "1", "command": "a.exe", "shortcuts": { "Text Editor": { "command": "a", "description": "Edit" } },
  "modules": [{ "name": "m", "build": ["x"] }] }`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	withCfg, err := with.AppConfig(nil, nil)
	if err != nil {
		t.Fatalf("AppConfig error: %v", err)
	}
	withJSON, err := json.Marshal(withCfg)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if !strings.Contains(string(withJSON), `"Text Editor"`) {
		t.Fatalf("expected 'Text Editor' in JSON, got %s", string(withJSON))
	}

	vendoredCfg, err := vendored.AppConfig(nil, nil)
	if err != nil {
		t.Fatalf("AppConfig error: %v", err)
	}
	withoutJSON, err := json.Marshal(vendoredCfg)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if strings.Contains(string(withoutJSON), `"shortcuts"`) {
		t.Fatalf("did not expect 'shortcuts' in JSON, got %s", string(withoutJSON))
	}

	opening, err := Parse("t.json", []byte(`{ "id": "x", "version": "1", "command": "a.exe", "associations": { ".note": { "command": "a", "description": "Note" } },
  "modules": [{ "name": "m", "build": ["x"] }] }`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	openingCfg, err := opening.AppConfig(nil, nil)
	if err != nil {
		t.Fatalf("AppConfig error: %v", err)
	}
	openingJSON, err := json.Marshal(openingCfg)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if !strings.Contains(string(openingJSON), `".note"`) {
		t.Fatalf("expected '.note' in JSON, got %s", string(openingJSON))
	}
	if strings.Contains(string(withoutJSON), `"associations"`) {
		t.Fatalf("did not expect 'associations' in JSON, got %s", string(withoutJSON))
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
