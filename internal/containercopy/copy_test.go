package containercopy

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/mounts"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func fixture(t *testing.T, specs ...mounts.Spec) (*store.Store, *store.ContainerRecord) {
	t.Helper()
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.CreateContainer("example", store.ContainerRecord{HostConfig: store.ContainerHostConfig{Mounts: specs}})
	if err != nil {
		t.Fatal(err)
	}
	return st, c
}

func put(t *testing.T, root, name, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertFile(t *testing.T, name, expected string) {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil || string(b) != expected {
		t.Fatalf("%s: got %q, %v; want %q", name, b, err, expected)
	}
}

func TestEndpointPaths(t *testing.T) {
	for _, name := range []string{"example:/home/file", `example:home\file`, "example:./home/file"} {
		e, err := parseEndpoint(name)
		if err != nil || e.container != "example" || e.name != "home/file" {
			t.Fatalf("%q: %+v %v", name, e, err)
		}
	}
	for _, name := range []string{"", "example:", "example:../escape", "example:a/../escape", "example:/C:/host", `example:\\server\share`, "example:/a:stream", "example:/NUL", "example:/COM¹", "example:/trailing.", "example:/trailing ", `\\.\NUL`, "C:\\data\\file:stream", `C:\data\trailing.`, `C:\data\trailing `} {
		if _, err := parseEndpoint(name); err == nil {
			t.Errorf("accepted unsafe endpoint %q", name)
		}
	}
	for _, name := range []string{`C:\data\file`, "C:/data/file", "C:relative", `\\server\share\file`, ".\\relative"} {
		e, err := parseEndpoint(name)
		if err != nil || e.container != "" || !filepath.IsAbs(e.name) {
			t.Fatalf("host %q: %+v %v", name, e, err)
		}
	}
}

func TestCopyPlacement(t *testing.T) {
	cases := []struct {
		name                       string
		directory                  bool
		existing, suffix, expected string
		fail                       bool
	}{
		{"file renamed", false, "missing", "", "dest", false},
		{"file overwritten", false, "file", "", "dest", false},
		{"file into dir", false, "dir", "", "dest/source", false},
		{"file missing dir", false, "missing", "/", "", true},
		{"file directory hint", false, "file", "/", "", true},
		{"directory new", true, "missing", "", "dest/nested/日本語.txt", false},
		{"directory nested", true, "dir", "", "dest/source/nested/日本語.txt", false},
		{"directory contents", true, "dir", "/.", "dest/nested/日本語.txt", false},
		{"directory contents new", true, "missing", "/.", "dest/nested/日本語.txt", false},
		{"directory to file", true, "file", "", "", true},
	}
	for _, outbound := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("out=%t/%s", outbound, tc.name), func(t *testing.T) {
				st, c := fixture(t)
				host := t.TempDir()
				from, to := host, c.DataDir
				if outbound {
					from, to = c.DataDir, host
				}
				if tc.directory {
					put(t, from, "source/nested/日本語.txt", "payload")
					if err := os.Mkdir(filepath.Join(from, "source", "empty"), 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					put(t, from, "source", "payload")
				}
				if tc.existing == "file" {
					put(t, to, "dest", "old")
				}
				if tc.existing == "dir" {
					put(t, to, "dest/unrelated", "keep")
				}
				src, dst := filepath.Join(host, "source"), "example:/dest"
				if outbound {
					src, dst = "example:/source", filepath.Join(host, "dest")
				}
				if tc.suffix == "/." {
					src += tc.suffix
				} else {
					dst += tc.suffix
				}
				err := Copy(context.Background(), st, src, dst, nil, nil)
				if tc.fail {
					if err == nil {
						t.Fatal("expected placement error")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				assertFile(t, filepath.Join(to, filepath.FromSlash(tc.expected)), "payload")
				if tc.existing == "dir" {
					assertFile(t, filepath.Join(to, "dest", "unrelated"), "keep")
				}
				if tc.directory {
					base := filepath.Dir(filepath.Dir(filepath.Join(to, filepath.FromSlash(tc.expected))))
					if info, err := os.Stat(filepath.Join(base, "empty")); err != nil || !info.IsDir() {
						t.Fatalf("empty dir missing: %v", err)
					}
				}
			})
		}
	}
}

func TestCopyParentsTimesAndRoot(t *testing.T) {
	st, c := fixture(t)
	host := t.TempDir()
	p := put(t, host, "source/file", "payload")
	mtime := time.Unix(1234567890, 0)
	for _, name := range []string{p, filepath.Dir(p)} {
		if err := os.Chtimes(name, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := Copy(context.Background(), st, p, "example:/missing/parent/file", nil, nil); err == nil {
		t.Fatal("created missing parents")
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed copy created parents", err)
	}
	if err := Copy(context.Background(), st, filepath.Dir(p)+"/.", "example:/", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{c.DataDir, filepath.Join(c.DataDir, "file")} {
		info, err := os.Stat(name)
		if err != nil || !info.ModTime().Equal(mtime) {
			t.Fatalf("mtime %s: %v %v", name, info, err)
		}
	}
	out := filepath.Join(t.TempDir(), "export")
	if err := Copy(context.Background(), st, c.ShortID()+":/", out, nil, nil); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(out, "file"), "payload")
}

func TestDeclaredMounts(t *testing.T) {
	for _, typ := range []string{"volume", "bind"} {
		for _, target := range []string{"${data}", "${data}/Parent/work"} {
			for _, attached := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/attached=%t", typ, target, attached), func(t *testing.T) {
					source := "work"
					if typ == "bind" {
						source = t.TempDir()
					}
					st, c := fixture(t, mounts.Spec{Type: typ, Source: source, Target: target})
					m := c.Mounts[0]
					put(t, m.Source, "mounted.txt", "mounted")
					if attached {
						if err := mounts.Attach(c.DataDir, m); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = mounts.Detach(c.DataDir, m) })
					} else if target != "${data}" {
						// The logical ancestor is missing; copying must synthesize it.
						if _, err := os.Stat(filepath.Join(c.DataDir, "Parent")); !errors.Is(err, os.ErrNotExist) {
							t.Fatal(err)
						}
					}
					out := filepath.Join(t.TempDir(), "export")
					if err := Copy(context.Background(), st, "example:/", out, nil, nil); err != nil {
						t.Fatal(err)
					}
					rel := strings.TrimPrefix(strings.TrimPrefix(target, "${data}"), "/")
					assertFile(t, filepath.Join(out, filepath.FromSlash(rel), "mounted.txt"), "mounted")
					input := put(t, t.TempDir(), "input.txt", "input")
					if err := Copy(context.Background(), st, input, "example:/"+join(strings.ToLower(rel), "new.txt"), nil, nil); err != nil {
						t.Fatal(err)
					}
					assertFile(t, filepath.Join(m.Source, "new.txt"), "input")
					if target != "${data}" && !attached {
						put(t, c.DataDir, "Parent/work/hidden.txt", "private")
						var archive bytes.Buffer
						if err := Copy(context.Background(), st, "example:/", "-", nil, &archive); err != nil {
							t.Fatal(err)
						}
						if bytes.Contains(archive.Bytes(), []byte("hidden.txt")) {
							t.Fatal("private mount contents were exposed")
						}
					}
				})
			}
		}
	}
}

