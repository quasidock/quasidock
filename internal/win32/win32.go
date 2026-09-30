package win32

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// Win32 primitive types
type (
	BOOL    = int32
	DWORD   = uint32
	WORD    = uint16
	HRESULT = int32
	HANDLE  = uintptr
	PSID    = uintptr
	HKEY    = uintptr
	SOCKET  = uintptr
)

const (
	TRUE  BOOL = 1
	FALSE BOOL = 0

	INVALID_HANDLE_VALUE HANDLE = ^uintptr(0)
	INVALID_SOCKET       SOCKET = ^uintptr(0)

	INFINITE      DWORD = 0xFFFFFFFF
	WAIT_OBJECT_0 DWORD = 0
	WAIT_TIMEOUT  DWORD = 0x102
	WAIT_ABANDONED DWORD = 0x80

	EVENT_MODIFY_STATE DWORD = 0x0002

	STD_INPUT_HANDLE  DWORD = 0xFFFFFFF6 // (DWORD)-10
	STD_OUTPUT_HANDLE DWORD = 0xFFFFFFF5 // (DWORD)-11
	STD_ERROR_HANDLE  DWORD = 0xFFFFFFF4 // (DWORD)-12

	HANDLE_FLAG_INHERIT DWORD = 0x1

	GENERIC_READ    DWORD = 0x80000000
	GENERIC_WRITE   DWORD = 0x40000000
	GENERIC_EXECUTE DWORD = 0x20000000
	GENERIC_ALL     DWORD = 0x10000000

	FILE_SHARE_READ   DWORD = 0x1
	FILE_SHARE_WRITE  DWORD = 0x2
	FILE_SHARE_DELETE DWORD = 0x4

	CREATE_NEW        DWORD = 1
	CREATE_ALWAYS     DWORD = 2
	OPEN_EXISTING     DWORD = 3
	OPEN_ALWAYS       DWORD = 4
	TRUNCATE_EXISTING DWORD = 5

	FILE_ATTRIBUTE_READONLY      DWORD = 0x00000001
	FILE_ATTRIBUTE_DIRECTORY     DWORD = 0x00000010
	FILE_ATTRIBUTE_NORMAL        DWORD = 0x00000080
	FILE_ATTRIBUTE_REPARSE_POINT DWORD = 0x00000400
	INVALID_FILE_ATTRIBUTES      DWORD = 0xFFFFFFFF

	FILE_FLAG_BACKUP_SEMANTICS   DWORD = 0x02000000
	FILE_FLAG_OPEN_REPARSE_POINT DWORD = 0x00200000
	FILE_FLAG_OVERLAPPED         DWORD = 0x40000000
	DELETE                       DWORD = 0x00010000
	FILE_FLAG_DELETE_ON_CLOSE    DWORD = 0x04000000

	FILE_READ_ATTRIBUTES DWORD = 0x80
	FILE_LIST_DIRECTORY  DWORD = 0x1

	STARTF_USESTDHANDLES    DWORD = 0x00000100
	STARTF_TITLEISLINKNAME  DWORD = 0x00000800

	DETACHED_PROCESS            DWORD = 0x00000008
	CREATE_NEW_PROCESS_GROUP    DWORD = 0x00000200
	CREATE_NEW_CONSOLE          DWORD = 0x00000010
	CREATE_NO_WINDOW            DWORD = 0x08000000
	CREATE_UNICODE_ENVIRONMENT  DWORD = 0x00000400
	EXTENDED_STARTUPINFO_PRESENT DWORD = 0x00080000
	CREATE_BREAKAWAY_FROM_JOB   DWORD = 0x01000000

	PROC_THREAD_ATTRIBUTE_HANDLE_LIST          uintptr = 0x00020002
	PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES uintptr = 0x00020009
	PROC_THREAD_ATTRIBUTE_JOB_LIST             uintptr = 0x0002000D

	JobObjectBasicAccountingInformation int32 = 1
	JobObjectExtendedLimitInformation    int32 = 9

	JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE DWORD = 0x00002000
	JOB_OBJECT_LIMIT_BREAKAWAY_OK     DWORD = 0x00000800

	TOKEN_ASSIGN_PRIMARY DWORD = 0x0001

	PROCESS_TERMINATE                 DWORD = 0x0001
	PROCESS_QUERY_INFORMATION         DWORD = 0x0400
	PROCESS_QUERY_LIMITED_INFORMATION DWORD = 0x1000
	SYNCHRONIZE                       DWORD = 0x00100000
	TOKEN_DUPLICATE      DWORD = 0x0002
	TOKEN_QUERY          DWORD = 0x0008
	TOKEN_ADJUST_DEFAULT DWORD = 0x0080

	TokenUser            int32 = 1
	TokenIntegrityLevel  int32 = 25
	TokenIsAppContainer  int32 = 29
	SecurityImpersonation int32 = 2
	TokenPrimary         int32 = 1

	SE_GROUP_INTEGRITY DWORD = 0x00000020
	SE_GROUP_ENABLED   DWORD = 0x00000004

	SECURITY_MANDATORY_LOW_RID    DWORD = 0x1000
	SECURITY_MANDATORY_MEDIUM_RID DWORD = 0x2000

	E_ALREADY_EXISTS HRESULT = -2147024713 // (HRESULT)0x800700B7

	LowIntegritySidString    = "S-1-16-4096"
	MediumIntegritySidString = "S-1-16-8192"

	LABEL_SECURITY_INFORMATION      DWORD = 0x00000010
	DACL_SECURITY_INFORMATION       DWORD = 0x00000004
	SYSTEM_MANDATORY_LABEL_NO_WRITE_UP DWORD = 0x1
	ACCESS_ALLOWED_ACE_TYPE            uint8 = 0x0
	ACCESS_DENIED_ACE_TYPE             uint8 = 0x1
	SYSTEM_MANDATORY_LABEL_ACE_TYPE    uint8 = 0x11
	OBJECT_INHERIT_ACE                 uint8 = 0x1
	CONTAINER_INHERIT_ACE              uint8 = 0x2
	INHERITED_ACE                      uint8 = 0x10
	ACL_REVISION                       DWORD = 2

	SE_FILE_OBJECT   int32 = 1
	SE_REGISTRY_KEY  int32 = 4
	SE_KERNEL_OBJECT int32 = 6

	GRANT_ACCESS  int32 = 1
	REVOKE_ACCESS int32 = 4
	DENY_ACCESS   int32 = 3

	SUB_CONTAINERS_AND_OBJECTS_INHERIT DWORD = 0x3
	NO_INHERITANCE                     DWORD = 0x0
	TRUSTEE_IS_SID                     int32 = 0
	TRUSTEE_IS_UNKNOWN                 int32 = 0

	FILE_ALL_ACCESS            DWORD = 0x001F01FF
	FILE_GENERIC_READ_EXECUTE  DWORD = 0x001200A9
	FILE_MODIFY                DWORD = 0x00010156
	WRITE_OWNER                DWORD = 0x00080000

	HKEY_CLASSES_ROOT  HKEY = 0x80000000
	HKEY_CURRENT_USER  HKEY = 0x80000001
	HKEY_LOCAL_MACHINE HKEY = 0x80000002
	HKEY_USERS         HKEY = 0x80000003

	KEY_READ       DWORD = 0x20019
	KEY_WRITE      DWORD = 0x20006
	KEY_ALL_ACCESS DWORD = 0xF003F

	REG_NONE      DWORD = 0
	REG_SZ        DWORD = 1
	REG_EXPAND_SZ DWORD = 2
	REG_BINARY    DWORD = 3
	REG_DWORD     DWORD = 4
	REG_MULTI_SZ  DWORD = 7

	ERROR_SUCCESS          = 0
	ERROR_FILE_NOT_FOUND   = 2
	ERROR_PATH_NOT_FOUND   = 3
	ERROR_ACCESS_DENIED    = 5
	ERROR_SHARING_VIOLATION = 32
	ERROR_NOT_FOUND        = 1168
	ERROR_NO_MORE_ITEMS    = 259
	ERROR_BROKEN_PIPE      = 109

	FSCTL_SET_REPARSE_POINT DWORD = 0x000900A4
	FSCTL_GET_REPARSE_POINT DWORD = 0x000900A8
	FSCTL_SET_SPARSE        DWORD = 0x000900C4

	IO_REPARSE_TAG_MOUNT_POINT DWORD = 0xA0000003

	AF_INET   int32 = 2
	AF_INET6  int32 = 23
	AF_UNIX   int32 = 1
	SOCK_STREAM int32 = 1
	IPPROTO_TCP int32 = 6
	SOL_SOCKET  int32 = 0xFFFF
	SO_REUSEADDR int32 = 0x0004
	SO_EXCLUSIVEADDRUSE int32 = ^int32(0x0004)
	FIONBIO     int32 = -2147195266 // (int32)0x8004667E
	SD_RECEIVE  int32 = 0
	SD_SEND     int32 = 1
	SD_BOTH     int32 = 2

	SO_ERROR            int32 = 0x1007
	WSA_FLAG_OVERLAPPED DWORD = 0x01
	SOCKET_ERROR        int32 = -1
	FROM_PROTOCOL_INFO  int32 = -1

	WSAEWOULDBLOCK   = 10035
	WSAEACCES        = 10013
	WSAEADDRINUSE    = 10048
	WSAEAFNOSUPPORT  = 10047
	WSAEADDRNOTAVAIL = 10049
	WSAECONNREFUSED  = 10061
	WSAETIMEDOUT     = 10060

	ENABLE_PROCESSED_INPUT DWORD = 0x0001
	ENABLE_LINE_INPUT      DWORD = 0x0002
	ENABLE_ECHO_INPUT      DWORD = 0x0004

	LOAD_LIBRARY_SEARCH_SYSTEM32 DWORD = 0x800

	CRED_TYPE_GENERIC          DWORD = 1
	CRED_PERSIST_LOCAL_MACHINE DWORD = 2

	CERT_STORE_PROV_SYSTEM_W       uintptr = 10
	CERT_STORE_READONLY_FLAG       DWORD   = 0x00008000
	CERT_SYSTEM_STORE_CURRENT_USER DWORD   = 1 << 16
	CERT_SYSTEM_STORE_LOCAL_MACHINE DWORD  = 2 << 16

	SHCNE_ASSOCCHANGED int32 = 0x08000000
	SHCNF_IDLIST       uint32 = 0
)

