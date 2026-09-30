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
		ID:           "org.videolan.VLC",
		Version:      "3.0.24",
		Name:         strPtr("VLC media player"),
		Summary:      strPtr("Plays video and audio files"),
		Commands:     []string{"vlc"},
		Associations: map[string]string{".mkv": "Matroska video"},
	}

	if r, ok := RankOf(vlc, []string{"vlc"}); !ok || r != 0 {
		t.Errorf("RankOf vlc = (%d, %v), want (0, true)", r, ok)
	}

	if r, ok := RankOf(vlc, []string{"MEDIA", "videolan"}); !ok || r != 0 {
		t.Errorf("RankOf MEDIA videolan = (%d, %v), want (0, true)", r, ok)
	}

	if r, ok := RankOf(vlc, []string{"vlc", ".mkv"}); !ok || r != 1 {
		t.Errorf("RankOf vlc .mkv = (%d, %v), want (1, true)", r, ok)
	}

	if r, ok := RankOf(vlc, []string{"matroska"}); !ok || r != 1 {
		t.Errorf("RankOf matroska = (%d, %v), want (1, true)", r, ok)
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
		ID:       "org.ziglang.zig",
		Version:  "0.16.0",
		Commands: []string{"zig"},
	}
	if d := zig.Describe(); d != "commands: zig" {
		t.Errorf("Describe = %q", d)
	}
}

func TestCatalogJSONRoundTrip(t *testing.T) {
	entry := Entry{
		ID:       "org.ziglang.zig",
		Version:  "0.16.0",
		Manifest: "sha256:" + strings.Repeat("a", 64),
		Download: 88,
		Commands: []string{"zig"},
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

	if len(back.Apps) != 1 || back.Apps[0].ID != "org.ziglang.zig" || back.Apps[0].Download != 88 {
		t.Errorf("unexpected unmarshaled back: %+v", back)
	}
}