func TestMissingMountSource(t *testing.T) {
	bind := t.TempDir()
	st, _ := fixture(t, mounts.Spec{Type: "bind", Source: bind, Target: "${data}/work"})
	if err := os.Remove(bind); err != nil {
		t.Fatal(err)
	}
	if err := Copy(context.Background(), st, "example:/work", filepath.Join(t.TempDir(), "out"), nil, nil); err == nil {
		t.Fatal("accepted missing mount source")
	}
	if _, err := os.Stat(bind); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recreated mount source")
	}
}

func TestOverlapAndHardlinks(t *testing.T) {
	bind := t.TempDir()
	st, c := fixture(t, mounts.Spec{Type: "bind", Source: bind, Target: "${data}/work"})
	source := put(t, bind, "source", "outside")
	for _, pair := range [][2]string{{bind, "example:/work/child"}, {"example:/work", filepath.Join(bind, "child")}, {source, "example:/work/source"}} {
		if err := Copy(context.Background(), st, pair[0], pair[1], nil, nil); err == nil {
			t.Fatal("accepted overlap", pair)
		}
	}
	put(t, c.DataDir, "private", "replacement")
	linked := filepath.Join(c.DataDir, "linked")
	if err := os.Link(source, linked); err != nil {
		t.Fatal(err)
	}
	if err := Copy(context.Background(), st, "example:/private", source, nil, nil); err != nil {
		t.Fatal(err)
	}
	assertFile(t, source, "replacement")
	assertFile(t, linked, "outside")
}