// GUID is a Win32 128-bit globally unique identifier.
type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	FOLDERID_Programs = GUID{0xA77F5D77, 0x2E2B, 0x44C3, [8]byte{0xA6, 0xA2, 0xAB, 0xA6, 0x01, 0x05, 0x4A, 0x51}}
	CLSID_ShellLink   = GUID{0x00021401, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	IID_IShellLinkW   = GUID{0x000214F9, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	IID_IPersistFile  = GUID{0x0000010B, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
)

type SID_IDENTIFIER_AUTHORITY struct {
	Value [6]byte
}

// Win32 structs
type STARTUPINFOW struct {
	Cb              DWORD
	LpReserved      *uint16
	LpDesktop       *uint16
	LpTitle         *uint16
	DwX             DWORD
	DwY             DWORD
	DwXSize         DWORD
	DwYSize         DWORD
	DwXCountChars   DWORD
	DwYCountChars   DWORD
	DwFillAttribute DWORD
	DwFlags         DWORD
	WShowWindow     WORD
	CbReserved2     WORD
	LpReserved2     *byte
	HStdInput       HANDLE
	HStdOutput      HANDLE
	HStdError       HANDLE
}

type STARTUPINFOEXW struct {
	StartupInfo     STARTUPINFOW
	LpAttributeList uintptr
}

type PROCESS_INFORMATION struct {
	HProcess    HANDLE
	HThread     HANDLE
	DwProcessId DWORD
	DwThreadId  DWORD
}

type SECURITY_ATTRIBUTES struct {
	NLength              DWORD
	LpSecurityDescriptor uintptr
	BInheritHandle       BOOL
}

type JOBOBJECT_BASIC_LIMIT_INFORMATION struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              DWORD
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      DWORD
	Affinity                uintptr
	PriorityClass           DWORD
	SchedulingClass         DWORD
}

type IO_COUNTERS struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type JOBOBJECT_EXTENDED_LIMIT_INFORMATION struct {
	BasicLimitInformation JOBOBJECT_BASIC_LIMIT_INFORMATION
	IoInfo                IO_COUNTERS
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type JOBOBJECT_BASIC_ACCOUNTING_INFORMATION struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       DWORD
	TotalProcesses            DWORD
	ActiveProcesses           DWORD
	TotalTerminatedProcesses  DWORD
}

type SID_AND_ATTRIBUTES struct {
	Sid        PSID
	Attributes DWORD
}

type SECURITY_CAPABILITIES struct {
	AppContainerSid PSID
	Capabilities    *SID_AND_ATTRIBUTES
	CapabilityCount DWORD
	Reserved        DWORD
}

type TOKEN_MANDATORY_LABEL struct {
	Label SID_AND_ATTRIBUTES
}

type TOKEN_USER struct {
	User SID_AND_ATTRIBUTES
}

type ACL struct {
	AclRevision uint8
	Sbz1        uint8
	AclSize     uint16
	AceCount    uint16
	Sbz2        uint16
}

type ACE_HEADER struct {
	AceType uint8
	AceFlags uint8
	AceSize uint16
}

type ACCESS_ALLOWED_ACE struct {
	Header ACE_HEADER
	Mask   DWORD
	SidStart DWORD
}

type TRUSTEE_W struct {
	PMultipleTrustee         uintptr
	MultipleTrusteeOperation int32
	TrusteeForm              int32
	TrusteeType              int32
	PtstrName                uintptr
}

type EXPLICIT_ACCESS_W struct {
	GrfAccessPermissions DWORD
	GrfAccessMode        int32
	GrfInheritance       DWORD
	Trustee              TRUSTEE_W
}

type FILETIME struct {
	DwLowDateTime  DWORD
	DwHighDateTime DWORD
}

type CREDENTIALW struct {
	Flags              DWORD
	Type               DWORD
	TargetName         *uint16
	Comment            *uint16
	LastWritten        FILETIME
	CredentialBlobSize DWORD
	CredentialBlob     *byte
	Persist            DWORD
	AttributeCount     DWORD
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

type WSADATA struct {
	Version      WORD
	HighVersion  WORD
	MaxSockets   uint16
	MaxUdpDg     uint16
	VendorInfo   *byte
	Description  [257]byte
	SystemStatus [129]byte
}

type WSAPROTOCOL_INFOW struct {
	Data [157]uint32
}

type SockaddrIn struct {
	Family uint16
	Port   uint16
	Addr   [4]byte
	Zero   [8]byte
}

type SockaddrIn6 struct {
	Family   uint16
	Port     uint16
	Flowinfo uint32
	Addr     [16]byte
	ScopeID  uint32
}

type SockaddrUn struct {
	Family uint16
	Path   [108]byte
}

type FdSet struct {
	Count uint32
	Array [64]SOCKET
}

type Timeval struct {
	Sec  int32
	Usec int32
}

type CERT_CONTEXT struct {
	Encoding   DWORD
	Encoded    *byte
	EncodedLen DWORD
	Info       uintptr
	Store      uintptr
}

type CERT_ENHKEY_USAGE struct {
	Count       DWORD
	Identifiers *uintptr
}

type OVERLAPPED struct {
	Internal     uintptr
	InternalHigh uintptr
	Offset       DWORD
	OffsetHigh   DWORD
	HEvent       HANDLE
}

type FILE_NOTIFY_INFORMATION struct {
	NextEntryOffset DWORD
	Action          DWORD
	FileNameLength  DWORD
	FileName        [1]uint16
}

type FILE_SET_SPARSE_BUFFER struct {
	SetSparse uint8
}

type MOUNT_POINT_REPARSE_HEADER struct {
	ReparseTag           DWORD
	ReparseDataLength    uint16
	Reserved             uint16
	SubstituteNameOffset uint16
	SubstituteNameLength uint16
	PrintNameOffset      uint16
	PrintNameLength      uint16
}

// Lazy DLL references
var (
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")
	modAdvapi32 = syscall.NewLazyDLL("advapi32.dll")
	modUserenv  = syscall.NewLazyDLL("userenv.dll")
	modWs2_32   = syscall.NewLazyDLL("ws2_32.dll")
	modOle32    = syscall.NewLazyDLL("ole32.dll")
	modShell32  = syscall.NewLazyDLL("shell32.dll")
	modUser32   = syscall.NewLazyDLL("user32.dll")
	modCrypt32  = syscall.NewLazyDLL("crypt32.dll")
	modNtdll    = syscall.NewLazyDLL("ntdll.dll")

	modNetIso = syscall.NewLazyDLL("api-ms-win-net-isolation-l1-1-0.dll")
	modSecBase = syscall.NewLazyDLL("api-ms-win-security-base-l1-2-2.dll")
)

// Procs
var (
	procGetLastError               = modKernel32.NewProc("GetLastError")
	procCloseHandle                = modKernel32.NewProc("CloseHandle")
	procGetStdHandle               = modKernel32.NewProc("GetStdHandle")
	procSetStdHandle               = modKernel32.NewProc("SetStdHandle")
	procSetHandleInformation       = modKernel32.NewProc("SetHandleInformation")
	procCreateFileW                = modKernel32.NewProc("CreateFileW")
	procReadFile                   = modKernel32.NewProc("ReadFile")
	procWriteFile                  = modKernel32.NewProc("WriteFile")
	procCreatePipe                 = modKernel32.NewProc("CreatePipe")
	procGetFileType                = modKernel32.NewProc("GetFileType")
	procGetModuleFileNameW         = modKernel32.NewProc("GetModuleFileNameW")
	procGetSystemDirectoryW        = modKernel32.NewProc("GetSystemDirectoryW")
	procLoadLibraryExW             = modKernel32.NewProc("LoadLibraryExW")
	procGetProcAddress             = modKernel32.NewProc("GetProcAddress")
	procGetSystemTimeAsFileTime    = modKernel32.NewProc("GetSystemTimeAsFileTime")
	procCreateProcessW             = modKernel32.NewProc("CreateProcessW")
	procInitializeProcThreadAttributeList = modKernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute  = modKernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttributeList = modKernel32.NewProc("DeleteProcThreadAttributeList")
	procWaitForSingleObject        = modKernel32.NewProc("WaitForSingleObject")
	procWaitForMultipleObjects     = modKernel32.NewProc("WaitForMultipleObjects")
	procCreateMutexW               = modKernel32.NewProc("CreateMutexW")
	procReleaseMutex               = modKernel32.NewProc("ReleaseMutex")
	procCreateEventW               = modKernel32.NewProc("CreateEventW")
	procOpenEventW                 = modKernel32.NewProc("OpenEventW")
	procSetEvent                   = modKernel32.NewProc("SetEvent")
	procOpenProcess                = modKernel32.NewProc("OpenProcess")
	procGetCurrentProcessId        = modKernel32.NewProc("GetCurrentProcessId")
	procGetProcessId               = modKernel32.NewProc("GetProcessId")
	procGetCurrentProcess          = modKernel32.NewProc("GetCurrentProcess")
	procGetExitCodeProcess         = modKernel32.NewProc("GetExitCodeProcess")
	procTerminateProcess           = modKernel32.NewProc("TerminateProcess")
	procCreateJobObjectW           = modKernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject    = modKernel32.NewProc("SetInformationJobObject")
	procQueryInformationJobObject  = modKernel32.NewProc("QueryInformationJobObject")
	procTerminateJobObject         = modKernel32.NewProc("TerminateJobObject")
	procDeviceIoControl            = modKernel32.NewProc("DeviceIoControl")
	procGetFileAttributesW         = modKernel32.NewProc("GetFileAttributesW")
	procCreateDirectoryW           = modKernel32.NewProc("CreateDirectoryW")
	procRemoveDirectoryW           = modKernel32.NewProc("RemoveDirectoryW")
	procMoveFileExW                = modKernel32.NewProc("MoveFileExW")
	procDefineDosDeviceW           = modKernel32.NewProc("DefineDosDeviceW")
	procQueryDosDeviceW            = modKernel32.NewProc("QueryDosDeviceW")
	procSleep                      = modKernel32.NewProc("Sleep")
	procGetConsoleMode             = modKernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = modKernel32.NewProc("SetConsoleMode")
	procSetConsoleCtrlHandler      = modKernel32.NewProc("SetConsoleCtrlHandler")
	procReadConsoleW               = modKernel32.NewProc("ReadConsoleW")
	procGetConsoleOutputCP         = modKernel32.NewProc("GetConsoleOutputCP")
	procSetConsoleOutputCP         = modKernel32.NewProc("SetConsoleOutputCP")
	procLocalFree                  = modKernel32.NewProc("LocalFree")
	procReadDirectoryChangesW      = modKernel32.NewProc("ReadDirectoryChangesW")
	procCreateIoCompletionPort     = modKernel32.NewProc("CreateIoCompletionPort")
	procGetQueuedCompletionStatus  = modKernel32.NewProc("GetQueuedCompletionStatus")
	procPostQueuedCompletionStatus = modKernel32.NewProc("PostQueuedCompletionStatus")
	procCancelIoEx                 = modKernel32.NewProc("CancelIoEx")
	procLockFileEx                 = modKernel32.NewProc("LockFileEx")
	procUnlockFileEx               = modKernel32.NewProc("UnlockFileEx")

	// advapi32
	procOpenProcessToken           = modAdvapi32.NewProc("OpenProcessToken")
	procDuplicateTokenEx           = modAdvapi32.NewProc("DuplicateTokenEx")
	procSetTokenInformation        = modAdvapi32.NewProc("SetTokenInformation")
	procGetTokenInformation        = modAdvapi32.NewProc("GetTokenInformation")
	procCreateProcessAsUserW       = modAdvapi32.NewProc("CreateProcessAsUserW")
	procInitializeAcl              = modAdvapi32.NewProc("InitializeAcl")
	procAddMandatoryAce            = modAdvapi32.NewProc("AddMandatoryAce")
	procDeleteAce                  = modAdvapi32.NewProc("DeleteAce")
	procGetAce                     = modAdvapi32.NewProc("GetAce")
	procFreeSid                    = modAdvapi32.NewProc("FreeSid")
	procEqualSid                   = modAdvapi32.NewProc("EqualSid")
	procGetLengthSid               = modAdvapi32.NewProc("GetLengthSid")
	procGetSidIdentifierAuthority  = modAdvapi32.NewProc("GetSidIdentifierAuthority")
	procGetSidSubAuthorityCount    = modAdvapi32.NewProc("GetSidSubAuthorityCount")
	procGetSidSubAuthority         = modAdvapi32.NewProc("GetSidSubAuthority")
	procConvertStringSidToSidW     = modAdvapi32.NewProc("ConvertStringSidToSidW")
	procConvertSidToStringSidW     = modAdvapi32.NewProc("ConvertSidToStringSidW")
	procGetNamedSecurityInfoW      = modAdvapi32.NewProc("GetNamedSecurityInfoW")
	procSetNamedSecurityInfoW      = modAdvapi32.NewProc("SetNamedSecurityInfoW")
	procSetSecurityInfo            = modAdvapi32.NewProc("SetSecurityInfo")
	procSetEntriesInAclW           = modAdvapi32.NewProc("SetEntriesInAclW")
	procRegCreateKeyExW            = modAdvapi32.NewProc("RegCreateKeyExW")
	procRegOpenKeyExW              = modAdvapi32.NewProc("RegOpenKeyExW")
	procRegCloseKey                = modAdvapi32.NewProc("RegCloseKey")
	procRegSetValueExW             = modAdvapi32.NewProc("RegSetValueExW")
	procRegQueryValueExW           = modAdvapi32.NewProc("RegQueryValueExW")
	procRegEnumKeyExW              = modAdvapi32.NewProc("RegEnumKeyExW")
	procRegQueryInfoKeyW           = modAdvapi32.NewProc("RegQueryInfoKeyW")
	procRegDeleteTreeW             = modAdvapi32.NewProc("RegDeleteTreeW")
	procRegDeleteKeyW              = modAdvapi32.NewProc("RegDeleteKeyW")
	procRegDeleteValueW            = modAdvapi32.NewProc("RegDeleteValueW")
	procRegEnumValueW              = modAdvapi32.NewProc("RegEnumValueW")
	procRegLoadAppKeyW             = modAdvapi32.NewProc("RegLoadAppKeyW")
	procCredReadW                  = modAdvapi32.NewProc("CredReadW")
	procCredWriteW                 = modAdvapi32.NewProc("CredWriteW")
	procCredDeleteW                = modAdvapi32.NewProc("CredDeleteW")
	procCredFree                   = modAdvapi32.NewProc("CredFree")

	// userenv
	procCreateAppContainerProfile             = modUserenv.NewProc("CreateAppContainerProfile")
	procDeriveAppContainerSidFromAppContainerName = modUserenv.NewProc("DeriveAppContainerSidFromAppContainerName")
	procDeleteAppContainerProfile             = modUserenv.NewProc("DeleteAppContainerProfile")
	procCreateEnvironmentBlock                = modUserenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock               = modUserenv.NewProc("DestroyEnvironmentBlock")

	// api-ms-win-net-isolation
	procNetworkIsolationGetAppContainerConfig = modNetIso.NewProc("NetworkIsolationGetAppContainerConfig")
	procNetworkIsolationSetAppContainerConfig = modNetIso.NewProc("NetworkIsolationSetAppContainerConfig")

	// api-ms-win-security-base
	procDeriveCapabilitySidsFromName = modSecBase.NewProc("DeriveCapabilitySidsFromName")

	// ws2_32
	procWSAStartup         = modWs2_32.NewProc("WSAStartup")
	procWSAGetLastError    = modWs2_32.NewProc("WSAGetLastError")
	procWSASocketW         = modWs2_32.NewProc("WSASocketW")
	procClosesocket        = modWs2_32.NewProc("closesocket")
	procBind               = modWs2_32.NewProc("bind")
	procListen             = modWs2_32.NewProc("listen")
	procAccept             = modWs2_32.NewProc("accept")
	procConnect            = modWs2_32.NewProc("connect")
	procSend               = modWs2_32.NewProc("send")
	procRecv               = modWs2_32.NewProc("recv")
	procShutdown           = modWs2_32.NewProc("shutdown")
	procSetsockopt         = modWs2_32.NewProc("setsockopt")
	procGetsockopt         = modWs2_32.NewProc("getsockopt")
	procGetsockname        = modWs2_32.NewProc("getsockname")
	procIoctlsocket        = modWs2_32.NewProc("ioctlsocket")
	procSelect             = modWs2_32.NewProc("select")
	procWSADuplicateSocketW = modWs2_32.NewProc("WSADuplicateSocketW")

	// ole32 & shell32 & user32 & crypt32
	procCoInitializeEx          = modOle32.NewProc("CoInitializeEx")
	procCoUninitialize          = modOle32.NewProc("CoUninitialize")
	procCoCreateInstance        = modOle32.NewProc("CoCreateInstance")
	procCoTaskMemFree           = modOle32.NewProc("CoTaskMemFree")
	procSHGetKnownFolderPath    = modShell32.NewProc("SHGetKnownFolderPath")
	procSHChangeNotify          = modShell32.NewProc("SHChangeNotify")
	procShell_NotifyIconW       = modShell32.NewProc("Shell_NotifyIconW")
	procSHQueryUserNotificationState = modShell32.NewProc("SHQueryUserNotificationState")
	procMessageBoxW             = modUser32.NewProc("MessageBoxW")
	procCertOpenStore           = modCrypt32.NewProc("CertOpenStore")
	procCertCloseStore          = modCrypt32.NewProc("CertCloseStore")
	procCertEnumCertificatesInStore = modCrypt32.NewProc("CertEnumCertificatesInStore")
	procCertGetEnhancedKeyUsage = modCrypt32.NewProc("CertGetEnhancedKeyUsage")

	// ntdll
	procNtQueryInformationFile  = modNtdll.NewProc("NtQueryInformationFile")
)

// WTF-8 / UTF-16 conversions preserving unpaired surrogates
func UTF16Ptr(s string) *uint16 {
	u := UTF8ToUTF16(s)
	u = append(u, 0)
	return &u[0]
}

func UTF8ToUTF16(s string) []uint16 {
	var res []uint16
	for i := 0; i < len(s); {
		b := s[i]
		if b < 0x80 {
			res = append(res, uint16(b))
			i++
		} else if b < 0xE0 && i+1 < len(s) {
			r := (uint16(b&0x1F) << 6) | uint16(s[i+1]&0x3F)
			res = append(res, r)
			i += 2
		} else if b < 0xF0 && i+2 < len(s) {
			r := (uint16(b&0x0F) << 12) | (uint16(s[i+1]&0x3F) << 6) | uint16(s[i+2]&0x3F)
			res = append(res, r)
			i += 3
		} else if b < 0xF8 && i+3 < len(s) {
			cp := (uint32(b&0x07) << 18) | (uint32(s[i+1]&0x3F) << 12) | (uint32(s[i+2]&0x3F) << 6) | uint32(s[i+3]&0x3F)
			cp -= 0x10000
			res = append(res, uint16(0xD800|(cp>>10)), uint16(0xDC00|(cp&0x3FF)))
			i += 4
		} else {
			res = append(res, uint16(b))
			i++
		}
	}
	return res
}

func UTF16ToUTF8(s []uint16) string {
	var buf []byte
	for i := 0; i < len(s); i++ {
		v := s[i]
		if v == 0 {
			break
		}
		if v < 0x80 {
			buf = append(buf, byte(v))
		} else if v < 0x800 {
			buf = append(buf, byte(0xC0|(v>>6)), byte(0x80|(v&0x3F)))
		} else if v >= 0xD800 && v <= 0xDBFF && i+1 < len(s) && s[i+1] >= 0xDC00 && s[i+1] <= 0xDFFF {
			// surrogate pair
			cp := (uint32(v-0xD800) << 10) | uint32(s[i+1]-0xDC00) + 0x10000
			buf = append(buf, byte(0xF0|(cp>>18)), byte(0x80|((cp>>12)&0x3F)), byte(0x80|((cp>>6)&0x3F)), byte(0x80|(cp&0x3F)))
			i++
		} else {
			// lone surrogate or 3-byte BMP char
			buf = append(buf, byte(0xE0|(v>>12)), byte(0x80|((v>>6)&0x3F)), byte(0x80|(v&0x3F)))
		}
	}
	return string(buf)
}

func UTF16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	var s []uint16
	for *p != 0 {
		s = append(s, *p)
		p = (*uint16)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + 2))
	}
	return UTF16ToUTF8(s)
}

