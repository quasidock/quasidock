package win32

import (
	"syscall"
	"unsafe"
)

func OpenProcessToken(process HANDLE, access DWORD) (HANDLE, error) {
	var token HANDLE
	r, _, err := procOpenProcessToken.Call(process, uintptr(access), uintptr(unsafe.Pointer(&token)))
	if r == 0 {
		return 0, err
	}
	return token, nil
}

func DuplicateTokenEx(existing HANDLE, access DWORD, sec *SECURITY_ATTRIBUTES, impLevel, tokenType int32) (HANDLE, error) {
	var newToken HANDLE
	r, _, err := procDuplicateTokenEx.Call(
		existing,
		uintptr(access),
		uintptr(unsafe.Pointer(sec)),
		uintptr(impLevel),
		uintptr(tokenType),
		uintptr(unsafe.Pointer(&newToken)),
	)
	if r == 0 {
		return 0, err
	}
	return newToken, nil
}

func SetTokenInformation(token HANDLE, class int32, info unsafe.Pointer, length DWORD) error {
	r, _, err := procSetTokenInformation.Call(token, uintptr(class), uintptr(info), uintptr(length))
	if r == 0 {
		return err
	}
	return nil
}

func GetTokenInformation(token HANDLE, class int32, info unsafe.Pointer, length DWORD, returnLength *DWORD) error {
	r, _, err := procGetTokenInformation.Call(token, uintptr(class), uintptr(info), uintptr(length), uintptr(unsafe.Pointer(returnLength)))
	if r == 0 {
		return err
	}
	return nil
}

func InitializeAcl(acl *ACL, length, revision DWORD) error {
	r, _, err := procInitializeAcl.Call(uintptr(unsafe.Pointer(acl)), uintptr(length), uintptr(revision))
	if r == 0 {
		return err
	}
	return nil
}

func AddMandatoryAce(acl *ACL, revision, flags, policy DWORD, label PSID) error {
	r, _, err := procAddMandatoryAce.Call(uintptr(unsafe.Pointer(acl)), uintptr(revision), uintptr(flags), uintptr(policy), label)
	if r == 0 {
		return err
	}
	return nil
}

func DeleteAce(acl *ACL, index DWORD) error {
	r, _, err := procDeleteAce.Call(uintptr(unsafe.Pointer(acl)), uintptr(index))
	if r == 0 {
		return err
	}
	return nil
}

func GetAce(acl *ACL, index DWORD) (uintptr, error) {
	var ace uintptr
	r, _, err := procGetAce.Call(uintptr(unsafe.Pointer(acl)), uintptr(index), uintptr(unsafe.Pointer(&ace)))
	if r == 0 {
		return 0, err
	}
	return ace, nil
}

func FreeSid(sid PSID) {
	procFreeSid.Call(sid)
}

func EqualSid(a, b PSID) bool {
	r, _, _ := procEqualSid.Call(a, b)
	return r != 0
}

func GetLengthSid(sid PSID) DWORD {
	r, _, _ := procGetLengthSid.Call(sid)
	return DWORD(r)
}

func GetSidIdentifierAuthority(sid PSID) *SID_IDENTIFIER_AUTHORITY {
	r, _, _ := procGetSidIdentifierAuthority.Call(sid)
	return (*SID_IDENTIFIER_AUTHORITY)(unsafe.Pointer(r))
}

func GetSidSubAuthorityCount(sid PSID) *uint8 {
	r, _, _ := procGetSidSubAuthorityCount.Call(sid)
	return (*uint8)(unsafe.Pointer(r))
}

func GetSidSubAuthority(sid PSID, index DWORD) *DWORD {
	r, _, _ := procGetSidSubAuthority.Call(sid, uintptr(index))
	return (*DWORD)(unsafe.Pointer(r))
}

func ConvertStringSidToSid(s string) (PSID, error) {
	sPtr := UTF16Ptr(s)
	var sid PSID
	r, _, err := procConvertStringSidToSidW.Call(uintptr(unsafe.Pointer(sPtr)), uintptr(unsafe.Pointer(&sid)))
	if r == 0 {
		return 0, err
	}
	return sid, nil
}

func ConvertSidToStringSid(sid PSID) (string, error) {
	var strPtr *uint16
	r, _, err := procConvertSidToStringSidW.Call(sid, uintptr(unsafe.Pointer(&strPtr)))
	if r == 0 {
		return "", err
	}
	defer LocalFree(uintptr(unsafe.Pointer(strPtr)))
	return UTF16ToUTF8(utf16Slice(strPtr)), nil
}

func utf16Slice(p *uint16) []uint16 {
	if p == nil {
		return nil
	}
	var res []uint16
	for cur := p; *cur != 0; cur = (*uint16)(unsafe.Pointer(uintptr(unsafe.Pointer(cur)) + 2)) {
		res = append(res, *cur)
	}
	return res
}

func LocalFree(p uintptr) uintptr {
	r, _, _ := procLocalFree.Call(p)
	return r
}

func GetNamedSecurityInfo(name string, objType int32, secInfo DWORD) (owner, group PSID, dacl, sacl *ACL, secDesc uintptr, err error) {
	namePtr := UTF16Ptr(name)
	r, _, _ := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(objType),
		uintptr(secInfo),
		uintptr(unsafe.Pointer(&owner)),
		uintptr(unsafe.Pointer(&group)),
		uintptr(unsafe.Pointer(&dacl)),
		uintptr(unsafe.Pointer(&sacl)),
		uintptr(unsafe.Pointer(&secDesc)),
	)
	if r != 0 {
		return 0, 0, nil, nil, 0, syscall.Errno(r)
	}
	return owner, group, dacl, sacl, secDesc, nil
}

