package written

import (
	"testing"
)

func TestIsProgram(t *testing.T) {
	for _, n := range []string{"a.exe", "B.EXE", "x.Com", "s.scr", `dir\tool.exe`} {
		if !IsProgram(n) {
			t.Errorf("expected IsProgram(%q) to be true", n)
		}
	}

	for _, n := range []string{"a.dll", "a.exe.txt", "exe", "a.bat", "a.ps1"} {
		if IsProgram(n) {
			t.Errorf("expected IsProgram(%q) to be false", n)
		}
	}
}
