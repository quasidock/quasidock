package lazy

import (
	"testing"
)

func TestEventName(t *testing.T) {
	root1 := `C:\Users\u\AppData\Local\zigsaw`
	root2 := `D:\other`
	hex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	ev1 := EventName(root1, hex, "ready")
	ev2 := EventName(root2, hex, "ready")

	if ev1 == ev2 {
		t.Errorf("expected different event names for different roots")
	}
	if len(ev1) >= 260 {
		t.Errorf("event name exceeds 260 chars: %d", len(ev1))
	}
}
