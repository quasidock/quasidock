package updates

import (
	"strings"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/lifecycle"
)

func TestDue(t *testing.T) {
	day := int64(24 * 60 * 60)
	a := []string{"a@1"}
	ab := []string{"a@1", "b@1"}

	// Nothing waiting: nothing to tell.
	if Due(State{}, nil, 100*day) {
		t.Fatal("expected false when nothing is waiting")
	}

	// Never told.
	if !Due(State{}, a, 100*day) {
		t.Fatal("expected true when never told")
	}

	// Told today, then not again today, or tomorrow.
	if Due(State{Notified: a, NotifiedAt: 100 * day}, a, 100*day) {
		t.Fatal("expected false when told today")
	}
	if Due(State{Notified: a, NotifiedAt: 100 * day}, a, 102*day) {
		t.Fatal("expected false when within 3 days")
	}

	// A missed banner leaves nothing, so it comes back after 3 days.
	if !Due(State{Notified: a, NotifiedAt: 100 * day}, a, 103*day) {
		t.Fatal("expected true after 3 days")
	}

	// Unless they've looked.
	if Due(State{Notified: a, NotifiedAt: 100 * day, Seen: a}, a, 200*day) {
		t.Fatal("expected false after seen")
	}

	// Something new is told at once.
	if !Due(State{Notified: a, NotifiedAt: 100 * day, Seen: a}, ab, 100*day) {
		t.Fatal("expected true for new item")
	}

	// One of two updated: what's left was told about and looked at.
	if Due(State{Notified: ab, NotifiedAt: 100 * day, Seen: ab}, a, 200*day) {
		t.Fatal("expected false when remaining items were seen")
	}
}

func TestWriteNotification(t *testing.T) {
	app := lifecycle.Waiting{
		ID:         "org.a",
		From:       "1",
		To:         "2",
		FromDigest: "sha256:0",
		ToDigest:   "sha256:1",
	}

	var sb strings.Builder
	WriteNotification(&sb, State{Apps: []lifecycle.Waiting{app}})
	expected1 := "zigsaw: 1 update waiting\norg.a 1 to 2. Click to see what's waiting; nothing gets installed.\n"
	if sb.String() != expected1 {
		t.Fatalf("got %q, want %q", sb.String(), expected1)
	}

	sb.Reset()
	zVer := "9.0.0"
	WriteNotification(&sb, State{Apps: []lifecycle.Waiting{app, app}, Zigsaw: &zVer})
	expected2 := "zigsaw: 3 updates waiting\norg.a 1 to 2, and 2 more. Click to see what's waiting; nothing gets installed.\n"
	if sb.String() != expected2 {
		t.Fatalf("got %q, want %q", sb.String(), expected2)
	}

	sb.Reset()
	WriteNotification(&sb, State{Zigsaw: &zVer})
	expected3 := "zigsaw: 1 update waiting\nzigsaw 9.0.0 is out. Click to see what's waiting; nothing gets installed.\n"
	if sb.String() != expected3 {
		t.Fatalf("got %q, want %q", sb.String(), expected3)
	}
}

func TestNotificationLengthFitsWindows(t *testing.T) {
	long := strings.Repeat("x", 400)
	app := lifecycle.Waiting{
		ID:         long,
		From:       long,
		To:         long,
		FromDigest: "sha256:0",
		ToDigest:   "sha256:1",
	}

	var sb strings.Builder
	WriteNotification(&sb, State{Apps: []lifecycle.Waiting{app}})
	lines := strings.Split(sb.String(), "\n")
	if len(lines[0]) > 63 {
		t.Fatalf("title too long: %d chars", len(lines[0]))
	}
	if len(lines[1]) > 255 {
		t.Fatalf("body too long: %d chars", len(lines[1]))
	}
}

func TestClip(t *testing.T) {
	if s := Clip("abc", 3); s != "abc" {
		t.Fatalf("got %q, want abc", s)
	}
	if s := Clip("abc", 2); s != "ab" {
		t.Fatalf("got %q, want ab", s)
	}
	// UTF-8 multibyte
	if s := Clip("a\xc3\xa9", 2); s != "a" {
		t.Fatalf("got %q, want a", s)
	}
	if s := Clip("a\xc3\xa9b", 3); s != "a\xc3\xa9" {
		t.Fatalf("got %q, want a\\xc3\\xa9", s)
	}
}

func TestFormatAgo(t *testing.T) {
	cases := []struct {
		s    int64
		want string
	}{
		{-5, "just now"},
		{59, "just now"},
		{60, "1 minute ago"},
		{2 * 3600, "2 hours ago"},
		{24 * 3600, "1 day ago"},
	}

	for _, c := range cases {
		got := FormatAgo(c.s)
		if got != c.want {
			t.Fatalf("FormatAgo(%d) = %q, want %q", c.s, got, c.want)
		}
	}
}
