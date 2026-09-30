package forward

import (
	"testing"
)

func TestForwardArgs(t *testing.T) {
	f, err := Open([]uint16{8080, 9000}, []uint16{20001, 20002})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer f.Close()

	args := f.Args()
	if len(args) != 3 {
		t.Fatalf("len(args) = %d; want 3", len(args))
	}
	if args[1] != "use=20001" {
		t.Errorf("args[1] = %q, want use=20001", args[1])
	}
	if args[2] != "use=20002" {
		t.Errorf("args[2] = %q, want use=20002", args[2])
	}
}