// Win32 API functions
func GetLastError() DWORD {
	r, _, _ := procGetLastError.Call()
	return DWORD(r)
}

func CloseHandle(h HANDLE) bool {
	r, _, _ := procCloseHandle.Call(h)
	return r != 0
}

func GetStdHandle(which DWORD) HANDLE {
	r, _, _ := procGetStdHandle.Call(uintptr(which))
	return r
}

func SetStdHandle(which DWORD, h HANDLE) bool {
	r, _, _ := procSetStdHandle.Call(uintptr(which), h)
	return r != 0
}

func DeviceIoControl(h HANDLE, ioctlCode DWORD, inBuf unsafe.Pointer, inSize DWORD, outBuf unsafe.Pointer, outSize DWORD, bytesReturned *DWORD, overlapped *OVERLAPPED) error {
	r, _, err := procDeviceIoControl.Call(
		h,
		uintptr(ioctlCode),
		uintptr(inBuf),
		uintptr(inSize),
		uintptr(outBuf),
		uintptr(outSize),
		uintptr(unsafe.Pointer(bytesReturned)),
		uintptr(unsafe.Pointer(overlapped)),
	)
	if r == 0 {
		return err
	}
	return nil
}

func SetHandleInformation(h HANDLE, mask, flags DWORD) bool {
	r, _, _ := procSetHandleInformation.Call(h, uintptr(mask), uintptr(flags))
	return r != 0
}

