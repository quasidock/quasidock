package oci

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestV4DataAndCommands(t *testing.T) {
	seed := "sha256:" + strings.Repeat("a", 64)
	cfg := AppConfig{FormatVersion: 4, ID: "fixture", Version: "1", DataLayer: seed, Cmd: []string{"${data}/app.exe"}, Workdir: "${data}/home", UnsetEnv: []string{"TEMP"}, Labels: map[string]string{"test": "value"}}
	b, _ := json.Marshal(cfg)
	for _, media := range []string{MediaTypeConfig, MediaTypeConfigV3, MediaTypeConfigV1} {
		if _, err := ParseConfig("fixture", media, b); err == nil {
			t.Fatalf("legacy accepted v4 fields: %s", media)
		}
	}
	parsed, err := ParseConfig("fixture", MediaTypeConfigV4, b)
	if err != nil || parsed.FormatVersion != 4 {
		t.Fatalf("v4: %+v %v", parsed, err)
	}
	m := Manifest{Layers: []Descriptor{{MediaType: MediaTypeLayerTarGzip, Digest: seed}, {MediaType: MediaTypeLayerTar, Digest: "sha256:" + strings.Repeat("b", 64)}}}
	if err := ValidateLayers("fixture", &m, parsed); err != nil {
		t.Fatal(err)
	}
	if len(AppLayers(&m, parsed)) != 1 || OwnLayer(&m).Digest != m.Layers[1].Digest || DataDescriptor(&m, parsed).Digest != seed {
		t.Fatal("incorrect layer roles")
	}
	for _, layers := range [][]Descriptor{{m.Layers[1]}, {m.Layers[1], m.Layers[0]}, {m.Layers[0], m.Layers[0], m.Layers[1]}, {{MediaType: MediaTypeLayerTar, Digest: seed}, m.Layers[1]}} {
		bad := Manifest{Layers: layers}
		if err := ValidateLayers("fixture", &bad, parsed); err == nil {
			t.Fatalf("accepted malformed layers: %+v", layers)
		}
	}
}
