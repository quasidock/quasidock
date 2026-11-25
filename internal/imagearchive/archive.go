// Package imagearchive transfers Quasidock images as self-contained OCI layouts.
// Layer bytes remain opaque: deployment is left to the normal runtime paths.
package imagearchive

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

const (
	indexMediaType = "application/vnd.oci.image.index.v1+json"
	refName        = "org.opencontainers.image.ref.name"
	// Only JSON metadata is held in memory; layers are streamed to disk.
	maxJSONSize = 16 << 20
)

type layout struct {
	Version string `json:"imageLayoutVersion"`
}

type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type indexDescriptor struct {
	oci.Descriptor
	Platform *platform `json:"platform,omitempty"`
}

type imageIndex struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Manifests     []indexDescriptor `json:"manifests"`
}

type provenance struct {
	Source       string   `json:"source,omitempty"`
	Repositories []string `json:"repositories,omitempty"`
}

type metadata struct {
	Version  int                   `json:"version"`
	Images   map[string]provenance `json:"images"`
	Defaults map[string]string     `json:"defaults"`
}

// LoadOptions controls replacement of existing local names.
type LoadOptions struct {
	Force bool
}

// LoadedImage describes one successfully registered manifest and its names.
type LoadedImage struct {
	Digest string
	Names  []string
}

func checkDescriptor(desc oci.Descriptor) error {
	if oci.DigestHex(desc.Digest) == "" || desc.Size < 0 {
		return fmt.Errorf("invalid descriptor: digest %q, size %d", desc.Digest, desc.Size)
	}
	return nil
}

func parseImage(digest string, data []byte, read func(oci.Descriptor) ([]byte, error)) (*store.LoadedImage, error) {
	var manifest oci.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", digest, err)
	}
	if manifest.SchemaVersion != 2 || (manifest.MediaType != "" && manifest.MediaType != oci.MediaTypeManifest) {
		return nil, fmt.Errorf("unsupported image manifest %s", digest)
	}
	for _, desc := range append([]oci.Descriptor{manifest.Config}, manifest.Layers...) {
		if err := checkDescriptor(desc); err != nil {
			return nil, err
		}
	}
	configBytes, err := read(manifest.Config)
	if err != nil {
		return nil, err
	}
	config, err := oci.ParseConfig(digest, manifest.Config.MediaType, configBytes)
	if err != nil {
		return nil, err
	}
	if err := oci.ValidateLayers(digest, &manifest, config); err != nil {
		return nil, err
	}
	return &store.LoadedImage{Manifest: manifest, Config: *config}, nil
}

func jsonBytes(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > maxJSONSize {
		return nil, errors.New("archive JSON metadata exceeds 16 MiB")
	}
	return b, nil
}
