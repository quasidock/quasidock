package acl

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"

	"github.com/quasidock/quasidock/internal/win32"
)

type Access int

const (
	AccessReadExecute Access = iota
	AccessFull
)

func (a Access) Mask() win32.DWORD {
	if a == AccessReadExecute {
		return win32.FILE_GENERIC_READ_EXECUTE
	}
	return win32.FILE_ALL_ACCESS
}

type RevokeKind int

const (
	RevokeNone RevokeKind = iota
	RevokeSids
	RevokePackageSids
)

type Revoke struct {
	Kind RevokeKind
	Sids []win32.PSID
}

type Origin int

const (
	OriginAny Origin = iota
	OriginExplicit
)

type IntegrityLevel int

const (
	IntegrityLow IntegrityLevel = iota
	IntegrityMedium
)

type Dacl struct {
	path               string
	objectType         int32
	securityDescriptor uintptr
	acl                *win32.ACL
}

func readDacl(path string) (*Dacl, error) {
	return readDaclObject(path, win32.SE_FILE_OBJECT)
}

func readDaclObject(name string, objType int32) (*Dacl, error) {
	_, _, dacl, _, secDesc, err := win32.GetNamedSecurityInfo(name, objType, win32.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("reading the ACL of %s: %w", name, err)
	}
	return &Dacl{
		path:               name,
		objectType:         objType,
		securityDescriptor: secDesc,
		acl:                dacl,
	}, nil
}

func (d *Dacl) Close() {
	if d.securityDescriptor != 0 {
		win32.LocalFree(d.securityDescriptor)
		d.securityDescriptor = 0
	}
}

func (d *Dacl) write(acl *win32.ACL) error {
	if err := win32.SetNamedSecurityInfo(d.path, d.objectType, win32.DACL_SECURITY_INFORMATION, 0, 0, acl, nil); err != nil {
		return fmt.Errorf("updating the ACL of %s: %w", d.path, err)
	}
	return nil
}

func (d *Dacl) addEntries(entries []win32.EXPLICIT_ACCESS_W) error {
	newAcl, err := win32.SetEntriesInAcl(entries, d.acl)
	if err != nil {
		return fmt.Errorf("building the ACL of %s: %w", d.path, err)
	}
	defer win32.FreeAcl(newAcl)
	return d.write(newAcl)
}

func (d *Dacl) find(aceType uint8, sid win32.PSID, mask win32.DWORD, origin Origin) *win32.DWORD {
	if d.acl == nil {
		return nil
	}
	count := uint32(d.acl.AceCount)
	for i := uint32(0); i < count; i++ {
		ace, err := win32.GetAce(d.acl, win32.DWORD(i))
		if err != nil {
			continue
		}
		if ace.Header.AceType != aceType {
			continue
		}
		if origin == OriginExplicit && (ace.Header.AceFlags&uint8(win32.INHERITED_ACE) != 0) {
			continue
		}
		if (ace.Mask & mask) != mask {
			continue
		}
		if win32.EqualSid(ace.Sid, sid) {
			idx := win32.DWORD(i)
			return &idx
		}
	}
	return nil
}

func (d *Dacl) revocations(which Revoke, keep win32.PSID) []win32.EXPLICIT_ACCESS_W {
	var out []win32.EXPLICIT_ACCESS_W
	if d.acl == nil {
		return out
	}
	count := uint32(d.acl.AceCount)
	for i := uint32(0); i < count; i++ {
		ace, err := win32.GetAce(d.acl, win32.DWORD(i))
		if err != nil {
			continue
		}
		if ace.Header.AceType != uint8(win32.ACCESS_ALLOWED_ACE_TYPE) || (ace.Header.AceFlags&uint8(win32.INHERITED_ACE) != 0) {
			continue
		}
		sid := ace.Sid
		if keep != 0 && win32.EqualSid(sid, keep) {
			continue
		}
		named := false
		switch which.Kind {
		case RevokeNone:
			named = false
		case RevokeSids:
			for _, s := range which.Sids {
				if win32.EqualSid(sid, s) {
					named = true
					break
				}
			}
		case RevokePackageSids:
			named = IsPackageSid(sid)
		}
		if !named {
			continue
		}

		dupe := false
		for _, e := range out {
			if win32.EqualSid(e.Trustee.PtstrName, sid) {
				dupe = true
				break
			}
		}
		if !dupe {
			out = append(out, win32.EXPLICIT_ACCESS_W{
				GrfAccessPermissions: 0,
				GrfAccessMode:        win32.REVOKE_ACCESS,
				GrfInheritance:       0,
				Trustee: win32.TRUSTEE_W{
					TrusteeForm: win32.TRUSTEE_IS_SID,
					PtstrName:   sid,
				},
			})
		}
	}
	return out
}