func GetSystemTimeAsFileTime(ft *int64) {
	procGetSystemTimeAsFileTime.Call(uintptr(unsafe.Pointer(ft)))
}

func CreateFile(name string, access, share DWORD, sec *SECURITY_ATTRIBUTES, disposition, flags DWORD, template HANDLE) (HANDLE, error) {
	namePtr := UTF16Ptr(name)
	r, _, err := procCreateFileW.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(access),
		uintptr(share),
		uintptr(unsafe.Pointer(sec)),
		uintptr(disposition),
		uintptr(flags),
		template,
	)
	if r == uintptr(INVALID_HANDLE_VALUE) {
		return INVALID_HANDLE_VALUE, err
	}
	return r, nil
}

func ReadFile(h HANDLE, buf []byte) (int, error) {
	var read DWORD
	var p *byte
	if len(buf) > 0 {
		p = &buf[0]
	}
	r, _, err := procReadFile.Call(h, uintptr(unsafe.Pointer(p)), uintptr(len(buf)), uintptr(unsafe.Pointer(&read)), 0)
	if r == 0 {
		return int(read), err
	}
	return int(read), nil
}

func WriteFile(h HANDLE, buf []byte) (int, error) {
	var written DWORD
	var p *byte
	if len(buf) > 0 {
		p = &buf[0]
	}
	r, _, err := procWriteFile.Call(h, uintptr(unsafe.Pointer(p)), uintptr(len(buf)), uintptr(unsafe.Pointer(&written)), 0)
	if r == 0 {
		return int(written), err
	}
	return int(written), nil
}

