package acl_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/acl"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

func TestCurrentUserSid(t *testing.T) {
	sid, err := acl.CurrentUserSid()
	if err != nil {
		t.Fatalf("CurrentUserSid failed: %v", err)
	}
	if sid == 0 {
		t.Fatalf("CurrentUserSid returned 0")
	}

	strSid, err := win32.ConvertSidToStringSid(sid)
	if err != nil {
		t.Fatalf("ConvertSidToStringSid failed: %v", err)
	}
	if len(strSid) == 0 || strSid[:2] != "S-" {
		t.Fatalf("unexpected SID string: %s", strSid)
	}
}

func TestBelowMedium(t *testing.T) {
	bm, err := acl.BelowMedium()
	if err != nil {
		t.Fatalf("BelowMedium failed: %v", err)
	}
	// Under normal test runner, we run at medium or high integrity
	if bm {
		t.Logf("Running below medium integrity")
	}
}

func TestProtectUnprotect(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "protect_test")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	// Protect
	if err := acl.Protect(target); err != nil {
		t.Fatalf("Protect failed: %v", err)
	}

	// Unprotect
	if err := acl.Unprotect(target); err != nil {
		t.Fatalf("Unprotect failed: %v", err)
	}
}

func TestPackageSid(t *testing.T) {
	// Package SID: S-1-15-2-123-456-789-101-102-103-104
	pkgSid, err := win32.ConvertStringSidToSid("S-1-15-2-1-2-3-4-5-6-7")
	if err != nil {
		t.Fatalf("ConvertStringSidToSid failed: %v", err)
	}
	defer win32.LocalFree(pkgSid)

	if !acl.IsPackageSid(pkgSid) {
		t.Fatalf("expected true for package SID")
	}

	// ALL APPLICATION PACKAGES: S-1-15-2-1 (subauthority count != 8)
	allApp, err := win32.ConvertStringSidToSid("S-1-15-2-1")
	if err != nil {
		t.Fatalf("ConvertStringSidToSid failed: %v", err)
	}
	defer win32.LocalFree(allApp)

	if acl.IsPackageSid(allApp) {
		t.Fatalf("expected false for ALL APPLICATION PACKAGES")
	}

	// Capability SID: S-1-15-3-1024-1-2-3-4-5-6-7
	capSid, err := win32.ConvertStringSidToSid("S-1-15-3-1024-1-2-3-4-5-6-7")
	if err != nil {
		t.Fatalf("ConvertStringSidToSid failed: %v", err)
	}
	defer win32.LocalFree(capSid)

	if acl.IsPackageSid(capSid) {
		t.Fatalf("expected false for capability SID")
	}
}
