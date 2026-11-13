package associations

import (
	"fmt"
	"os"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/oci"
	"github.com/zigsaw-io/zigsaw/internal/regkey"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

func TestProgID(t *testing.T) {
	got := ProgID("com.github.flxzt.rnote", ".rnote")
	if got != "Zigsaw.com.github.flxzt.rnote.rnote" {
		t.Fatalf("expected Zigsaw.com.github.flxzt.rnote.rnote, got %q", got)
	}
}

func TestShownAs(t *testing.T) {
	cfg := oci.AppConfig{
		ID:      "x",
		Version: "1",
		Command: "a.exe",
	}
	name, icon := ShownAs(cfg, "rnote")
	if name != "rnote" || icon != nil {
		t.Fatalf("expected rnote / nil, got %s / %v", name, icon)
	}

	ico := "share/rnote.ico"
	cfg.Shortcuts = map[string]oci.Shortcut{
		"Rnote CLI": {Command: "rnote-cli"},
		"Rnote":     {Command: "rnote", Icon: &ico},
	}
	name, icon = ShownAs(cfg, "rnote")
	if name != "Rnote" || icon == nil || *icon != "share/rnote.ico" {
		t.Fatalf("expected Rnote / share/rnote.ico, got %s / %v", name, icon)
	}
	name, icon = ShownAs(cfg, "rnote-cli")
	if name != "Rnote CLI" || icon != nil {
		t.Fatalf("expected Rnote CLI / nil, got %s / %v", name, icon)
	}
	name, icon = ShownAs(cfg, "other")
	if name != "other" || icon != nil {
		t.Fatalf("expected other / nil, got %s / %v", name, icon)
	}
}

func TestRegistryKeysRoundTrip(t *testing.T) {
	path := fmt.Sprintf(`Software\ZigsawTest\unit-%d`, os.Getpid())
	classes, err := regkey.Create(win32.HKEY_CURRENT_USER, path)
	if err != nil {
		t.Fatalf("create test key failed: %v", err)
	}
	defer func() {
		classes.Close()
		_ = win32.RegDeleteTree(win32.HKEY_CURRENT_USER, path)
		_ = win32.RegDeleteKey(win32.HKEY_CURRENT_USER, `Software\ZigsawTest`)
	}()

	err = setIn(&classes, "Zigsaw.a.note", StoreValue, `C:\störe`)
	if err != nil {
		t.Fatalf("setIn failed: %v", err)
	}
	key, err := regkey.Open(classes.Handle, "Zigsaw.a.note", win32.KEY_READ)
	if err != nil || key == nil {
		t.Fatalf("open key failed: %v", err)
	}
	val, ok := getStr(key, StoreValue)
	if !ok || val != `C:\störe` {
		t.Fatalf("expected C:\\störe, got %q (ok=%v)", val, ok)
	}
	if _, ok := getStr(key, "missing"); ok {
		t.Fatalf("expected missing to not be found")
	}
	key.Close()

	if noneKey, err := regkey.Open(classes.Handle, "none", win32.KEY_READ); err == nil && noneKey != nil {
		noneKey.Close()
		t.Fatalf("expected none key to be nil")
	}

	o := Owned{
		ProgID:    "Zigsaw.a.note",
		App:       "a",
		Extension: ".note",
	}
	_ = setIn(&classes, `.note\OpenWithProgids`, "Zigsaw.a.note", "")
	_ = setIn(&classes, `.note\OpenWithProgids`, "Other.note", "")
	_ = unregister(&classes, o)

	subKeys, err := classes.SubKeys()
	if err != nil || len(subKeys) != 1 || subKeys[0] != ".note" {
		t.Fatalf("expected [.note], got %v (err=%v)", subKeys, err)
	}

	_ = setIn(&classes, "Zigsaw.a.note", StoreValue, "x")
	_ = setIn(&classes, `.note\OpenWithProgids`, "Zigsaw.a.note", "")
	openWith, err := regkey.Open(classes.Handle, `.note\OpenWithProgids`, win32.KEY_READ|win32.KEY_WRITE)
	if err != nil || openWith == nil {
		t.Fatalf("open .note\\OpenWithProgids failed: %v", err)
	}
	_ = openWith.DeleteValue("Other.note")
	openWith.Close()
	_ = unregister(&classes, o)

	subKeys, _ = classes.SubKeys()
	if len(subKeys) != 0 {
		t.Fatalf("expected empty subkeys, got %v", subKeys)
	}
}