func CreatePipe(sec *SECURITY_ATTRIBUTES, size DWORD) (read, write HANDLE, err error) {
	var r, w HANDLE
	res, _, e := procCreatePipe.Call(uintptr(unsafe.Pointer(&r)), uintptr(unsafe.Pointer(&w)), uintptr(unsafe.Pointer(sec)), uintptr(size))
	if res == 0 {
		return 0, 0, e
	}
	return r, w, nil
}

func GetFileType(h HANDLE) DWORD {
	r, _, _ := procGetFileType.Call(h)
	return DWORD(r)
}

func GetModuleFileName() (string, error) {
	buf := make([]uint16, 4096)
	r, _, err := procGetModuleFileNameW.Call(0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return "", err
	}
	return UTF16ToUTF8(buf[:r]), nil
}

func GetSystemDirectory() (string, error) {
	buf := make([]uint16, 1024)
	r, _, err := procGetSystemDirectoryW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return "", err
	}
	return UTF16ToUTF8(buf[:r]), nil
}

func SystemProgram(name string) (string, error) {
	sys, err := GetSystemDirectory()
	if err != nil {
		return "", err
	}
	return sys + "\\" + name, nil
}

func WaitForSingleObject(h HANDLE, ms DWORD) DWORD {
	r, _, _ := procWaitForSingleObject.Call(h, uintptr(ms))
	return DWORD(r)
}

func WaitForMultipleObjects(handles []HANDLE, waitAll bool, ms DWORD) DWORD {
	if len(handles) == 0 {
		return 0
	}
	var wAll uintptr
	if waitAll {
		wAll = 1
	}
	r, _, _ := procWaitForMultipleObjects.Call(uintptr(len(handles)), uintptr(unsafe.Pointer(&handles[0])), wAll, uintptr(ms))
	return DWORD(r)
}

func CreateMutex(name string) (HANDLE, error) {
	var namePtr *uint16
	if name != "" {
		namePtr = UTF16Ptr(name)
	}
	r, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(namePtr)))
	if r == 0 {
		return 0, err
	}
	return r, nil
}

func ReleaseMutex(m HANDLE) bool {
	r, _, _ := procReleaseMutex.Call(m)
	return r != 0
}

func CreateEvent(name string, manualReset, initialState bool) (HANDLE, error) {
	var namePtr *uint16
	if name != "" {
		namePtr = UTF16Ptr(name)
	}
	var mReset, iState uintptr
	if manualReset {
		mReset = 1
	}
	if initialState {
		iState = 1
	}
	r, _, err := procCreateEventW.Call(0, mReset, iState, uintptr(unsafe.Pointer(namePtr)))
	if r == 0 {
		return 0, err
	}
	return r, nil
}

