package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/testregistry"
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
		{"inspect"}, {"inspect", "-f"}, {"inspect", "--format={{.Id}}"}, {"inspect", "--quiet", "example"},
		{"connect"}, {"connect", "example"}, {"connect", "example", ""},
		{"disconnect", "", "container"}, {"disconnect", "example", "container", "extra"},
	} {
		if code, err := networkCommand(ctx, args, networkBackend{}); code != 1 || err == nil || out.Len() != 0 {
			t.Errorf("%v: exit %d, error %v, output %q", args, code, err, out.String())
		}
	}
	// An invalid network command must reach its parser without opening the store.
	root := filepath.Join(t.TempDir(), "unopened-store")
	t.Setenv("QUASIDOCK_HOME", root)
	if code, err := Dispatch(ctx, []string{"network", "inspect", "--invalid"}); code != 1 || err == nil || !strings.Contains(err.Error(), "unknown inspect option") {
		t.Fatalf("network dispatch: exit %d, error %v", code, err)
	}
	if _, err := os.Stat(root); ctx.Store != nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("network command opened the local store: %v", err)
	}
}

func TestNetworkSavedMembership(t *testing.T) {
	ctx, out := fixtureCLI(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	fixtureImage(t, reg, "1", []byte("configuration only"))
	image := reg.Host() + "/app:1"
	for _, args := range [][]string{
		{"--name", "app", "--sandbox=appcontainer", "--share=network", image},
		{"--name", "offline", "--sandbox=appcontainer", "--unshare=network", image},
	} {
		if code, err := createCommand(ctx, args); code != 0 || err != nil {
			t.Fatalf("create %v: %d %v", args, code, err)
		}
	}
	out.Reset()
	const id = "96a332e7-9e89-4bd1-922e-26010d43d249"
	backend := networkBackend{
		list: func() ([]win32.HCNNetwork, error) {
			return []win32.HCNNetwork{{ID: id, Name: "Example", Driver: "private"}}, nil
		},
		privateSubnet: func(got string) error {
			if got != id {
				t.Errorf("validated %q, want %q", got, id)
			}
			return nil
		},
	}
	call := func(args []string, message string) {
		t.Helper()
		code, err := networkCommand(ctx, args, backend)
		if message == "" && (code != 0 || err != nil) || message != "" && (code != 1 || err == nil || !strings.Contains(err.Error(), message)) || out.Len() != 0 {
			t.Fatalf("%v: exit %d, error %v, output %q", args, code, err, out.String())
		}
	}
	call([]string{"connect", "Example", "offline"}, "requires image network permission")
	call([]string{"connect", "eXaMpLe", "app"}, "")
	call([]string{"connect", "{" + strings.ToUpper(id) + "}", "app"}, "already connected")
	if err := containerInspect(ctx, []string{"app"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"`+id+`": "Example"`) || strings.Contains(out.String(), `"NetworkSettings"`) {
		t.Fatalf("unexpected stopped inspection: %s", out.String())
	}
	out.Reset()
	backend.list = func() ([]win32.HCNNetwork, error) {
		t.Fatal("disconnect tried to read deleted host networks")
		return nil, nil
	}
	call([]string{"disconnect", "{" + strings.ToUpper(id) + "}", "app"}, "")
	call([]string{"disconnect", "example", "app"}, "not found")
	rec, err := ctx.Store.ResolveContainer("app")
	if err != nil || len(rec.HostConfig.Networks) != 0 {
		t.Fatalf("disconnected record: %+v, %v", rec, err)
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

func TestNetworkInspectOutput(t *testing.T) {
	private := win32.HCNNetwork{ID: "00000000-1234-5678-90ab-bcdef0123456", Name: "Example", Driver: "private"}
	defaultSwitch := win32.HCNNetwork{ID: "10000000-1234-5678-90ab-bcdef0123456", Name: "Default Switch", Driver: "ics"}
	shadow := win32.HCNNetwork{ID: "20000000-1234-5678-90ab-bcdef0123456", Name: defaultSwitch.ID, Driver: "nat"}
	ctx := context.New()
	var out bytes.Buffer
	ctx.Stdout = &out
	listCalls := 0
	var queried []string
	backend := networkBackend{
		list: func() ([]win32.HCNNetwork, error) {
			listCalls++
			return []win32.HCNNetwork{shadow, private, defaultSwitch}, nil
		},
		inspect: func(id string) (win32.HCNNetworkDetails, error) {
			queried = append(queried, id)
			switch id {
			case private.ID:
				return win32.HCNNetworkDetails{HCNNetwork: private, Properties: map[string]any{
					"AdditionalParams": map[string]any{"LargeID": json.Number("18446744073709551615")},
				}}, nil
			case defaultSwitch.ID:
				return win32.HCNNetworkDetails{HCNNetwork: defaultSwitch, Properties: map[string]any{"Type": "ICS"}}, nil
			default:
				t.Fatalf("queried unexpected network %q", id)
				return win32.HCNNetworkDetails{}, nil
			}
		},
	}
	first := `{"Id":"` + defaultSwitch.ID + `","Name":"Default Switch","Driver":"ics","Scope":"local","Properties":{"Type":"ICS"}}`
	second := `{"Id":"` + private.ID + `","Name":"Example","Driver":"private","Scope":"local","Properties":{"AdditionalParams":{"LargeID":18446744073709551615}}}`
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "JSON",
			args: []string{defaultSwitch.ID, "example", "{" + strings.ToUpper(defaultSwitch.ID) + "}", "default switch"},
			want: "[" + first + "," + second + "," + first + "," + first + "]",
		},
		{
			name: "template",
			args: []string{"--format={{.Id}} {{json .Properties}}", strings.ToUpper(defaultSwitch.ID), "example"},
			want: defaultSwitch.ID + ` {"Type":"ICS"}` + "\n" + private.ID + ` {"AdditionalParams":{"LargeID":18446744073709551615}}` + "\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			out.Reset()
			listCalls = 0
			queried = nil
			if code, err := networkCommand(ctx, append([]string{"inspect"}, test.args...), backend); code != 0 || err != nil {
				t.Fatalf("inspect: exit %d, error %v, output %q", code, err, out.String())
			}
			got := out.String()
			if test.name == "JSON" {
				var compact bytes.Buffer
				if !strings.HasPrefix(got, "[\n  {") || json.Compact(&compact, out.Bytes()) != nil {
					t.Fatalf("expected an indented JSON array, got %q", got)
				}
				got = compact.String()
			}
			if got != test.want {
				t.Errorf("inspect output = %q, want %q", got, test.want)
			}
			if listCalls != 1 || !slices.Equal(queried, []string{defaultSwitch.ID, private.ID}) {
				t.Errorf("listed %d times and queried %v", listCalls, queried)
			}
		})
	}
}

func TestNetworkInspectErrors(t *testing.T) {
	private := win32.HCNNetwork{ID: "00000000-1234-5678-90ab-bcdef0123456", Name: "Example"}
	other := win32.HCNNetwork{ID: "10000000-1234-5678-90ab-bcdef0123456", Name: "Other"}
	for _, test := range []struct {
		name          string
		args          []string
		inspectErr    error
		message       string
		wantInspected int
	}{
		{name: "unknown", args: []string{"Example", "missing"}, message: `network "missing" not found`},
		{name: "ID prefix", args: []string{private.ID[:12]}, message: "not found"},
		{name: "name prefix", args: []string{"exam"}, message: "not found"},
		{name: "ambiguous", args: []string{"Shared"}, message: "use a full network GUID"},
		{name: "removed", args: []string{"Example", "Other"}, inspectErr: win32.ErrHCNNetworkNotFound, message: `network "Other" not found`, wantInspected: 2},
		{name: "permission", args: []string{"Other"}, inspectErr: fs.ErrPermission, message: "elevated PowerShell session", wantInspected: 1},
		{name: "unavailable", args: []string{"Other"}, inspectErr: win32.ErrHCNUnavailable, message: "Host Network Service", wantInspected: 1},
		{name: "invalid template", args: []string{"--format", "{{", "Example"}, message: "template:", wantInspected: 1},
		{name: "template execution", args: []string{"Example", "Other", "-f", `{{if eq .Name "Other"}}{{.Missing}}{{else}}{{.Id}}{{end}}`}, message: "can't evaluate field Missing", wantInspected: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.New()
			var out bytes.Buffer
			ctx.Stdout = &out
			inspected := 0
			backend := networkBackend{
				list: func() ([]win32.HCNNetwork, error) {
					return []win32.HCNNetwork{
						private,
						other,
						{ID: "20000000-1234-5678-90ab-bcdef0123456", Name: "Shared"},
						{ID: "30000000-1234-5678-90ab-bcdef0123456", Name: "shared"},
					}, nil
				},
				inspect: func(id string) (win32.HCNNetworkDetails, error) {
					inspected++
					switch id {
					case private.ID:
						return win32.HCNNetworkDetails{HCNNetwork: private}, nil
					case other.ID:
						return win32.HCNNetworkDetails{HCNNetwork: other}, test.inspectErr
					default:
						t.Fatalf("queried unexpected network %q", id)
						return win32.HCNNetworkDetails{}, nil
					}
				},
			}
			code, err := networkCommand(ctx, append([]string{"inspect"}, test.args...), backend)
			if code != 1 || err == nil || !strings.Contains(err.Error(), test.message) || out.Len() != 0 {
				t.Fatalf("inspect: exit %d, error %v, output %q", code, err, out.String())
			}
			if inspected != test.wantInspected || (test.inspectErr != nil && !errors.Is(err, test.inspectErr)) {
				t.Errorf("inspected %d networks, error %v", inspected, err)
			}
		})
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
		for _, args := range [][]string{{"ls"}, {"create", "example"}, {"inspect", "example"}} {
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
	inspection := call("inspect", strings.ToLower(name), id)
	var networks []inspectedNetwork
	if inspection.err != nil || inspection.code != 0 || json.Unmarshal([]byte(inspection.out), &networks) != nil || len(networks) != 2 {
		t.Fatalf("inspect private network by name and ID: %v\n%s", inspection.err, inspection.out)
	}
	for _, network := range networks {
		if network.Id != id || !strings.EqualFold(network.Name, name) || network.Driver != "private" || network.Scope != "local" || network.Properties["Type"] != "Private" {
			t.Errorf("unexpected network inspect result: %+v", network)
		}
	}
	diagnostics, err = exec.Command(hnsdiag, "list", "networks", id).CombinedOutput()
	if err != nil || !strings.Contains(strings.ToLower(string(diagnostics)), id) {
		t.Fatalf("private network %s missing from hnsdiag: %v\n%s", id, err, diagnostics)
	}
	if out, err := exec.Command(hnsdiag, "delete", "networks", id).CombinedOutput(); err != nil {
		t.Fatalf("delete HNS network %s: %v\n%s", id, err, out)
	}
	created = nil
	quiet := call("ls", "-q")
	if quiet.err != nil || quiet.code != 0 || strings.Contains(quiet.out, id) {
		t.Fatalf("network ls after deletion of %s: %v\n%s", id, quiet.err, quiet.out)
	}
	for _, target := range []string{name, id} {
		missing := call("inspect", target)
		if missing.code != 1 || missing.err == nil || !strings.Contains(missing.err.Error(), "not found") || missing.out != "" {
			t.Errorf("inspect %q after deletion: exit %d, error %v, output %q", target, missing.code, missing.err, missing.out)
		}
	}
}