func TestReparseAndSharingFailures(t *testing.T) {
	st, c := fixture(t)
	outside := t.TempDir()
	put(t, outside, "sentinel", "safe")
	m := mounts.Mount{Source: outside, Destination: filepath.Join(c.DataDir, "link")}
	if err := mounts.Attach(c.DataDir, m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounts.Detach(c.DataDir, m) })
	input := put(t, t.TempDir(), "input", "new")
	for _, pair := range [][2]string{{"example:/link", filepath.Join(t.TempDir(), "out")}, {input, "example:/link/sentinel"}, {"example:/", filepath.Join(t.TempDir(), "out")}} {
		if err := Copy(context.Background(), st, pair[0], pair[1], nil, nil); err == nil {
			t.Fatal("followed reparse point", pair)
		}
	}
	assertFile(t, filepath.Join(outside, "sentinel"), "safe")
	h, err := win32.CreateFile(input, win32.GENERIC_READ, 0, nil, win32.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(h)
	if err := Copy(context.Background(), st, input, "example:/new", nil, nil); err == nil {
		t.Fatal("copied exclusively locked source")
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed copy left a file")
	}
}

func TestCopyOperationLockAndCancellation(t *testing.T) {
	st, c := fixture(t)
	put(t, c.DataDir, "file", "data")
	op, err := st.LockContainer(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if err := Copy(ctx, st, "example:/file", filepath.Join(t.TempDir(), "out"), nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lock wait did not cancel", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- Copy(context.Background(), st, "example:/file", filepath.Join(st.Root, "out"), nil, nil)
	}()
	select {
	case err := <-done:
		t.Fatal("bypassed operation lock", err)
	case <-time.After(40 * time.Millisecond):
	}
	if err := op.Remove(); err != nil {
		t.Fatal(err)
	}
	op.Release()
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrContainerNotFound) {
			t.Fatal("used stale container record", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("copy did not finish")
	}
}

func makeTar(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, h := range headers {
		if h.ModTime.IsZero() {
			h.ModTime = time.Unix(1234567890, 0)
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA {
			if _, err := w.Write(bytes.Repeat([]byte{'x'}, int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTarRoundTrip(t *testing.T) {
	st, c := fixture(t)
	put(t, c.DataDir, "dir/nested/日本語.txt", "\x00binary\xff\n")
	if err := os.Mkdir(filepath.Join(c.DataDir, "dir", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Copy(context.Background(), st, "example:/dir", "-", nil, &out); err != nil {
		t.Fatal(err)
	}
	var names []string
	r := tar.NewReader(bytes.NewReader(out.Bytes()))
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if want := []string{"dir/", "dir/empty/", "dir/nested/", "dir/nested/日本語.txt"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("tar names: %v", names)
	}
	if err := os.Mkdir(filepath.Join(c.DataDir, "restore"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Copy(context.Background(), st, "-", "example:/restore", bytes.NewReader(out.Bytes()), nil); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(c.DataDir, "restore", "dir", "nested", "日本語.txt"), "\x00binary\xff\n")
	if err := Copy(context.Background(), st, "-", "example:/absent", bytes.NewReader(out.Bytes()), nil); err == nil {
		t.Fatal("tar accepted missing destination")
	}
	nested := makeTar(t, &tar.Header{Name: "a/b/file", Typeflag: tar.TypeReg, Size: 3})
	if err := Copy(context.Background(), st, "-", "example:/restore", bytes.NewReader(nested), nil); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(c.DataDir, "restore", "a", "b", "file"), "xxx")
}

func TestTarValidationBeforeWrites(t *testing.T) {
	good := &tar.Header{Name: "first", Typeflag: tar.TypeReg, Size: 3}
	cases := map[string][]byte{}
	for _, name := range []string{"../escape", "/absolute", `C:\escape`, `a\..\escape`, "NUL", "file:stream", "trailing. ", "FIRST"} {
		cases[name] = makeTar(t, good, &tar.Header{Name: name, Typeflag: tar.TypeReg, Size: 2})
	}
	for _, typ := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeFifo, tar.TypeChar} {
		cases[fmt.Sprintf("type %d", typ)] = makeTar(t, good, &tar.Header{Name: "special", Typeflag: typ, Linkname: "first"})
	}
	cases["conflict"] = makeTar(t, good, &tar.Header{Name: "first/child", Typeflag: tar.TypeReg, Size: 1})
	full := makeTar(t, good, &tar.Header{Name: "second", Typeflag: tar.TypeReg, Size: 4096})
	cases["truncated"] = full[:1537]
	cases["empty input"] = nil
	cases["missing end blocks"] = makeTar(t, good)[:1024]
	cases["incomplete zero padding"] = append(makeTar(t, good), 0)
	cases["trailing garbage"] = append(makeTar(t, good), []byte("garbage")...)
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			st, c := fixture(t)
			put(t, c.DataDir, "first", "original")
			if err := Copy(context.Background(), st, "-", "example:/", bytes.NewReader(payload), nil); err == nil {
				t.Fatal("accepted invalid tar")
			}
			assertFile(t, filepath.Join(c.DataDir, "first"), "original")
			entries, err := os.ReadDir(c.DataDir)
			if err != nil || len(entries) != 1 {
				t.Fatal("archive validation changed destination", entries, err)
			}
			staging, _ := filepath.Glob(st.Path("tmp", "container-copy*"))
			if len(staging) != 0 {
				t.Fatal("staging leaked", staging)
			}
		})
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestTarOutputFailures(t *testing.T) {
	st, c := fixture(t)
	p := put(t, c.DataDir, "file", "original")
	failure := errors.New("output disconnected")
	if err := Copy(context.Background(), st, "example:/file", "-", nil, writerFunc(func([]byte) (int, error) { return 0, failure })); !errors.Is(err, failure) {
		t.Fatal("lost output error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := Copy(ctx, st, "example:/file", "-", nil, writerFunc(func(p []byte) (int, error) { cancel(); return len(p), nil })); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation", err)
	}
	var changed bool
	err := Copy(context.Background(), st, "example:/file", "-", nil, writerFunc(func(b []byte) (int, error) {
		if !changed {
			changed = true
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return 0, err
			}
			_, err = f.WriteString("changed")
			_ = f.Close()
			if err != nil {
				return 0, err
			}
		}
		return len(b), nil
	}))
	if err == nil || !strings.Contains(err.Error(), "changed during copy") {
		t.Fatal("source change was not detected", err)
	}
}

func TestFailedReplacementPreservesDestination(t *testing.T) {
	st, c := fixture(t)
	source := put(t, t.TempDir(), "file", "replacement")
	dest := put(t, c.DataDir, "file", "original")
	// Sharing read access permits preflight, but withholding delete sharing
	// prevents publication after the new file has been fully written.
	h, err := win32.CreateFile(dest, win32.GENERIC_READ, win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE, nil, win32.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer win32.CloseHandle(h)
	if err := Copy(context.Background(), st, source, "example:/file", nil, nil); err == nil {
		t.Fatal("replaced a file without delete sharing")
	}
	assertFile(t, dest, "original")
	temps, err := filepath.Glob(filepath.Join(c.DataDir, ".quasidock-cp-*"))
	if err != nil || len(temps) != 0 {
		t.Fatal("temporary replacement file leaked", temps, err)
	}
}

func TestRootedDestinationSurvivesReparseMutation(t *testing.T) {
	st, c := fixture(t)
	outside := t.TempDir()
	put(t, outside, "sentinel", "safe")
	if err := os.Mkdir(filepath.Join(c.DataDir, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs, err := containerFilesystem(st, c)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.close()
	g, root, err := fs.directory("target", false)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	defer func() { _ = root.Close() }()
	m := mounts.Mount{Source: outside, Destination: filepath.Join(c.DataDir, "target")}
	// Some Windows configurations reject in-place changes while a directory is
	// open. Otherwise the already-open Root must fail or refer to the original
	// directory, rather than following the newly installed junction.
	if err := mounts.Attach(c.DataDir, m); err != nil {
		return
	}
	created := false
	defer func() {
		if created {
			_ = root.Remove("sentinel")
		}
		_ = root.Close()
		g.close()
		fs.close()
		_ = mounts.Detach(c.DataDir, m)
	}()
	f, err := root.OpenFile("sentinel", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		assertFile(t, filepath.Join(outside, "sentinel"), "safe")
		return
	}
	created = true
	if _, err := f.WriteString("private"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	assertFile(t, filepath.Join(outside, "sentinel"), "safe")
}

func TestEmptyTarAndLateReadError(t *testing.T) {
	st, c := fixture(t)
	if err := Copy(context.Background(), st, "-", "example:/", bytes.NewReader(makeTar(t)), nil); err != nil {
		t.Fatal("valid empty tar failed", err)
	}
	failure := errors.New("input disconnected")
	reader := io.MultiReader(bytes.NewReader(makeTar(t, &tar.Header{Name: "file", Typeflag: tar.TypeReg, Size: 3})), errorReader{failure})
	if err := Copy(context.Background(), st, "-", "example:/", reader, nil); !errors.Is(err, failure) {
		t.Fatal("lost late read error", err)
	}
	if entries, err := os.ReadDir(c.DataDir); err != nil || len(entries) != 0 {
		t.Fatal("late read failure changed destination", entries, err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