func OpenEvent(name string, access DWORD) (HANDLE, error) {
	namePtr := UTF16Ptr(name)
	r, _, err := procOpenEventW.Call(uintptr(access), 0, uintptr(unsafe.Pointer(namePtr)))
	if r == 0 {
		return 0, err
	}
	return r, nil
}

func SetEvent(e HANDLE) bool {
	r, _, _ := procSetEvent.Call(e)
	return r != 0
}

func OpenProcess(access DWORD, inherit bool, pid DWORD) (HANDLE, error) {
	var inh uintptr
	if inherit {
		inh = 1
	}
	r, _, err := procOpenProcess.Call(uintptr(access), inh, uintptr(pid))
	if r == 0 {
		return 0, err
	}
	return r, nil
}

func GetCurrentProcessId() DWORD {
	r, _, _ := procGetCurrentProcessId.Call()
	return DWORD(r)
}

func GetCurrentProcess() HANDLE {
	r, _, _ := procGetCurrentProcess.Call()
	return r
}

func GetExitCodeProcess(h HANDLE) (DWORD, error) {
	var code DWORD
	r, _, err := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code)))
	if r == 0 {
		return 0, err
	}
	return code, nil
}

func TerminateProcess(h HANDLE, code uint32) bool {
	r, _, _ := procTerminateProcess.Call(h, uintptr(code))
	return r != 0
}

func CreateJobObject(name string) (HANDLE, error) {
	var namePtr *uint16
	if name != "" {
		namePtr = UTF16Ptr(name)
	}
	r, _, err := procCreateJobObjectW.Call(0, uintptr(unsafe.Pointer(namePtr)))
	if r == 0 {
		return 0, err
	}
	return r, nil
}

func SetInformationJobObject(job HANDLE, class int32, info unsafe.Pointer, length DWORD) error {
	r, _, err := procSetInformationJobObject.Call(job, uintptr(class), uintptr(info), uintptr(length))
	if r == 0 {
		return err
	}
	return nil
}

func QueryInformationJobObject(job HANDLE, class int32, info unsafe.Pointer, length DWORD) error {
	r, _, err := procQueryInformationJobObject.Call(job, uintptr(class), uintptr(info), uintptr(length), 0)
	if r == 0 {
		return err
	}
	return nil
}

func TerminateJobObject(job HANDLE, code uint32) bool {
	r, _, _ := procTerminateJobObject.Call(job, uintptr(code))
	return r != 0
}

func GetFileAttributes(name string) (DWORD, error) {
	namePtr := UTF16Ptr(name)
	r, _, err := procGetFileAttributesW.Call(uintptr(unsafe.Pointer(namePtr)))
	if DWORD(r) == INVALID_FILE_ATTRIBUTES {
		return INVALID_FILE_ATTRIBUTES, err
	}
	return DWORD(r), nil
}

func CreateDirectory(name string) error {
	namePtr := UTF16Ptr(name)
	r, _, err := procCreateDirectoryW.Call(uintptr(unsafe.Pointer(namePtr)), 0)
	if r == 0 {
		return err
	}
	return nil
}

func RemoveDirectory(path string) error {
	pathPtr := UTF16Ptr(path)
	r, _, err := procRemoveDirectoryW.Call(uintptr(unsafe.Pointer(pathPtr)))
	if r == 0 {
		return err
	}
	return nil
}

func MoveFileEx(existing, newPath string, flags DWORD) error {
	exPtr := UTF16Ptr(existing)
	newPtr := UTF16Ptr(newPath)
	r, _, err := procMoveFileExW.Call(uintptr(unsafe.Pointer(exPtr)), uintptr(unsafe.Pointer(newPtr)), uintptr(flags))
	if r == 0 {
		return err
	}
	return nil
}

const (
	DDD_RAW_TARGET_PATH       = 0x00000001
	DDD_REMOVE_DEFINITION     = 0x00000002
	DDD_EXACT_MATCH_ON_REMOVE = 0x00000004
	DDD_NO_BROADCAST_SYSTEM   = 0x00000008
)

func DefineDosDevice(flags DWORD, device, target string) error {
	devPtr := UTF16Ptr(device)
	var tgtPtr *uint16
	if target != "" {
		tgtPtr = UTF16Ptr(target)
	}
	r, _, err := procDefineDosDeviceW.Call(uintptr(flags), uintptr(unsafe.Pointer(devPtr)), uintptr(unsafe.Pointer(tgtPtr)))
	if r == 0 {
		return err
	}
	return nil
}

func QueryDosDevice(device string) (string, error) {
	var devPtr *uint16
	if device != "" {
		devPtr = UTF16Ptr(device)
	}
	buf := make([]uint16, 4096)
	r, _, err := procQueryDosDeviceW.Call(uintptr(unsafe.Pointer(devPtr)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return "", err
	}
	return UTF16ToUTF8(buf[:r]), nil
}

func Sleep(ms DWORD) {
	procSleep.Call(uintptr(ms))
}

// SynchronousIo checks if I/O on handle is synchronous. Returns true, false, or error.
func SynchronousIo(h HANDLE) (bool, error) {
	type ioStatusBlock struct {
		Status, Information uintptr
	}
	var iosb ioStatusBlock
	var mode uint32
	// FileModeInformation = 16
	r, _, _ := procNtQueryInformationFile.Call(h, uintptr(unsafe.Pointer(&iosb)), uintptr(unsafe.Pointer(&mode)), 4, 16)
	if r != 0 {
		return false, fmt.Errorf("NtQueryInformationFile failed: 0x%x", r)
	}
	// FILE_SYNCHRONOUS_IO_ALERT (0x10) | FILE_SYNCHRONOUS_IO_NONALERT (0x20)
	return (mode & (0x10 | 0x20)) != 0, nil
}

func GetProcessId(process HANDLE) DWORD {
	r, _, _ := procGetProcessId.Call(process)
	return DWORD(r)
}

func WSAStartup(version WORD, data *WSADATA) (int32, error) {
	r, _, err := procWSAStartup.Call(uintptr(version), uintptr(unsafe.Pointer(data)))
	return int32(r), err
}

func WSAGetLastError() int32 {
	r, _, _ := procWSAGetLastError.Call()
	return int32(r)
}

func ErrorName(code int32) string {
	switch code {
	case 0:
		return "ERROR_SUCCESS"
	case 2:
		return "ERROR_FILE_NOT_FOUND"
	case 3:
		return "ERROR_PATH_NOT_FOUND"
	case 5:
		return "ERROR_ACCESS_DENIED"
	case 32:
		return "ERROR_SHARING_VIOLATION"
	case 109:
		return "ERROR_BROKEN_PIPE"
	case 259:
		return "ERROR_NO_MORE_ITEMS"
	case 1168:
		return "ERROR_NOT_FOUND"
	case 10013:
		return "WSAEACCES"
	case 10035:
		return "WSAEWOULDBLOCK"
	case 10048:
		return "WSAEADDRINUSE"
	case 10060:
		return "WSAETIMEDOUT"
	case 10061:
		return "WSAECONNREFUSED"
	default:
		return syscall.Errno(code).Error()
	}
}

func Errno(err error) int32 {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return int32(errno)
	}
	return WSAGetLastError()
}

func WSASocket(af, kind, proto int32, info *WSAPROTOCOL_INFOW, group uint32, flags DWORD) (SOCKET, error) {
	r, _, err := procWSASocketW.Call(uintptr(af), uintptr(kind), uintptr(proto), uintptr(unsafe.Pointer(info)), uintptr(group), uintptr(flags))
	return SOCKET(r), err
}