// Grant grants sid access to path and its children, unless already granted,
// and revokes alsoRevoke SIDs. Returns true if ACL changed.
func Grant(path string, sid win32.PSID, access Access, alsoRevoke Revoke) (bool, error) {
	dacl, err := readDacl(path)
	if err != nil {
		return false, err
	}
	defer dacl.Close()

	entries := dacl.revocations(alsoRevoke, sid)
	if dacl.find(uint8(win32.ACCESS_ALLOWED_ACE_TYPE), sid, access.Mask(), OriginAny) == nil {
		entries = append(entries, win32.EXPLICIT_ACCESS_W{
			GrfAccessPermissions: access.Mask(),
			GrfAccessMode:        win32.GRANT_ACCESS,
			GrfInheritance:       win32.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: win32.TRUSTEE_W{
				TrusteeForm: win32.TRUSTEE_IS_SID,
				PtstrName:   sid,
			},
		})
	}
	if len(entries) == 0 {
		return false, nil
	}
	if err := dacl.addEntries(entries); err != nil {
		return false, err
	}
	return true, nil
}

// RevokeAccess removes allow ACEs named by which. Returns true if ACL changed.
func RevokeAccess(path string, which Revoke) (bool, error) {
	dacl, err := readDacl(path)
	if err != nil {
		return false, err
	}
	defer dacl.Close()

	entries := dacl.revocations(which, 0)
	if len(entries) == 0 {
		return false, nil
	}
	if err := dacl.addEntries(entries); err != nil {
		return false, err
	}
	return true, nil
}

// Grants returns true if an explicit allow ACE of path's own grants sid at least access.
func Grants(path string, sid win32.PSID, access Access) (bool, error) {
	dacl, err := readDacl(path)
	if err != nil {
		return false, err
	}
	defer dacl.Close()

	return dacl.find(uint8(win32.ACCESS_ALLOWED_ACE_TYPE), sid, access.Mask(), OriginExplicit) != nil, nil
}

// ExplicitGrant snapshots only this principal's explicit allow mask. Mount
// cleanup restores that mask rather than replacing unrelated ACL entries.
func ExplicitGrant(path string, sid win32.PSID) (win32.DWORD, error) {
	d, err := readDacl(path)
	if err != nil {
		return 0, err
	}
	defer d.Close()
	var mask win32.DWORD
	if d.acl == nil {
		return mask, nil
	}
	for i := uint32(0); i < uint32(d.acl.AceCount); i++ {
		a, err := win32.GetAce(d.acl, i)
		if err != nil {
			return 0, err
		}
		if a.Header.AceType == uint8(win32.ACCESS_ALLOWED_ACE_TYPE) && a.Header.AceFlags&uint8(win32.INHERITED_ACE) == 0 && win32.EqualSid(a.Sid, sid) {
			mask |= a.Mask
		}
	}
	return mask, nil
}

func RestoreGrant(path string, sid win32.PSID, mask win32.DWORD) error {
	current, err := ExplicitGrant(path, sid)
	if err != nil {
		return err
	}
	if current == mask {
		return nil
	}
	d, err := readDacl(path)
	if err != nil {
		return err
	}
	defer d.Close()
	entries := d.revocations(Revoke{Kind: RevokeSids, Sids: []win32.PSID{sid}}, 0)
	if mask != 0 {
		entries = append(entries, win32.EXPLICIT_ACCESS_W{GrfAccessPermissions: mask, GrfAccessMode: win32.GRANT_ACCESS, GrfInheritance: win32.SUB_CONTAINERS_AND_OBJECTS_INHERIT, Trustee: win32.TRUSTEE_W{TrusteeForm: win32.TRUSTEE_IS_SID, PtstrName: sid}})
	}
	if len(entries) == 0 {
		return nil
	}
	return d.addEntries(entries)
}

