package shim

import (
	"fmt"
	"strings"
	"testing"
)

func TestArgumentsAsTyped(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"npm install", " install"},
		{`"C:\Program Files\x\npm.exe" install "a b"`, ` install "a b"`},
		{"npm", ""},
		{`"C:\x y\npm.exe"`, ""},
		{"C:\\bin\\npm.exe\t-v  \"\\\"q\\\"\"", "\t-v  \"\\\"q\\\"\""},
	}
	for _, c := range cases {
		got := ArgumentsAsTyped(c.line)
		if got != c.want {
			t.Errorf("ArgumentsAsTyped(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestAppendFiles(t *testing.T) {
	cases := []struct {
		args string
		want string
	}{
		{` "C:\My Notes\a b.rnote"`, `run "--file=C:\My Notes\a b.rnote"`},
		{` C:\n\a.rnote "D:\x y\b.rnote"`, `run --file=C:\n\a.rnote "--file=D:\x y\b.rnote"`},
		{"", "run"},
		{` ""`, "run"},
	}
	for _, c := range cases {
		var b strings.Builder
		b.WriteString("run")
		AppendFiles(&b, c.args)
		got := b.String()
		if got != c.want {
			t.Errorf("AppendFiles(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestOneUrl(t *testing.T) {
	url, err := OneUrl(` "x-app://a b/c"`, "x-app")
	if err != nil || url != "x-app://a b/c" {
		t.Errorf("expected x-app://a b/c, got %q, err %v", url, err)
	}

	url, err = OneUrl(" x-app:a", "other,x-app")
	if err != nil || url != "x-app:a" {
		t.Errorf("expected x-app:a, got %q, err %v", url, err)
	}

	_, err = OneUrl(` "x-app://a" --gpu-launcher=calc ""`, "x-app")
	if err != ErrSeveralArguments {
		t.Errorf("expected ErrSeveralArguments, got %v", err)
	}

	_, err = OneUrl(" --gpu-launcher=calc", "x-app")
	if err != ErrOtherScheme {
		t.Errorf("expected ErrOtherScheme, got %v", err)
	}

	_, err = OneUrl(" other://a", "x-app")
	if err != ErrOtherScheme {
		t.Errorf("expected ErrOtherScheme, got %v", err)
	}

	_, err = OneUrl("", "x-app")
	if err != ErrOtherScheme {
		t.Errorf("expected ErrOtherScheme, got %v", err)
	}
}

func TestAppendKept(t *testing.T) {
	cases := []struct {
		args string
		want string
	}{
		{" --64 -o x.o x.s", "as -o x.o x.s"},
		{"", "as"},
		{" --64", "as"},
		{"\t\"--64\"  \"a b\" --640 c\\\"d \"e\\\\\"", "as \"a b\" --640 \"c\\\"d\" e\\"},
	}
	for _, c := range cases {
		var b strings.Builder
		b.WriteString("as")
		AppendKept(&b, c.args, []string{"--64"})
		got := b.String()
		if got != c.want {
			t.Errorf("AppendKept(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestTail(t *testing.T) {
	tail := NewTail(8192)
	tail.Append([]byte("one\ntwo\n"))
	if tail.Text() != "one\ntwo\n" {
		t.Fatalf("expected one\\ntwo\\n, got %q", tail.Text())
	}

	for i := 0; i < 1000; i++ {
		tail.Append([]byte(fmt.Sprintf("line %d\n", i)))
	}
	kept := tail.Text()
	if !strings.HasPrefix(kept, "line ") {
		t.Fatalf("expected prefix 'line ', got %q", kept[:20])
	}
	if !strings.HasSuffix(kept, "line 999\n") {
		t.Fatalf("expected suffix 'line 999\\n'")
	}
	if LastLines(kept, 2) != "line 998\nline 999" {
		t.Fatalf("expected 'line 998\\nline 999', got %q", LastLines(kept, 2))
	}
}

func TestLastLines(t *testing.T) {
	if got := LastLines("a\nb\nc\n\n", 2); got != "b\nc" {
		t.Errorf("expected 'b\\nc', got %q", got)
	}
	if got := LastLines("a\nb", 5); got != "a\nb" {
		t.Errorf("expected 'a\\nb', got %q", got)
	}
	if got := LastLines("\r\n", 3); got != "" {
		t.Errorf("expected '', got %q", got)
	}
}