func Closesocket(s SOCKET) (int32, error) {
	r, _, err := procClosesocket.Call(uintptr(s))
	return int32(r), err
}

func Bind(s SOCKET, addr unsafe.Pointer, addrlen int32) (int32, error) {
	r, _, err := procBind.Call(uintptr(s), uintptr(addr), uintptr(addrlen))
	return int32(r), err
}

func Listen(s SOCKET, backlog int32) (int32, error) {
	r, _, err := procListen.Call(uintptr(s), uintptr(backlog))
	return int32(r), err
}

func Accept(s SOCKET, addr unsafe.Pointer, addrlen *int32) (SOCKET, error) {
	r, _, err := procAccept.Call(uintptr(s), uintptr(addr), uintptr(unsafe.Pointer(addrlen)))
	return SOCKET(r), err
}

func Connect(s SOCKET, addr unsafe.Pointer, addrlen int32) (int32, error) {
	r, _, err := procConnect.Call(uintptr(s), uintptr(addr), uintptr(addrlen))
	return int32(r), err
}

func Send(s SOCKET, buf []byte, flags int32) (int32, error) {
	var ptr uintptr
	if len(buf) > 0 {
		ptr = uintptr(unsafe.Pointer(&buf[0]))
	}
	r, _, err := procSend.Call(uintptr(s), ptr, uintptr(len(buf)), uintptr(flags))
	return int32(r), err
}

func Recv(s SOCKET, buf []byte, flags int32) (int32, error) {
	var ptr uintptr
	if len(buf) > 0 {
		ptr = uintptr(unsafe.Pointer(&buf[0]))
	}
	r, _, err := procRecv.Call(uintptr(s), ptr, uintptr(len(buf)), uintptr(flags))
	return int32(r), err
}

func Shutdown(s SOCKET, how int32) (int32, error) {
	r, _, err := procShutdown.Call(uintptr(s), uintptr(how))
	return int32(r), err
}

func Setsockopt(s SOCKET, level, optname int32, optval unsafe.Pointer, optlen int32) (int32, error) {
	r, _, err := procSetsockopt.Call(uintptr(s), uintptr(level), uintptr(optname), uintptr(optval), uintptr(optlen))
	return int32(r), err
}

func Getsockopt(s SOCKET, level, optname int32, optval unsafe.Pointer, optlen *int32) (int32, error) {
	r, _, err := procGetsockopt.Call(uintptr(s), uintptr(level), uintptr(optname), uintptr(optval), uintptr(unsafe.Pointer(optlen)))
	return int32(r), err
}

func Getsockname(s SOCKET, name unsafe.Pointer, namelen *int32) (int32, error) {
	r, _, err := procGetsockname.Call(uintptr(s), uintptr(name), uintptr(unsafe.Pointer(namelen)))
	return int32(r), err
}

func Ioctlsocket(s SOCKET, cmd int32, arg *uint32) (int32, error) {
	r, _, err := procIoctlsocket.Call(uintptr(s), uintptr(cmd), uintptr(unsafe.Pointer(arg)))
	return int32(r), err
}

func Select(nfds int32, readfds, writefds, exceptfds *FdSet, timeout *Timeval) (int32, error) {
	r, _, err := procSelect.Call(uintptr(nfds), uintptr(unsafe.Pointer(readfds)), uintptr(unsafe.Pointer(writefds)), uintptr(unsafe.Pointer(exceptfds)), uintptr(unsafe.Pointer(timeout)))
	return int32(r), err
}

func WSADuplicateSocket(s SOCKET, processId DWORD, protocolInfo *WSAPROTOCOL_INFOW) (int32, error) {
	r, _, err := procWSADuplicateSocketW.Call(uintptr(s), uintptr(processId), uintptr(unsafe.Pointer(protocolInfo)))
	return int32(r), err
}

func RegCreateKeyEx(key HKEY, subKey string, access DWORD) (HKEY, int32) {
	var subPtr *uint16
	if subKey != "" {
		subPtr = UTF16Ptr(subKey)
	}
	var out HKEY
	r, _, _ := procRegCreateKeyExW.Call(key, uintptr(unsafe.Pointer(subPtr)), 0, 0, 0, uintptr(access), 0, uintptr(unsafe.Pointer(&out)), 0)
	return out, int32(r)
}

func RegOpenKeyEx(key HKEY, subKey string, access DWORD) (HKEY, int32) {
	var subPtr *uint16
	if subKey != "" {
		subPtr = UTF16Ptr(subKey)
	}
	var out HKEY
	r, _, _ := procRegOpenKeyExW.Call(key, uintptr(unsafe.Pointer(subPtr)), 0, uintptr(access), uintptr(unsafe.Pointer(&out)))
	return out, int32(r)
}

func RegCloseKey(key HKEY) int32 {
	r, _, _ := procRegCloseKey.Call(key)
	return int32(r)
}

func RegSetValueEx(key HKEY, valueName *uint16, valType DWORD, data []byte) int32 {
	var dataPtr uintptr
	if len(data) > 0 {
		dataPtr = uintptr(unsafe.Pointer(&data[0]))
	}
	r, _, _ := procRegSetValueExW.Call(key, uintptr(unsafe.Pointer(valueName)), 0, uintptr(valType), dataPtr, uintptr(len(data)))
	return int32(r)
}

func RegQueryValueEx(key HKEY, valueName *uint16, valType *DWORD, data []byte, dataLen *DWORD) int32 {
	var dataPtr uintptr
	if len(data) > 0 {
		dataPtr = uintptr(unsafe.Pointer(&data[0]))
	}
	r, _, _ := procRegQueryValueExW.Call(key, uintptr(unsafe.Pointer(valueName)), 0, uintptr(unsafe.Pointer(valType)), dataPtr, uintptr(unsafe.Pointer(dataLen)))
	return int32(r)
}

func RegDeleteValue(key HKEY, valueName string) int32 {
	namePtr := UTF16Ptr(valueName)
	r, _, _ := procRegDeleteValueW.Call(key, uintptr(unsafe.Pointer(namePtr)))
	return int32(r)
}

func RegEnumKeyEx(key HKEY, index DWORD, name *uint16, nameLen *DWORD) int32 {
	r, _, _ := procRegEnumKeyExW.Call(key, uintptr(index), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(nameLen)), 0, 0, 0, 0)
	return int32(r)
}

func RegEnumValue(key HKEY, index DWORD, name *uint16, nameLen *DWORD, valType *DWORD, data *byte, dataLen *DWORD) int32 {
	r, _, _ := procRegEnumValueW.Call(key, uintptr(index), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(nameLen)), 0, uintptr(unsafe.Pointer(valType)), uintptr(unsafe.Pointer(data)), uintptr(unsafe.Pointer(dataLen)))
	return int32(r)
}

func RegQueryInfoKey(key HKEY, subKeys *DWORD, maxSubKeyLen *DWORD, values *DWORD, maxValueNameLen *DWORD, maxValueLen *DWORD) int32 {
	r, _, _ := procRegQueryInfoKeyW.Call(key, 0, 0, 0, uintptr(unsafe.Pointer(subKeys)), uintptr(unsafe.Pointer(maxSubKeyLen)), 0, uintptr(unsafe.Pointer(values)), uintptr(unsafe.Pointer(maxValueNameLen)), uintptr(unsafe.Pointer(maxValueLen)), 0, 0)
	return int32(r)
}

