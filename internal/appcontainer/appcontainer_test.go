package appcontainer_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestAppContainer(t *testing.T) {
	testID := fmt.Sprintf("test-%d", os.Getpid())

	// Derive package SID without creating
	pkgSid, err := appcontainer.PackageSid(testID)
	if err != nil {
		t.Fatalf("PackageSid failed: %v", err)
	}
	if pkgSid == 0 {
		t.Fatalf("PackageSid returned 0")
	}

	capSid, err := appcontainer.AppCapability(testID)
	if err != nil {
		t.Fatalf("AppCapability failed: %v", err)
	}
	if capSid == 0 {
		t.Fatalf("AppCapability returned 0")
	}

	// Ensure (create)
	profile, err := appcontainer.Ensure(testID)
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	defer func() {
		_ = appcontainer.DeleteProfile(testID)
	}()

	if !win32.EqualSid(profile.Sid, pkgSid) {
		t.Fatalf("profile SID does not match derived package SID")
	}

	// Ensure second time (should open existing)
	profile2, err := appcontainer.Ensure(testID)
	if err != nil {
		t.Fatalf("Ensure (existing) failed: %v", err)
	}
	if !win32.EqualSid(profile2.Sid, pkgSid) {
		t.Fatalf("profile2 SID does not match derived package SID")
	}

	// Test Loopback Exempt and Unexempt
	res, err := appcontainer.Exempt(profile.Sid)
	if err != nil {
		t.Fatalf("Exempt error: %v", err)
	}
	if res.Status == appcontainer.ExemptRefused {
		t.Logf("Loopback exemption refused (code %d) - may need admin rights", res.Refused)
	} else {
		isExempt := appcontainer.IsExempt(profile.Sid)
		if isExempt == nil || !*isExempt {
			t.Errorf("expected isExempt == true")
		}

		unex, err := appcontainer.Unexempt(profile.Sid)
		if err != nil {
			t.Errorf("Unexempt failed: %v", err)
		}
		if !unex {
			t.Errorf("expected Unexempt to return true")
		}
	}
}
