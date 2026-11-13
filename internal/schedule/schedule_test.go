package schedule

import (
	"strings"
	"testing"
)

func TestNameFor(t *testing.T) {
	a := NameFor(defaultBase, "D:\\Stores\\One")
	a2 := NameFor(defaultBase, "d:\\stores\\one\\")
	if a != a2 {
		t.Fatalf("expected case/trailing slash invariance: %q vs %q", a, a2)
	}

	b := NameFor(defaultBase, "D:\\Stores\\Two")
	if a == b {
		t.Fatalf("expected different names for different stores: %q vs %q", a, b)
	}

	if !strings.HasPrefix(a, "Zigsaw update check ") {
		t.Fatalf("expected prefix 'Zigsaw update check ', got %q", a)
	}
}

func TestParseStore(t *testing.T) {
	start := "<Exec><Command>x</Command><Arguments>--store "
	end := " --task \"n\"</Arguments></Exec>"

	p1 := ParseStore(start + "D:\\zs\\plain" + end)
	if p1 == nil || *p1 != "D:\\zs\\plain" {
		t.Fatalf("got %v, want D:\\zs\\plain", p1)
	}

	p2 := ParseStore(start + "\"D:\\zs\\a store\"" + end)
	if p2 == nil || *p2 != "D:\\zs\\a store" {
		t.Fatalf("got %v, want D:\\zs\\a store", p2)
	}

	p3 := ParseStore(start + "&quot;D:\\zs\\a&amp;b c&quot;" + end)
	if p3 == nil || *p3 != "D:\\zs\\a&b c" {
		t.Fatalf("got %v, want D:\\zs\\a&b c", p3)
	}

	// Non-ascii
	p4 := ParseStore(start + "D:\\caf\xe9" + end)
	if p4 != nil {
		t.Fatalf("expected nil for non-ascii, got %v", p4)
	}

	// Non-matching
	p5 := ParseStore("<Arguments>other</Arguments>")
	if p5 != nil {
		t.Fatalf("expected nil for non-matching, got %v", p5)
	}
}
