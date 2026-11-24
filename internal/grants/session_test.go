package grants

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quasidock/quasidock/internal/acl"
	"github.com/quasidock/quasidock/internal/appcontainer"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
	"github.com/quasidock/quasidock/internal/written"
)

func TestLastSessionSettlesWrittenPrograms(t *testing.T) {
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	id := "test.written.session"
	program := filepath.Join(t.TempDir(), "created.exe")
	if err := os.WriteFile(program, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := acl.LabelProgram(program, acl.IntegrityLow); err != nil {
		t.Fatal(err)
	}
	if err := written.Record(st, id, []string{program}); err != nil {
		t.Fatal(err)
	}
	first, err := Begin(st, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.End(st) }()
	second, err := Begin(st, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.End(st) }()
	if err := first.End(st); err != nil {
		t.Fatal(err)
	}
	if label, err := acl.EffectiveLabel(program); err != nil || label == nil || *label != win32.SECURITY_MANDATORY_LOW_RID {
		t.Fatalf("program changed while another run was active: %v %v", label, err)
	}
	if err := second.End(st); err != nil {
		t.Fatal(err)
	}
	if label, err := acl.EffectiveLabel(program); err != nil || label == nil || *label != win32.SECURITY_MANDATORY_MEDIUM_RID {
		t.Fatalf("last run did not restore medium integrity: %v %v", label, err)
	}
	if programs, err := st.ReadPrograms(id); err != nil || len(programs) != 1 || programs[0] != program {
		t.Fatalf("written program record was lost: %v %v", programs, err)
	}
}

func TestLastSessionTakesBackRecordedLoopbackExemption(t *testing.T) {
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("test.loopback.%d", time.Now().UnixNano())
	profile, err := appcontainer.Ensure(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = appcontainer.DeleteProfile(id) }()
	defer win32.FreeSid(profile.Sid)
	defer win32.LocalFree(profile.Capability)
	result, err := appcontainer.Exempt(profile.Sid)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == appcontainer.ExemptRefused {
		t.Skipf("Windows refused loopback exemption: %d", result.Refused)
	}
	defer func() { _, _ = appcontainer.Unexempt(profile.Sid) }()
	// A user-created exemption has no record and must survive settlement.
	if err := Settle(st, id); err != nil {
		t.Fatal(err)
	}
	if exempt := appcontainer.IsExempt(profile.Sid); exempt == nil || !*exempt {
		t.Fatal("settlement removed an unrecorded exemption")
	}
	if err := st.RecordGrant(id, store.Grant{Kind: store.GrantLoopback}); err != nil {
		t.Fatal(err)
	}
	first, err := Begin(st, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.End(st) }()
	second, err := Begin(st, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.End(st) }()
	if err := first.End(st); err != nil {
		t.Fatal(err)
	}
	if exempt := appcontainer.IsExempt(profile.Sid); exempt == nil || !*exempt {
		t.Fatal("exemption removed while another run was active")
	}
	if err := second.End(st); err != nil {
		t.Fatal(err)
	}
	if exempt := appcontainer.IsExempt(profile.Sid); exempt == nil || *exempt {
		t.Fatal("last run did not take back its exemption")
	}
	if records, err := st.ReadGrants(id); err != nil || len(records) != 0 {
		t.Fatalf("settled records = %v, %v", records, err)
	}
}
