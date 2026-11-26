package cli

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestNetworkArguments(t *testing.T) {
	ctx := context.New()
	var out bytes.Buffer
	ctx.Stdout = &out
	for _, args := range [][]string{
		nil, {"rm", "example"}, {"create"}, {"create", "one", "two"},
		{"create", ""}, {"create", ".hidden"}, {"create", "white space"},
		{"create", "path/name"}, {"create", "héllo"}, {"create", strings.Repeat("a", 129)},
		{"ls", "example"}, {"list", "--quiet=true"},
	} {
		if code, err := networkCommand(ctx, args, networkBackend{}); code != 1 || err == nil || out.Len() != 0 {
			t.Errorf("%v: exit %d, error %v, output %q", args, code, err, out.String())
		}
	}
	// An invalid network command must reach its parser without opening the store.
	root := filepath.Join(t.TempDir(), "unopened-store")
	t.Setenv("QUASIDOCK_HOME", root)
	if code, err := Dispatch(ctx, []string{"network", "ls", "--invalid"}); code != 1 || err == nil || !strings.Contains(err.Error(), "unknown network ls option") {
		t.Fatalf("network dispatch: exit %d, error %v", code, err)
	}
	if _, err := os.Stat(root); ctx.Store != nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("network command opened the local store: %v", err)
	}
}

func TestNetworkCreate(t *testing.T) {
	const id = "0abcd012-1234-5678-90ab-bcdef0123456"
	ctx := context.New()
	var out bytes.Buffer
	ctx.Stdout = &out
	name := "Project_1." + strings.Repeat("a", 118)
	backend := networkBackend{
		list: func() ([]win32.HCNNetwork, error) { return nil, nil },
		createPrivate: func(got string) (string, error) {
			if got != name {
				t.Fatalf("create got name %q, want %q", got, name)
			}
			return id, nil
		},
	}
	if code, err := networkCommand(ctx, []string{"create", name}, backend); code != 0 || err != nil || out.String() != id+"\n" {
		t.Fatalf("create: exit %d, error %v, output %q", code, err, out.String())
	}

	out.Reset()
	backend.list = func() ([]win32.HCNNetwork, error) {
		return []win32.HCNNetwork{{ID: id, Name: name, Driver: "ics"}}, nil
	}
	backend.createPrivate = nil
	if code, err := networkCommand(ctx, []string{"create", strings.ToLower(name)}, backend); code != 1 || err == nil || !strings.Contains(err.Error(), "already exists") || out.Len() != 0 {
		t.Fatalf("duplicate create: exit %d, error %v, output %q", code, err, out.String())
	}

	backend.list = func() ([]win32.HCNNetwork, error) { return nil, nil }
	backend.createPrivate = func(string) (string, error) { return "", win32.ErrHCNNetworkExists }
	if code, err := networkCommand(ctx, []string{"create", name}, backend); code != 1 || !errors.Is(err, win32.ErrHCNNetworkExists) || !strings.Contains(err.Error(), name) || out.Len() != 0 {
		t.Fatalf("concurrent duplicate create: exit %d, error %v, output %q", code, err, out.String())
	}
}

func TestNetworkList(t *testing.T) {
	const (
		firstID  = "00000000-1234-5678-90ab-bcdef0123456"
		secondID = "10000000-1234-5678-90ab-bcdef0123456"
		thirdID  = "20000000-1234-5678-90ab-bcdef0123456"
	)
	ctx := context.New()
	var out bytes.Buffer
	ctx.Stdout = &out
	backend := networkBackend{list: func() ([]win32.HCNNetwork, error) {
		return []win32.HCNNetwork{
			{ID: thirdID, Name: "Default Switch", Driver: "ics"},
			{ID: secondID, Name: "Alpha", Driver: "private"},
			{ID: firstID, Name: "alpha", Driver: "nat"},
		}, nil
	}}
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"ls"}, strings.Join([]string{
			"NETWORK ID                             NAME             DRIVER    SCOPE",
			firstID + "   alpha            nat       local",
			secondID + "   Alpha            private   local",
			thirdID + "   Default Switch   ics       local",
			"",
		}, "\n")},
		{[]string{"list", "-q"}, firstID + "\n" + secondID + "\n" + thirdID + "\n"},
		{[]string{"ls", "--quiet"}, firstID + "\n" + secondID + "\n" + thirdID + "\n"},
	} {
		out.Reset()
		if code, err := networkCommand(ctx, test.args, backend); code != 0 || err != nil || out.String() != test.want {
			t.Errorf("%v: exit %d, error %v, got %q, want %q", test.args, code, err, out.String(), test.want)
		}
	}
	backend.list = func() ([]win32.HCNNetwork, error) { return nil, nil }
	out.Reset()
	if code, err := networkCommand(ctx, []string{"ls"}, backend); code != 0 || err != nil || out.String() != "NETWORK ID   NAME   DRIVER   SCOPE\n" {
		t.Errorf("empty list: exit %d, error %v, output %q", code, err, out.String())
	}
	out.Reset()
	if code, err := networkCommand(ctx, []string{"ls", "-q"}, backend); code != 0 || err != nil || out.Len() != 0 {
		t.Errorf("empty quiet list: exit %d, error %v, output %q", code, err, out.String())
	}
}