func SetNamedSecurityInfo(name string, objType int32, secInfo DWORD, owner, group PSID, dacl, sacl *ACL) error {
	namePtr := UTF16Ptr(name)
	r, _, _ := procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(objType),
		uintptr(secInfo),
		owner,
		group,
		uintptr(unsafe.Pointer(dacl)),
		uintptr(unsafe.Pointer(sacl)),
	)
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func SetSecurityInfo(handle HANDLE, objType int32, secInfo DWORD, owner, group PSID, dacl, sacl *ACL) error {
	r, _, _ := procSetSecurityInfo.Call(
		handle,
		uintptr(objType),
		uintptr(secInfo),
		owner,
		group,
		uintptr(unsafe.Pointer(dacl)),
		uintptr(unsafe.Pointer(sacl)),
	)
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func SetEntriesInAcl(entries []EXPLICIT_ACCESS_W, oldAcl *ACL) (*ACL, error) {
	var pEntries uintptr
	if len(entries) > 0 {
		pEntries = uintptr(unsafe.Pointer(&entries[0]))
	}
	var newAcl *ACL
	r, _, _ := procSetEntriesInAclW.Call(
		uintptr(len(entries)),
		pEntries,
		uintptr(unsafe.Pointer(oldAcl)),
		uintptr(unsafe.Pointer(&newAcl)),
	)
	if r != 0 {
		return nil, syscall.Errno(r)
	}
	return newAcl, nil
}

// AppContainer APIs
func CreateAppContainerProfile(name, displayName, description string, caps []SID_AND_ATTRIBUTES) (PSID, HRESULT) {
	namePtr := UTF16Ptr(name)
	dispPtr := UTF16Ptr(displayName)
	descPtr := UTF16Ptr(description)
	var pCaps *SID_AND_ATTRIBUTES
	if len(caps) > 0 {
		pCaps = &caps[0]
	}
	var sid PSID
	hr, _, _ := procCreateAppContainerProfile.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(dispPtr)),
		uintptr(unsafe.Pointer(descPtr)),
		uintptr(unsafe.Pointer(pCaps)),
		uintptr(len(caps)),
		uintptr(unsafe.Pointer(&sid)),
	)
	return sid, HRESULT(hr)
}

func DeriveAppContainerSidFromAppContainerName(name string) (PSID, HRESULT) {
	namePtr := UTF16Ptr(name)
	var sid PSID
	hr, _, _ := procDeriveAppContainerSidFromAppContainerName.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(&sid)),
	)
	return sid, HRESULT(hr)
}

func DeleteAppContainerProfile(name string) HRESULT {
	namePtr := UTF16Ptr(name)
	hr, _, _ := procDeleteAppContainerProfile.Call(uintptr(unsafe.Pointer(namePtr)))
	return HRESULT(hr)
}

func CreateEnvironmentBlock(token HANDLE, inherit bool) (uintptr, error) {
	var block uintptr
	var inh uintptr
	if inherit {
		inh = 1
	}
	r, _, err := procCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&block)), token, inh)
	if r == 0 {
		return 0, err
	}
	return block, nil
}

func DestroyEnvironmentBlock(block uintptr) bool {
	r, _, _ := procDestroyEnvironmentBlock.Call(block)
	return r != 0
}

func NetworkIsolationGetAppContainerConfig() ([]SID_AND_ATTRIBUTES, error) {
	var count DWORD
	var pSids *SID_AND_ATTRIBUTES
	r, _, _ := procNetworkIsolationGetAppContainerConfig.Call(
		uintptr(unsafe.Pointer(&count)),
		uintptr(unsafe.Pointer(&pSids)),
	)
	if r != 0 {
		return nil, syscall.Errno(r)
	}
	if count == 0 || pSids == nil {
		return nil, nil
	}
	slice := unsafe.Slice(pSids, count)
	res := make([]SID_AND_ATTRIBUTES, count)
	copy(res, slice)
	return res, nil
}

func NetworkIsolationSetAppContainerConfig(sids []SID_AND_ATTRIBUTES) error {
	var pSids *SID_AND_ATTRIBUTES
	if len(sids) > 0 {
		pSids = &sids[0]
	}
	r, _, _ := procNetworkIsolationSetAppContainerConfig.Call(
		uintptr(len(sids)),
		uintptr(unsafe.Pointer(pSids)),
	)
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func DeriveCapabilitySidsFromName(name string) (caps []PSID, groups []PSID, err error) {
	namePtr := UTF16Ptr(name)
	var groupSids, capSids *PSID
	var groupCount, capCount DWORD
	r, _, errCall := procDeriveCapabilitySidsFromName.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(&groupSids)),
		uintptr(unsafe.Pointer(&groupCount)),
		uintptr(unsafe.Pointer(&capSids)),
		uintptr(unsafe.Pointer(&capCount)),
	)
	if r == 0 {
		return nil, nil, errCall
	}
	if capCount > 0 && capSids != nil {
		slice := unsafe.Slice(capSids, capCount)
		caps = make([]PSID, capCount)
		copy(caps, slice)
	}
	if groupCount > 0 && groupSids != nil {
		slice := unsafe.Slice(groupSids, groupCount)
		groups = make([]PSID, groupCount)
		copy(groups, slice)
	}
	return caps, groups, nil
}