func RestoreLabel(path string, level *uint32) error {
	current, err := OwnLabel(path)
	if err != nil {
		return err
	}
	if current == nil && level == nil || current != nil && level != nil && *current == *level {
		return nil
	}
	if level == nil {
		return Unlabel(path)
	}
	sid, err := win32.ConvertStringSidToSid(fmt.Sprintf("S-1-16-%d", *level))
	if err != nil {
		return err
	}
	defer win32.LocalFree(sid)
	buf, sacl, err := win32.NewMandatoryAcl(sid, win32.DWORD(win32.OBJECT_INHERIT_ACE|win32.CONTAINER_INHERIT_ACE))
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(buf)
	return writeLabel(path, sacl)
}

// IsPackageSid checks if sid is an AppContainer package SID (S-1-15-2-...).
func IsPackageSid(sid win32.PSID) bool {
	auth := win32.GetSidIdentifierAuthority(sid)
	if auth == nil || !bytes.Equal(auth.Value[:], []byte{0, 0, 0, 0, 0, 15}) {
		return false
	}
	count := win32.GetSidSubAuthorityCount(sid)
	if count == nil || *count != 8 {
		return false
	}
	sub0 := win32.GetSidSubAuthority(sid, 0)
	return sub0 != nil && *sub0 == 2
}

// Protect denies the current user FILE_MODIFY right to path and its children.
func Protect(path string) error {
	user, err := CurrentUserSid()
	if err != nil {
		return err
	}
	dacl, err := readDacl(path)
	if err != nil {
		return err
	}
	defer dacl.Close()

	if dacl.find(uint8(win32.ACCESS_DENIED_ACE_TYPE), user, win32.FILE_MODIFY, OriginExplicit) != nil {
		return nil
	}
	entries := []win32.EXPLICIT_ACCESS_W{{
		GrfAccessPermissions: win32.FILE_MODIFY,
		GrfAccessMode:        win32.DENY_ACCESS,
		GrfInheritance:       win32.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: win32.TRUSTEE_W{
			TrusteeForm: win32.TRUSTEE_IS_SID,
			PtstrName:   user,
		},
	}}
	return dacl.addEntries(entries)
}

// Unprotect removes the denial of FILE_MODIFY for the current user.
func Unprotect(path string) error {
	user, err := CurrentUserSid()
	if err != nil {
		return err
	}
	dacl, err := readDacl(path)
	if err != nil {
		return err
	}
	defer dacl.Close()

	idx := dacl.find(uint8(win32.ACCESS_DENIED_ACE_TYPE), user, win32.FILE_MODIFY, OriginExplicit)
	if idx == nil {
		return nil
	}
	if err := win32.DeleteAce(dacl.acl, *idx); err != nil {
		return fmt.Errorf("DeleteAce: %w", err)
	}
	return dacl.write(dacl.acl)
}

var (
	currentUserSidOnce sync.Once
	cachedUserSid      win32.PSID
	cachedUserSidErr   error
	userSidData        []byte
)

// CurrentUserSid returns the SID of the current process user.
func CurrentUserSid() (win32.PSID, error) {
	currentUserSidOnce.Do(func() {
		token, err := win32.OpenProcessToken(win32.GetCurrentProcess(), win32.TOKEN_QUERY)
		if err != nil {
			cachedUserSidErr = fmt.Errorf("OpenProcessToken: %w", err)
			return
		}
		defer win32.CloseHandle(token)

		userSidData, cachedUserSid, cachedUserSidErr = win32.GetTokenUserSid(token)
	})
	runtime.KeepAlive(userSidData)
	return cachedUserSid, cachedUserSidErr
}

