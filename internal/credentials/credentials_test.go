package credentials

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ghcr.io", "ghcr.io"},
		{"GHCR.io", "ghcr.io"},
		{"https://ghcr.io/v2/", "ghcr.io"},
		{"ghcr.io/quasidock", "ghcr.io"},
		{"localhost:5000/test/", "localhost:5000"},
		{"http://127.0.0.1:5001", "127.0.0.1:5001"},
		{"docker.io", "docker.io"},
		{"index.docker.io", "docker.io"},
		{"https://index.docker.io/v1/", "docker.io"},
		{"Registry-1.Docker.io", "docker.io"},
	}

	for _, c := range cases {
		got, err := NormalizeHost(c.in)
		if err != nil {
			t.Fatalf("NormalizeHost(%q) returned error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	bad := []string{"", "https://", "/path", "a b.io", "user@ghcr.io"}
	for _, b := range bad {
		if _, err := NormalizeHost(b); err == nil {
			t.Errorf("NormalizeHost(%q) expected error, got nil", b)
		}
	}
}

func TestEnvAppliesTo(t *testing.T) {
	if !EnvAppliesTo(DefaultRegistry, "ghcr.io") {
		t.Error("expected true for default registry and ghcr.io")
	}
	if !EnvAppliesTo("ghcr.io/you", "GHCR.IO") {
		t.Error("expected true for ghcr.io/you and GHCR.IO")
	}
	if !EnvAppliesTo("localhost:5000/test/", "localhost:5000") {
		t.Error("expected true for localhost:5000/test/ and localhost:5000")
	}
	if !EnvAppliesTo("docker.io/you", "registry-1.docker.io") {
		t.Error("expected true for docker.io/you and registry-1.docker.io")
	}
	if EnvAppliesTo(DefaultRegistry, "docker.io") {
		t.Error("expected false for default registry and docker.io")
	}
	if EnvAppliesTo(DefaultRegistry, "ghcr.io.evil.example") {
		t.Error("expected false for default registry and ghcr.io.evil.example")
	}
	if EnvAppliesTo("localhost:5000/test", "localhost:5001") {
		t.Error("expected false for localhost:5000/test and localhost:5001")
	}
	if EnvAppliesTo("localhost:5000", "localhost") {
		t.Error("expected false for localhost:5000 and localhost")
	}

	if h := EnvHostOtherThan(DefaultRegistry, "docker.io"); h != "ghcr.io" {
		t.Errorf("EnvHostOtherThan = %q, want ghcr.io", h)
	}
	if h := EnvHostOtherThan(DefaultRegistry, "ghcr.io"); h != "" {
		t.Errorf("EnvHostOtherThan = %q, want empty", h)
	}
}

func TestDockerConfigParsing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("DOCKER_CONFIG", tmp)

	cfg := dockerConfig{
		Auths: map[string]dockerAuth{
			"https://index.docker.io/v1/": {Auth: "dXNlcjpwYTpzcw=="}, // "user:pa:ss"
			"localhost:5001":              {Username: "bob", Password: "pw"},
		},
		CredHelpers: map[string]string{
			"ghcr.io": "nonexistent-helper-test",
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	// docker.io lookup should find auth entry
	c, err := fromDocker("docker.io")
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("expected credential for docker.io, got nil")
	}
	if c.User != "user" || c.Secret != "pa:ss" {
		t.Errorf("unexpected cred: user=%q secret=%q", c.User, c.Secret)
	}
	if !c.IsDocker() {
		t.Errorf("expected IsDocker() to be true")
	}
	if c.Origin() != "in Docker's config.json" {
		t.Errorf("unexpected origin: %s", c.Origin())
	}

	// localhost:5001
	c2, err := fromDocker("localhost:5001")
	if err != nil {
		t.Fatal(err)
	}
	if c2 == nil || c2.User != "bob" || c2.Secret != "pw" {
		t.Errorf("unexpected cred for localhost:5001: %+v", c2)
	}
}
