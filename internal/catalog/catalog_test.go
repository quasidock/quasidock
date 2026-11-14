package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func strPtr(s string) *string {
	return &s
}

func TestRankOf(t *testing.T) {
	vlc := Entry{
		ID:         "vlc",
		Version:    "3.0.24",
		Name:       strPtr("VLC media player"),
		Summary:    strPtr("Plays video and audio files"),
		Entrypoint: []string{"vlc.exe"},
		Cmd:        []string{"--no-video-title-show"},
	}

	if r, ok := RankOf(vlc, []string{"vlc"}); !ok || r != 0 {
		t.Errorf("RankOf vlc = (%d, %v), want (0, true)", r, ok)
	}

	if r, ok := RankOf(vlc, []string{"MEDIA", "player"}); !ok || r != 0 {
		t.Errorf("RankOf MEDIA player = (%d, %v), want (0, true)", r, ok)
	}

	if r, ok := RankOf(vlc, []string{"vlc", "audio"}); !ok || r != 1 {
		t.Errorf("RankOf vlc audio = (%d, %v), want (1, true)", r, ok)
	}

	if _, ok := RankOf(vlc, []string{"vlc", "pdf"}); ok {
		t.Errorf("expected no match for vlc pdf")
	}

	if r, ok := RankOf(vlc, []string{}); !ok || r != 0 {
		t.Errorf("RankOf empty = (%d, %v), want (0, true)", r, ok)
	}

	if d := vlc.Describe(); d != "VLC media player: Plays video and audio files" {
		t.Errorf("Describe = %q", d)
	}

	zig := Entry{
		ID:         "zig",
		Version:    "0.16.0",
		Entrypoint: []string{"zig.exe"},
	}
	if d := zig.Describe(); d != "entrypoint: zig.exe" {
		t.Errorf("Describe = %q", d)
	}
}

func TestCatalogJSONRoundTrip(t *testing.T) {
	entry := Entry{
		ID:         "zig",
		Version:    "0.16.0",
		Manifest:   "sha256:" + strings.Repeat("a", 64),
		Download:   88,
		Entrypoint: []string{"zig.exe"},
	}
	cat := Catalog{
		Registry: "ghcr.io/zigsaw-io",
		Apps:     []Entry{entry},
	}

	data, err := json.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}

	jsonStr := string(data)
	if strings.Contains(jsonStr, `"name"`) {
		t.Errorf("expected name to be omitted, got %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"registry":"ghcr.io/zigsaw-io"`) {
		t.Errorf("expected registry to be present, got %s", jsonStr)
	}

	var back Catalog
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}

	if len(back.Apps) != 1 || back.Apps[0].ID != "zig" || back.Apps[0].Download != 88 {
		t.Errorf("unexpected unmarshaled back: %+v", back)
	}
}
