package shortcuts

import (
	"testing"
)

func TestChoose(t *testing.T) {
	local := `C:\Users\me\AppData\Local`
	if c := choose("", local, `c:\users\ME\AppData\Local\Zigsaw\`); c.kind != choiceStartMenu {
		t.Fatalf("expected choiceStartMenu, got %v", c.kind)
	}
	if c := choose("", local, `C:\Temp\store`); c.kind != choiceNone {
		t.Fatalf("expected choiceNone, got %v", c.kind)
	}
	if c := choose("", local, `C:\Users\me\AppData\Local\zigsaw2`); c.kind != choiceNone {
		t.Fatalf("expected choiceNone, got %v", c.kind)
	}
	if c := choose("", "", `C:\Users\me\AppData\Local\zigsaw`); c.kind != choiceNone {
		t.Fatalf("expected choiceNone, got %v", c.kind)
	}
	if c := choose("", local, `C:\Temp\store`); c.kind != choiceNone {
		t.Fatalf("expected choiceNone, got %v", c.kind)
	}
	if c := choose(`D:\links`, local, `C:\Temp\store`); c.kind != choiceOverride || c.dir != `D:\links` {
		t.Fatalf("expected override D:\\links, got %+v", c)
	}
}

func TestShimName(t *testing.T) {
	bin := `C:\z\bin`
	if got := ShimName(`c:\Z\bin\gtk4-demo.exe`, bin); got != "gtk4-demo" {
		t.Fatalf("expected gtk4-demo, got %q", got)
	}
	if got := ShimName(`C:\z\bin\sub\a.exe`, bin); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if got := ShimName(`C:\other\a.exe`, bin); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if got := ShimName(`C:\z\bin\a.cmd`, bin); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
