package process

import (
	"errors"
	"testing"
)

func TestBuildCommandLine(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{args: []string{"plain"}, want: `C:\app\x.exe plain`},
		{args: []string{"two words"}, want: `C:\app\x.exe "two words"`},
		{args: []string{""}, want: `C:\app\x.exe ""`},
		{args: []string{`say "hi"`}, want: `C:\app\x.exe "say \"hi\""`},
		{args: []string{`C:\dir with space\`}, want: `C:\app\x.exe "C:\dir with space\\"`},
		{args: []string{`a\\b`}, want: `C:\app\x.exe a\\b`},
	}

	for _, c := range cases {
		got := BuildCommandLine(`C:\app\x.exe`, c.args)
		if got != c.want {
			t.Errorf("BuildCommandLine(%v) = %q; want %q", c.args, got, c.want)
		}
	}
}

func TestBuildBatchCommandLine(t *testing.T) {
	cmd := `C:\Windows\System32\cmd.exe`
	prefix := cmd + ` /d /e:ON /v:OFF /c ""C:\app\run.cmd"`

	cases := []struct {
		arg  string
		want string
	}{
		{arg: "plain-1.2/x:y@z", want: "plain-1.2/x:y@z"},
		{arg: `a\b`, want: `a\b`},
		{arg: "", want: `""`},
		{arg: "two words", want: `"two words"`},
		{arg: `C:\dir\`, want: `"C:\dir\\"`},
		{arg: `say "hi"`, want: `"say ""hi"""`},
		{arg: `a\"b`, want: `"a\\""b"`},
		{arg: "%PATH%", want: `"%%cd:~,%PATH%%cd:~,%"`},
		{arg: "!x!", want: `"!x!"`},
		{arg: "& | < > ^ ( )", want: `"& | < > ^ ( )"`},
		{arg: `"&calc&"`, want: `"""&calc&"""`},
		{arg: "--flag=a,b;c", want: `"--flag=a,b;c"`},
		{arg: "tab\there", want: "\"tab\there\""},
		{arg: "héllo-wörld", want: "héllo-wörld"},
		{arg: "next\u0085line", want: "\"next\u0085line\""},
	}

	for _, c := range cases {
		got, err := BuildBatchCommandLine(cmd, `C:\app\run.cmd`, []string{c.arg})
		if err != nil {
			t.Fatalf("BuildBatchCommandLine failed for %q: %v", c.arg, err)
		}
		expected := prefix + " " + c.want + `"`
		if got != expected {
			t.Errorf("BuildBatchCommandLine(%q) = %q; want %q", c.arg, got, expected)
		}
	}

	got, err := BuildBatchCommandLine(cmd, `C:\app\run.cmd`, []string{"a", "b c"})
	if err != nil {
		t.Fatal(err)
	}
	expected := prefix + ` a "b c""`
	if got != expected {
		t.Errorf("got %q; want %q", got, expected)
	}

	got, err = BuildBatchCommandLine(cmd, `C:\app\run.cmd`, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected = prefix + `"`
	if got != expected {
		t.Errorf("got %q; want %q", got, expected)
	}

	got, err = BuildBatchCommandLine(cmd, `C:\100%\run.cmd`, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected = cmd + ` /d /e:ON /v:OFF /c ""C:\100%%cd:~,%\run.cmd""`
	if got != expected {
		t.Errorf("got %q; want %q", got, expected)
	}

	_, err = BuildBatchCommandLine(cmd, `C:\a"b.cmd`, nil)
	if !errors.Is(err, ErrInvalidBatchScript) {
		t.Errorf("expected ErrInvalidBatchScript, got %v", err)
	}

	_, err = BuildBatchCommandLine(cmd, `C:\dir\`, nil)
	if !errors.Is(err, ErrInvalidBatchScript) {
		t.Errorf("expected ErrInvalidBatchScript, got %v", err)
	}

	for _, bad := range []string{"a\nb", "a\rb", "a\x00b"} {
		_, err = BuildBatchCommandLine(cmd, `C:\app\run.cmd`, []string{bad})
		if !errors.Is(err, ErrInvalidBatchArgument) {
			t.Errorf("expected ErrInvalidBatchArgument for %q, got %v", bad, err)
		}
	}
}
