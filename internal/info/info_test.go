package info

import (
	"strings"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/oci"
)

func TestDescribeBuild(t *testing.T) {
	b1 := oci.Build{
		Sources: []string{"a"},
	}
	got1 := DescribeBuild(b1)
	want1 := "from 1 pinned download, without an SDK"
	if got1 != want1 {
		t.Errorf("DescribeBuild = %q, want %q", got1, want1)
	}

	b2 := oci.Build{
		Sources: []string{"a", "b"},
		Network: true,
		SDK: map[string]string{
			"zig":     "sha256:z",
			"busybox": "sha256:b",
		},
	}
	got2 := DescribeBuild(b2)
	want2 := "from 2 pinned downloads, with busybox and zig; a step used the network, so it may not rebuild the same"
	if got2 != want2 {
		t.Errorf("DescribeBuild = %q, want %q", got2, want2)
	}
}

func TestFormatBytes(t *testing.T) {
	if s := formatBytes(500); s != "500 B" {
		t.Errorf("got %s", s)
	}
	if s := formatBytes(1024); !strings.HasPrefix(s, "1.0 KiB") {
		t.Errorf("got %s", s)
	}
	if s := formatBytes(1048576); !strings.HasPrefix(s, "1.0 MiB") {
		t.Errorf("got %s", s)
	}
}
