package peek

import (
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/remote"
)

func TestGetLayers(t *testing.T) {
	app := remote.Info{
		Manifest: &oci.Manifest{
			Layers: []oci.Descriptor{
				{Digest: "sha256:layer1", Size: 100},
				{Digest: "sha256:layer2", Size: 200},
			},
		},
		Config: &oci.AppConfig{
			ID:      "my.app",
			Version: "1.0",
			Runtimes: map[string]oci.Runtime{
				"runtime1": {
					ID:      "org.runtime.one",
					Version: "2.0",
					Layer:   "sha256:layer1",
				},
			},
		},
	}

	layers := getLayers(app)
	if len(layers) != 2 {
		t.Fatalf("expected 2 layers, got %d", len(layers))
	}

	// App's own layer first
	if layers[0].ID != "my.app" || layers[0].Version != "1.0" {
		t.Errorf("expected my.app first, got %+v", layers[0])
	}
	// Runtime layer second
	if layers[1].ID != "org.runtime.one" || layers[1].Version != "2.0" {
		t.Errorf("expected runtime second, got %+v", layers[1])
	}
}