func TestNetworkBackendErrors(t *testing.T) {
	ctx := context.New()
	var out bytes.Buffer
	ctx.Stdout = &out
	for _, test := range []struct {
		err     error
		message string
	}{
		{fs.ErrPermission, "elevated PowerShell session"},
		{win32.ErrHCNUnavailable, "Host Network Service"},
	} {
		backend := networkBackend{list: func() ([]win32.HCNNetwork, error) { return nil, test.err }}
		for _, args := range [][]string{{"ls"}, {"create", "example"}} {
			if code, err := networkCommand(ctx, args, backend); code != 1 || !errors.Is(err, test.err) || !strings.Contains(err.Error(), test.message) || out.Len() != 0 {
				t.Errorf("%v: exit %d, error %v, output %q", args, code, err, out.String())
			}
		}
		backend.list = func() ([]win32.HCNNetwork, error) { return nil, nil }
		backend.createPrivate = func(string) (string, error) { return "", test.err }
		if code, err := networkCommand(ctx, []string{"create", "example"}, backend); code != 1 || !errors.Is(err, test.err) || !strings.Contains(err.Error(), test.message) || out.Len() != 0 {
			t.Errorf("create failure: exit %d, error %v, output %q", code, err, out.String())
		}
	}
}

func TestNetworkHCNSmoke(t *testing.T) {
	if os.Getenv("QUASIDOCK_TEST_HCN") != "1" {
		t.Skip("set QUASIDOCK_TEST_HCN=1 on an elevated Windows host with HNS to exercise real network creation")
	}
	hnsdiag, err := win32.SystemProgram("hnsdiag.exe")
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := exec.Command(hnsdiag, "list", "networks").CombinedOutput()
	if err != nil {
		t.Fatalf("read host networks: %v\n%s", err, diagnostics)
	}

	type result struct {
		out  string
		code uint32
		err  error
	}
	call := func(args ...string) result {
		ctx := context.New()
		var stdout bytes.Buffer
		ctx.Stdout = &stdout
		code, err := Dispatch(ctx, append([]string{"network"}, args...))
		return result{stdout.String(), code, err}
	}
	name := "quasidock-test-" + rand.Text()
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, variant := range []string{name, strings.ToLower(name)} {
		go func() {
			<-start
			results <- call("create", variant)
		}()
	}
	close(start)
	var created []string
	for range 2 {
		result := <-results
		if result.code == 0 && result.err == nil {
			created = append(created, strings.TrimSpace(result.out))
		} else if result.code != 1 || result.err == nil || !strings.Contains(result.err.Error(), "already exists") {
			t.Errorf("create: exit %d, error %v, output %q", result.code, result.err, result.out)
		}
	}
	t.Cleanup(func() {
		for _, id := range created {
			if out, err := exec.Command(hnsdiag, "delete", "networks", id).CombinedOutput(); err != nil {
				t.Errorf("clean up HNS network %s: %v\n%s", id, err, out)
			}
		}
	})
	if len(created) != 1 {
		t.Fatalf("concurrent creates returned %d IDs, want one", len(created))
	}
	id := created[0]
	table := call("ls")
	var listed bool
	for _, row := range strings.Split(table.out, "\n") {
		fields := strings.Fields(row)
		if len(fields) == 4 && fields[0] == id && strings.EqualFold(fields[1], name) && fields[2] == "private" && fields[3] == "local" {
			listed = true
			break
		}
	}
	if table.err != nil || table.code != 0 || !listed {
		t.Fatalf("private network %s missing from network ls: %v\n%s", id, table.err, table.out)
	}
	diagnostics, err = exec.Command(hnsdiag, "list", "networks", id).CombinedOutput()
	if err != nil || !strings.Contains(strings.ToLower(string(diagnostics)), id) {
		t.Fatalf("private network %s missing from hnsdiag: %v\n%s", id, err, diagnostics)
	}
	if out, err := exec.Command(hnsdiag, "delete", "networks", id).CombinedOutput(); err != nil {
		t.Fatalf("delete HNS network %s: %v\n%s", id, err, out)
	}
	quiet := call("ls", "-q")
	if quiet.err != nil || quiet.code != 0 || strings.Contains(quiet.out, id) {
		t.Fatalf("network ls after deletion of %s: %v\n%s", id, quiet.err, quiet.out)
	}
	created = nil
}
