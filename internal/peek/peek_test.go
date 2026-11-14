package peek

import (
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/remote"
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
			From: map[string]oci.Runtime{
				"runtime1": {
					ID:      "runtime-one",
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
	// Base image layer second
	if layers[1].ID != "runtime-one" || layers[1].Version != "2.0" {
		t.Errorf("expected base image second, got %+v", layers[1])
	}
}