func RegDeleteTree(key HKEY, subKey string) int32 {
	var subPtr *uint16
	if subKey != "" {
		subPtr = UTF16Ptr(subKey)
	}
	r, _, _ := procRegDeleteTreeW.Call(key, uintptr(unsafe.Pointer(subPtr)))
	return int32(r)
}

func RegDeleteKey(key HKEY, subKey string) int32 {
	var subPtr *uint16
	if subKey != "" {
		subPtr = UTF16Ptr(subKey)
	}
	r, _, _ := procRegDeleteKeyW.Call(key, uintptr(unsafe.Pointer(subPtr)))
	return int32(r)
}

func RegLoadAppKey(file string, access DWORD) (HKEY, int32) {
	filePtr := UTF16Ptr(file)
	var out HKEY
	r, _, _ := procRegLoadAppKeyW.Call(uintptr(unsafe.Pointer(filePtr)), uintptr(unsafe.Pointer(&out)), uintptr(access), 0, 0)
	return out, int32(r)
}

func GetConsoleMode(h HANDLE, mode *DWORD) bool {
	r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(mode)))
	return r != 0
}

func SetConsoleMode(h HANDLE, mode DWORD) bool {
	r, _, _ := procSetConsoleMode.Call(h, uintptr(mode))
	return r != 0
}

const (
	CTRL_C_EVENT     DWORD = 0
	CTRL_BREAK_EVENT DWORD = 1
)

func SetConsoleCtrlHandler(handler uintptr, add bool) bool {
	var a uintptr
	if add {
		a = 1
	}
	r, _, _ := procSetConsoleCtrlHandler.Call(handler, a)
	return r != 0
}

func ReadConsole(h HANDLE, buf []uint16, charsRead *DWORD) bool {
	var ptr uintptr
	if len(buf) > 0 {
		ptr = uintptr(unsafe.Pointer(&buf[0]))
	}
	r, _, _ := procReadConsoleW.Call(h, ptr, uintptr(len(buf)), uintptr(unsafe.Pointer(charsRead)), 0)
	return r != 0
}

// File locking
const (
	LOCKFILE_FAIL_IMMEDIATELY = 0x00000001
	LOCKFILE_EXCLUSIVE_LOCK   = 0x00000002
)

type Overlapped struct {
	Internal     uintptr
	InternalHigh uintptr
	Offset       uint32
	OffsetHigh   uint32
	HEvent       uintptr
}

func LockFileEx(h HANDLE, flags DWORD, reserved DWORD, numberOfBytesToLockLow DWORD, numberOfBytesToLockHigh DWORD, overlapped *Overlapped) error {
	r1, _, err := procLockFileEx.Call(
		h,
		uintptr(flags),
		uintptr(reserved),
		uintptr(numberOfBytesToLockLow),
		uintptr(numberOfBytesToLockHigh),
		uintptr(unsafe.Pointer(overlapped)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

func UnlockFileEx(h HANDLE, reserved DWORD, numberOfBytesToUnlockLow DWORD, numberOfBytesToUnlockHigh DWORD, overlapped *Overlapped) error {
	r1, _, err := procUnlockFileEx.Call(
		h,
		uintptr(reserved),
		uintptr(numberOfBytesToUnlockLow),
		uintptr(numberOfBytesToUnlockHigh),
		uintptr(unsafe.Pointer(overlapped)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

// CryptoAPI constants and functions
const (
	CERT_SYSTEM_STORE_LOCAL_MACHINE_ID     = 2
	CERT_SYSTEM_STORE_LOCAL_MACHINE_ENTERPRISE_ID = 7
	CERT_SYSTEM_STORE_LOCAL_MACHINE_GROUP_POLICY_ID = 8
	CERT_SYSTEM_STORE_CURRENT_USER_ID      = 1
	CERT_SYSTEM_STORE_CURRENT_USER_GROUP_POLICY_ID = 9
	CERT_SYSTEM_STORE_SERVICES_ID          = 5
	CERT_SYSTEM_STORE_USERS_ID             = 6
	CERT_SYSTEM_STORE_LOCATION_SHIFT       = 16
	CERT_FIND_PROP_ONLY_ENHKEY_USAGE_FLAG  = 0x4
	CERT_FIND_EXT_ONLY_ENHKEY_USAGE_FLAG   = 0x2
	CRYPT_E_NOT_FOUND                      = 0x80092004
	X509_ASN_ENCODING                      = 0x1
)

func CertSystemStore(location DWORD) DWORD {
	return location << CERT_SYSTEM_STORE_LOCATION_SHIFT
}

type CertContext struct {
	CertEncodingType DWORD
	EncodedCert      *byte
	LengthEncoded    DWORD
	CertInfo         uintptr
	CertStore        uintptr
}

type CertEnhKeyUsage struct {
	Count       DWORD
	Identifiers *uintptr // pointer to array of LPSTR
}

func CertOpenStore(storeProvider uintptr, msgAndCertEncodingType DWORD, cryptProv uintptr, flags DWORD, pvPara uintptr) (uintptr, error) {
	r1, _, err := procCertOpenStore.Call(
		storeProvider,
		uintptr(msgAndCertEncodingType),
		cryptProv,
		uintptr(flags),
		pvPara,
	)
	if r1 == 0 {
		return 0, err
	}
	return r1, nil
}

func CertCloseStore(certStore uintptr, flags DWORD) error {
	r1, _, err := procCertCloseStore.Call(certStore, uintptr(flags))
	if r1 == 0 {
		return err
	}
	return nil
}

func CertEnumCertificatesInStore(certStore uintptr, prevCertContext uintptr) uintptr {
	r1, _, _ := procCertEnumCertificatesInStore.Call(certStore, prevCertContext)
	return r1
}

func CertGetEnhancedKeyUsage(certContext uintptr, flags DWORD, usage uintptr, size *DWORD) bool {
	r1, _, _ := procCertGetEnhancedKeyUsage.Call(
		certContext,
		uintptr(flags),
		usage,
		uintptr(unsafe.Pointer(size)),
	)
	return r1 != 0
}

// Credentials
const (
	CRED_MAX_CREDENTIAL_BLOB_SIZE = 512
)

func CredRead(targetName string, credType DWORD) (*CREDENTIALW, error) {
	targetPtr := UTF16Ptr(targetName)
	var pCred uintptr
	r, _, err := procCredReadW.Call(
		uintptr(unsafe.Pointer(targetPtr)),
		uintptr(credType),
		0,
		uintptr(unsafe.Pointer(&pCred)),
	)
	if r == 0 {
		return nil, err
	}
	return (*CREDENTIALW)(unsafe.Pointer(pCred)), nil
}

func CredFree(p uintptr) {
	_, _, _ = procCredFree.Call(p)
}

func CredWrite(cred *CREDENTIALW, flags DWORD) error {
	r, _, err := procCredWriteW.Call(uintptr(unsafe.Pointer(cred)), uintptr(flags))
	if r == 0 {
		return err
	}
	return nil
}

func CredDelete(targetName string, credType DWORD, flags DWORD) error {
	targetPtr := UTF16Ptr(targetName)
	r, _, err := procCredDeleteW.Call(
		uintptr(unsafe.Pointer(targetPtr)),
		uintptr(credType),
		uintptr(flags),
	)
	if r == 0 {
		return err
	}
	return nil
}