// BelowMedium returns true if current process integrity level is below medium.
func BelowMedium() (bool, error) {
	token, err := win32.OpenProcessToken(win32.GetCurrentProcess(), win32.TOKEN_QUERY)
	if err != nil {
		return false, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer win32.CloseHandle(token)

	rid, err := win32.GetTokenIntegrityRid(token)
	if err != nil {
		return false, err
	}
	return rid < win32.SECURITY_MANDATORY_MEDIUM_RID, nil
}

// OwnLabel returns the RID of the path's own integrity label, or nil if none.
func OwnLabel(path string) (*uint32, error) {
	return labelOf(path, false)
}

// EffectiveLabel returns the RID of the path's effective integrity label, or nil if none.
func EffectiveLabel(path string) (*uint32, error) {
	return labelOf(path, true)
}

func labelOf(path string, inheritedToo bool) (*uint32, error) {
	_, _, _, sacl, secDesc, err := win32.GetNamedSecurityInfo(path, win32.SE_FILE_OBJECT, win32.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("reading the integrity label of %s: %w", path, err)
	}
	if secDesc != 0 {
		defer win32.LocalFree(secDesc)
	}
	if sacl == nil {
		return nil, nil
	}
	count := uint32(sacl.AceCount)
	for i := uint32(0); i < count; i++ {
		ace, err := win32.GetAce(sacl, win32.DWORD(i))
		if err != nil {
			continue
		}
		if ace.Header.AceType != uint8(win32.SYSTEM_MANDATORY_LABEL_ACE_TYPE) {
			continue
		}
		if !inheritedToo && (ace.Header.AceFlags&uint8(win32.INHERITED_ACE) != 0) {
			continue
		}
		sid := ace.Sid
		count := win32.GetSidSubAuthorityCount(sid)
		if count == nil || *count == 0 {
			continue
		}
		lastSub := win32.GetSidSubAuthority(sid, win32.DWORD(*count-1))
		if lastSub != nil {
			val := uint32(*lastSub)
			return &val, nil
		}
	}
	return nil, nil
}

func writeLabel(path string, sacl *win32.ACL) error {
	err := win32.SetNamedSecurityInfo(path, win32.SE_FILE_OBJECT, win32.LABEL_SECURITY_INFORMATION, 0, 0, nil, sacl)
	if err == nil {
		return nil
	}
	// If access denied, user might need WRITE_OWNER temporarily
	user, errUser := CurrentUserSid()
	if errUser != nil {
		return fmt.Errorf("changing the integrity label of %s: %w", path, err)
	}
	dacl, errDacl := readDacl(path)
	if errDacl != nil {
		return fmt.Errorf("changing the integrity label of %s: %w", path, err)
	}
	defer dacl.Close()

	if dacl.find(uint8(win32.ACCESS_ALLOWED_ACE_TYPE), user, win32.WRITE_OWNER, OriginAny) != nil {
		return fmt.Errorf("changing the integrity label of %s: %w", path, err)
	}

	entries := []win32.EXPLICIT_ACCESS_W{{
		GrfAccessPermissions: win32.WRITE_OWNER,
		GrfAccessMode:        win32.GRANT_ACCESS,
		GrfInheritance:       win32.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: win32.TRUSTEE_W{
			TrusteeForm: win32.TRUSTEE_IS_SID,
			PtstrName:   user,
		},
	}}
	if err := dacl.addEntries(entries); err != nil {
		return fmt.Errorf("changing the integrity label of %s needs the right to change its owner: %w", path, err)
	}
	againErr := win32.SetNamedSecurityInfo(path, win32.SE_FILE_OBJECT, win32.LABEL_SECURITY_INFORMATION, 0, 0, nil, sacl)
	_ = dacl.write(dacl.acl)
	if againErr != nil {
		return fmt.Errorf("changing the integrity label of %s: %w", path, againErr)
	}
	return nil
}

// LabelLow labels path and its children as low integrity.
func LabelLow(path string) (bool, error) {
	own, err := OwnLabel(path)
	if err != nil {
		return false, err
	}
	if own != nil && *own <= win32.SECURITY_MANDATORY_LOW_RID {
		return false, nil
	}
	low, err := win32.ConvertStringSidToSid(win32.LowIntegritySidString)
	if err != nil {
		return false, err
	}
	defer win32.LocalFree(low)

	inherit := win32.DWORD(win32.OBJECT_INHERIT_ACE | win32.CONTAINER_INHERIT_ACE)
	buf, sacl, err := win32.NewMandatoryAcl(low, inherit)
	if err != nil {
		return false, err
	}
	defer runtime.KeepAlive(buf)
	if err := writeLabel(path, sacl); err != nil {
		return false, err
	}
	return true, nil
}

// Unlabel removes the integrity label from path.
func Unlabel(path string) error {
	bm, err := BelowMedium()
	if err != nil {
		return err
	}
	if bm {
		return fmt.Errorf("quasidock runs at low integrity here, and can't take back the label of %s", path)
	}
	empty, err := win32.NewEmptyAcl()
	if err != nil {
		return err
	}
	return writeLabel(path, &empty)
}

// LabelProgram gives a specific executable file a label of its own (medium or low).
func LabelProgram(path string, level IntegrityLevel) error {
	if level == IntegrityMedium {
		bm, err := BelowMedium()
		if err != nil {
			return err
		}
		if bm {
			return fmt.Errorf("quasidock runs at low integrity here, and can't take the label off %s", path)
		}
	}
	sidStr := win32.LowIntegritySidString
	if level == IntegrityMedium {
		sidStr = win32.MediumIntegritySidString
	}
	sid, err := win32.ConvertStringSidToSid(sidStr)
	if err != nil {
		return err
	}
	defer win32.LocalFree(sid)

	buf, sacl, err := win32.NewMandatoryAcl(sid, 0)
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(buf)
	return writeLabel(path, sacl)
}

// LetChildrenQueryUs adjusts the process SACL so low-integrity children can query parent info.
func LetChildrenQueryUs() error {
	self := win32.GetCurrentProcess()
	medium, err := win32.ConvertStringSidToSid(win32.MediumIntegritySidString)
	if err != nil {
		return fmt.Errorf("ConvertStringSidToSid: %w", err)
	}
	defer win32.LocalFree(medium)

	buf, sacl, err := win32.NewMandatoryAcl(medium, 0)
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(buf)
	if err := win32.SetSecurityInfo(self, win32.SE_KERNEL_OBJECT, win32.LABEL_SECURITY_INFORMATION, 0, 0, nil, sacl); err != nil {
		return fmt.Errorf("changing quasidock's own integrity label: %w", err)
	}
	return nil
}

// Registry key helpers
func LabelKeyLow(sub string) error {
	low, err := win32.ConvertStringSidToSid(win32.LowIntegritySidString)
	if err != nil {
		return err
	}
	defer win32.LocalFree(low)

	buf, sacl, err := win32.NewMandatoryAcl(low, win32.DWORD(win32.CONTAINER_INHERIT_ACE))
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(buf)
	name := "CURRENT_USER\\" + sub
	if err := win32.SetNamedSecurityInfo(name, win32.SE_REGISTRY_KEY, win32.LABEL_SECURITY_INFORMATION, 0, 0, nil, sacl); err != nil {
		return fmt.Errorf("changing the integrity label of %s: %w", name, err)
	}
	return nil
}

func GrantKey(sub string, sid win32.PSID) error {
	name := "CURRENT_USER\\" + sub
	dacl, err := readDaclObject(name, win32.SE_REGISTRY_KEY)
	if err != nil {
		return err
	}
	defer dacl.Close()

	if dacl.find(uint8(win32.ACCESS_ALLOWED_ACE_TYPE), sid, win32.KEY_ALL_ACCESS, OriginAny) != nil {
		return nil
	}
	entries := []win32.EXPLICIT_ACCESS_W{{
		GrfAccessPermissions: win32.KEY_ALL_ACCESS,
		GrfAccessMode:        win32.GRANT_ACCESS,
		GrfInheritance:       win32.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: win32.TRUSTEE_W{
			TrusteeForm: win32.TRUSTEE_IS_SID,
			PtstrName:   sid,
		},
	}}
	return dacl.addEntries(entries)
}

func GrantKeyRead(sub string, sid win32.PSID) error {
	name := "CURRENT_USER\\" + sub
	dacl, err := readDaclObject(name, win32.SE_REGISTRY_KEY)
	if err != nil {
		return err
	}
	defer dacl.Close()

	if dacl.find(uint8(win32.ACCESS_ALLOWED_ACE_TYPE), sid, win32.KEY_READ, OriginAny) != nil {
		return nil
	}
	entries := []win32.EXPLICIT_ACCESS_W{{
		GrfAccessPermissions: win32.KEY_READ,
		GrfAccessMode:        win32.GRANT_ACCESS,
		GrfInheritance:       win32.NO_INHERITANCE,
		Trustee: win32.TRUSTEE_W{
			TrusteeForm: win32.TRUSTEE_IS_SID,
			PtstrName:   sid,
		},
	}}
	return dacl.addEntries(entries)
}

func RevokeKey(sub string, sid win32.PSID) error {
	name := "CURRENT_USER\\" + sub
	dacl, err := readDaclObject(name, win32.SE_REGISTRY_KEY)
	if err != nil {
		return err
	}
	defer dacl.Close()

	entries := dacl.revocations(Revoke{Kind: RevokeSids, Sids: []win32.PSID{sid}}, 0)
	if len(entries) == 0 {
		return nil
	}
	return dacl.addEntries(entries)
}
