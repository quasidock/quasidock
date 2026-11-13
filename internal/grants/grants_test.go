package grants

import (
	"testing"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
)

func TestSamePathAndIsWithin(t *testing.T) {
	if !SamePath("D:\\Src\\", "d:/src") {
		t.Error("expected SamePath true")
	}
	if !SamePath("C:\\Users\\Ann\\Documents", "c:\\users\\ann\\documents\\") {
		t.Error("expected SamePath true")
	}
	if SamePath("D:\\src", "D:\\src2") {
		t.Error("expected SamePath false")
	}
	if SamePath("D:\\src\\a", "D:\\src") {
		t.Error("expected SamePath false")
	}

	data := `D:\zs\data\org.example.app`
	for _, in := range []string{data, data + `\`, `d:/zs/DATA/org.example.app/home`} {
		if !IsWithin(in, data) {
			t.Errorf("expected IsWithin true for %s", in)
		}
	}
	for _, out := range []string{`D:\zs\data\org.example.app2`, `D:\zs\data`, `D:\zs\data\org.example`} {
		if IsWithin(out, data) {
			t.Errorf("expected IsWithin false for %s", out)
		}
	}
}

func TestOwnRefusal(t *testing.T) {
	own := Own{Store: `D:\zs\Store`, SelfDir: `C:\tools\quasidock`}
	for _, p := range []string{`D:\zs\store`, `d:/zs/STORE/`, `D:\zs`, `D:\`} {
		msg, ok := own.Refusal(p)
		if !ok || msg != "quasidock's store is in it" {
			t.Errorf("expected refusal for %s, got %s (%v)", p, msg, ok)
		}
	}
	msg, ok := own.Refusal(`D:\zs\Store\deploy\0123`)
	if !ok || msg != "it's in quasidock's store" {
		t.Errorf("expected refusal for store subpath, got %s (%v)", msg, ok)
	}
	for _, p := range []string{`C:\tools\quasidock`, `C:\tools`} {
		msg, ok := own.Refusal(p)
		if !ok || msg != "quasidock is in it" {
			t.Errorf("expected refusal for %s, got %s (%v)", p, msg, ok)
		}
	}
	msg, ok = own.Refusal(`c:/tools/quasidock/x`)
	if !ok || msg != "it's in quasidock's folder" {
		t.Errorf("expected refusal for exe subpath, got %s (%v)", msg, ok)
	}
	for _, p := range []string{`D:\zs\store2`, `D:\z`, `C:\tools\quasidock2`, `E:\src`} {
		if _, ok := own.Refusal(p); ok {
			t.Errorf("expected no refusal for %s", p)
		}
	}
}

func TestStandingKeeps(t *testing.T) {
	full := acl.AccessFull
	read := acl.AccessReadExecute

	paths := []StandingPath{
		{Path: `D:\rw`, Access: acl.AccessFull},
		{Path: `D:\ro`, Access: acl.AccessReadExecute},
		{Path: `C:\Users\Ann\Documents`, Access: acl.AccessReadExecute},
		{Path: `c:\users\ann\documents\`, Access: acl.AccessFull},
	}

	savedLow := Standing{
		Sandbox: oci.SandboxLow,
		Paths:   paths,
		Cwd:     &full,
	}

	low := func(p string) store.Grant {
		return store.Grant{Kind: store.GrantLow, Path: p}
	}
	ac := func(p string) store.Grant {
		return store.Grant{Kind: store.GrantAppContainer, Path: p}
	}

	acc := savedLow.Keeps(low(`d:\RW\`), false)
	if acc == nil || *acc != acl.AccessFull {
		t.Fatalf("expected full access for low d:\\RW\\")
	}

	// Read-only path not kept for low
	if savedLow.Keeps(low(`D:\ro`), false) != nil {
		t.Fatal("expected nil for low D:\\ro")
	}

	// Granted twice, writable wins
	acc = savedLow.Keeps(low(`C:\Users\Ann\Documents`), false)
	if acc == nil || *acc != acl.AccessFull {
		t.Fatal("expected full for Documents")
	}

	// Working directory
	acc = savedLow.Keeps(low(`D:\proj`), true)
	if acc == nil || *acc != acl.AccessFull {
		t.Fatal("expected full for working directory")
	}

	savedAC := Standing{
		Sandbox: oci.SandboxAppContainer,
		Paths:   paths,
		Cwd:     &read,
	}
	acc = savedAC.Keeps(ac(`D:\ro`), false)
	if acc == nil || *acc != acl.AccessReadExecute {
		t.Fatal("expected read for AC D:\\ro")
	}
	acc = savedAC.Keeps(ac(`D:\rw`), false)
	if acc == nil || *acc != acl.AccessFull {
		t.Fatal("expected full for AC D:\\rw")
	}
}
