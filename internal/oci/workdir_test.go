package oci

import (
	"encoding/json"
	"testing"
)

func TestWorkdirRequiresV3AndSafeRelativePath(t *testing.T) {
	cfg := AppConfig{ID: "workdir-test", Version: "1", Entrypoint: []string{"app.exe"}, Workdir: "app"}
	b, _ := json.Marshal(cfg)
	if _, err := ParseConfig("fixture", MediaTypeConfig, b); err == nil {
		t.Fatal("v2 silently accepted workdir")
	}
	if _, err := ParseConfig("fixture", MediaTypeConfigV3, b); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../outside", "/app", "C:/app", "${data}", "app\\..\\outside"} {
		cfg.Workdir = p
		if err := ValidateConfig("fixture", &cfg); err == nil {
			t.Fatalf("accepted %s", p)
		}
	}
}
