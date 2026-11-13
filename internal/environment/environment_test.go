package environment

import (
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
)

func TestBuildPutsDirectoriesAfterTheSystem(t *testing.T) {
	host := map[string]string{}
	spec := Spec{
		ID:         "x",
		Profile:    NewProfile(`D:\d`),
		Path:       []string{`D:\app`},
		PathAfter:  []string{`D:\store\bin`, `D:\store\host`},
		SystemRoot: `C:\W`,
		Vars:       nil,
	}

	env, err := Build(host, spec)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	var pathVal string
	for _, kv := range env {
		if kv.Name == "PATH" {
			pathVal = kv.Value
			break
		}
	}

	want := `D:\app;C:\W\System32;C:\W;C:\W\System32\Wbem;D:\store\bin;D:\store\host`
	if pathVal != want {
		t.Errorf("PATH = %q; want %q", pathVal, want)
	}
}

func TestBuildPassesProxiesThroughForAppsWithNetwork(t *testing.T) {
	host := map[string]string{
		"SystemRoot": `C:\Windows`,
		"http_proxy": "http://proxy:3128",
		"NO_PROXY":   "localhost",
		"SECRET":     "x",
	}

	spec := Spec{
		ID:         "app",
		Profile:    NewProfile(`D:\data\app`),
		Path:       nil,
		SystemRoot: `C:\Windows`,
		Vars:       nil,
	}

	without, err := Build(host, spec)
	if err != nil {
		t.Fatal(err)
	}

	for _, kv := range without {
		for _, name := range append(Proxies, "SECRET") {
			if strings.EqualFold(kv.Name, name) {
				t.Errorf("without proxies contained %s=%s", kv.Name, kv.Value)
			}
		}
	}

	withNetwork := spec
	withNetwork.Proxies = true
	withNetwork.Vars = []Var{{Name: "NO_PROXY", Value: "example.org"}}

	with, err := Build(host, withNetwork)
	if err != nil {
		t.Fatal(err)
	}

	found := 0
	for _, kv := range with {
		if strings.EqualFold(kv.Name, "HTTP_PROXY") {
			if kv.Value != "http://proxy:3128" {
				t.Errorf("HTTP_PROXY = %q; want %q", kv.Value, "http://proxy:3128")
			}
			found++
		}
		if strings.EqualFold(kv.Name, "NO_PROXY") {
			if kv.Value != "example.org" {
				t.Errorf("NO_PROXY = %q; want %q", kv.Value, "example.org")
			}
			found++
		}
		if strings.EqualFold(kv.Name, "SECRET") {
			t.Errorf("SECRET passed through unexpectedly")
		}
	}
	if found != 2 {
		t.Errorf("found %d proxy vars, want 2", found)
	}
}

func TestJoin(t *testing.T) {
	var env []Var
	Join(&env, "SCAN", `C:\a\conf.d`)
	Join(&env, "scan", `C:\b\conf.d;c:\A\conf.d;;C:\c`)
	Set(&env, "OTHER", "x")

	if env[0].Value != `C:\a\conf.d;C:\b\conf.d;C:\c` {
		t.Errorf("joined value = %q; want %q", env[0].Value, `C:\a\conf.d;C:\b\conf.d;C:\c`)
	}

	vars := map[string]string{
		"Scan":  `${app}\d`,
		"OTHER": `${app}`,
	}
	placeholders := oci.Placeholders{App: `D:\x`}
	err := Expand(&env, placeholders, vars, []string{"SCAN"})
	if err != nil {
		t.Fatal(err)
	}

	if env[0].Value != `C:\a\conf.d;C:\b\conf.d;C:\c;D:\x\d` {
		t.Errorf("expanded SCAN = %q", env[0].Value)
	}
	if env[1].Value != `D:\x` {
		t.Errorf("expanded OTHER = %q", env[1].Value)
	}
}

func TestEncodeBlock(t *testing.T) {
	var vars []Var
	Set(&vars, "Path", "x")
	Set(&vars, "ComSpec", "cmd")
	Set(&vars, "PATH", "y")

	block, err := EncodeBlock(vars)
	if err != nil {
		t.Fatal(err)
	}

	want := []uint16{'C', 'o', 'm', 'S', 'p', 'e', 'c', '=', 'c', 'm', 'd', 0, 'P', 'a', 't', 'h', '=', 'y', 0, 0}

	if len(block) != len(want) {
		t.Fatalf("len(block)=%d, want %d", len(block), len(want))
	}
	for i := range want {
		if block[i] != want[i] {
			t.Fatalf("mismatch at %d: %d vs %d", i, block[i], want[i])
		}
	}
}
