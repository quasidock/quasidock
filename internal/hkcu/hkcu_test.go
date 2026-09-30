package hkcu

import (
	"testing"
)

func TestEscapedKeyNames(t *testing.T) {
	orig := `Software\VideoLAN\VLC`
	escaped := Escape(orig)
	if escaped != "Software/VideoLAN/VLC" {
		t.Errorf("Escape(%q) = %q; want %q", orig, escaped, "Software/VideoLAN/VLC")
	}

	unescaped := Unescape(escaped)
	if unescaped != orig {
		t.Errorf("Unescape(%q) = %q; want %q", escaped, unescaped, orig)
	}

	if !IsInStore(`C:\Store\data\app`, `c:\store\`) {
		t.Error("expected IsInStore to be true")
	}
	if IsInStore(`C:\Store2\data\app`, `C:\Store`) {
		t.Error("expected IsInStore to be false")
	}
	if !samePath(`C:\a\data\x\`, `c:\A\data\X`) {
		t.Error("expected samePath to be true")
	}
}

func TestKeysAboveAKey(t *testing.T) {
	if !IsAbove(`Software\JavaSoft\Prefs\org`, `Software\JavaSoft\Prefs\org\apache\pdfbox`) {
		t.Error("expected true")
	}
	if !IsAbove(`software\javasoft`, `Software\JavaSoft\Prefs`) {
		t.Error("expected true")
	}
	if IsAbove(`Software`, `Software\JavaSoft`) {
		t.Error("expected false")
	}
	if IsAbove(`Software\Java`, `Software\JavaSoft\Prefs`) {
		t.Error("expected false")
	}
	if IsAbove(`Software\JavaSoft\Prefs`, `Software\JavaSoft\Prefs`) {
		t.Error("expected false")
	}
	if IsAbove(`Software\Other`, `Software\JavaSoft\Prefs`) {
		t.Error("expected false")
	}
}
