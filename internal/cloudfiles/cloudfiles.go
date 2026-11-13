package cloudfiles

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"github.com/quasidock/quasidock/internal/win32"
)

var (
	ErrUnsupported = errors.New("cloud files sync root unsupported on this system/volume")
)

const (
	CF_CALLBACK_TYPE_FETCH_DATA        = 0
	CF_CALLBACK_TYPE_CANCEL_FETCH_DATA = 2
	CF_CALLBACK_TYPE_NONE              = 0xffffffff

	CF_OPERATION_TYPE_TRANSFER_DATA = 0

	CF_HYDRATION_POLICY_PROGRESSIVE = 1
	CF_POPULATION_POLICY_ALWAYS_FULL = 3

	CF_REGISTER_FLAG_UPDATE                                 = 1
	CF_REGISTER_FLAG_DISABLE_ON_DEMAND_POPULATION_ON_ROOT = 2
	CF_REGISTER_FLAG_MARK_IN_SYNC_ON_ROOT                  = 4

	CF_CONNECT_FLAG_REQUIRE_PROCESS_INFO          = 2
	CF_CONNECT_FLAG_REQUIRE_FULL_FILE_PATH        = 4
	CF_CONNECT_FLAG_BLOCK_SELF_IMPLICIT_HYDRATION = 8

	CF_PLACEHOLDER_CREATE_FLAG_DISABLE_ON_DEMAND_POPULATION = 1
	CF_PLACEHOLDER_CREATE_FLAG_MARK_IN_SYNC                 = 2

	CF_SYNC_ROOT_INFO_BASIC = 0

	FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS = 0x400000
	FILE_ATTRIBUTE_REPARSE_POINT         = 0x400

	StatusOk               int32 = 0
	StatusValidationFailed int32 = -1073688818 // int32(0xC000CF0E)
	StatusUnsuccessful     int32 = -1073688814 // int32(0xC000CF12)
)

type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var ProviderID = GUID{
	Data1: 0x5b0e7f1d,
	Data2: 0x9c42,
	Data3: 0x4a77,
	Data4: [8]byte{0x8e, 0x15, 0x2f, 0x63, 0xd0, 0x9a, 0x41, 0xc7},
}

type FsMetadata struct {
	Creation   int64
	Access     int64
	Write      int64
	Change     int64
	Attributes uint32
	_          uint32 // padding for 8-byte alignment of Size
	Size       int64
}

type PlaceholderCreateInfo struct {
	RelativeName *uint16
	Metadata     FsMetadata
	Identity     uintptr
	IdentityLen  uint32
	Flags        uint32
	Result       int32 // HRESULT
	_            uint32
	Usn          int64
}

type ProcessInfo struct {
	StructSize    uint32
	PID           uint32
	ImagePath     *uint16
	PackageName   *uint16
	ApplicationID *uint16
	CommandLine   *uint16
	SessionID     uint32
	_             uint32
}

type SyncRegistration struct {
	StructSize      uint32
	_               uint32
	ProviderName    *uint16
	ProviderVersion *uint16
	SyncRootID      uintptr
	SyncRootIDLen   uint32
	FileIdentity    uintptr
	FileIdentityLen uint32
	ProviderID      GUID
}

type SyncPolicies struct {
	StructSize            uint32
	HydrationPrimary      uint16
	HydrationModifier     uint16
	PopulationPrimary     uint16
	PopulationModifier    uint16
	InSync                uint32
	HardLink              uint32
	PlaceholderManagement uint32
}

type CallbackInfo struct {
	StructSize        uint32
	_                 uint32
	ConnectionKey     int64
	Context           uintptr
	VolumeGuidName    *uint16
	VolumeDosName     *uint16
	VolumeSerial      uint32
	_                 uint32
	RootFileID        int64
	RootIdentity      uintptr
	RootIdentityLen   uint32
	_                 uint32
	FileID            int64
	FileSize          int64
	FileIdentity      uintptr
	FileIdentityLen   uint32
	_                 uint32
	NormalizedPath    *uint16
	TransferKey       int64
	PriorityHint      uint8
	_                 [7]byte
	CorrelationVector uintptr
	ProcessInfo       *ProcessInfo
	RequestKey        int64
}

type FetchDataParams struct {
	ParamSize           uint32
	Flags               uint32
	RequiredOffset      int64
	RequiredLength      int64
	OptionalOffset      int64
	OptionalLength      int64
	LastDehydrationTime int64
	DehydrationReason   uint32
	_                   uint32
}

type CancelParams struct {
	ParamSize uint32
	Flags     uint32
	Offset    int64
	Length    int64
}

type CallbackRegistration struct {
	Type     uint32
	_        uint32
	Callback uintptr
}

type OperationInfo struct {
	StructSize        uint32
	Type              uint32
	ConnectionKey     int64
	TransferKey       int64
	CorrelationVector uintptr
	SyncStatus        uintptr
	RequestKey        int64
}

type TransferParams struct {
	ParamSize        uint32
	Flags            uint32
	CompletionStatus int32
	_                uint32
	Buffer           uintptr
	Offset           int64
	Length           int64
}

type cldAPI struct {
	cfRegisterSyncRoot       uintptr
	cfUnregisterSyncRoot     uintptr
	cfConnectSyncRoot        uintptr
	cfDisconnectSyncRoot     uintptr
	cfExecute                uintptr
	cfCreatePlaceholders     uintptr
	cfReportProviderProgress uintptr
	cfGetSyncRootInfoByPath  uintptr
	cfHydratePlaceholder     uintptr
}

var (
	apiOnce sync.Once
	apiInst *cldAPI
)

func getAPI() *cldAPI {
	apiOnce.Do(func() {
		dll, err := syscall.LoadLibrary("cldapi.dll")
		if err != nil {
			return
		}
		find := func(name string) uintptr {
			p, err := syscall.GetProcAddress(dll, name)
			if err != nil {
				return 0
			}
			return p
		}

		inst := &cldAPI{
			cfRegisterSyncRoot:       find("CfRegisterSyncRoot"),
			cfUnregisterSyncRoot:     find("CfUnregisterSyncRoot"),
			cfConnectSyncRoot:        find("CfConnectSyncRoot"),
			cfDisconnectSyncRoot:     find("CfDisconnectSyncRoot"),
			cfExecute:                find("CfExecute"),
			cfCreatePlaceholders:     find("CfCreatePlaceholders"),
			cfReportProviderProgress: find("CfReportProviderProgress"),
			cfGetSyncRootInfoByPath:  find("CfGetSyncRootInfoByPath"),
			cfHydratePlaceholder:     find("CfHydratePlaceholder"),
		}
		if inst.cfRegisterSyncRoot != 0 && inst.cfCreatePlaceholders != 0 {
			apiInst = inst
		}
	})
	return apiInst
}

func Win32Code(hr int32) (uint16, bool) {
	bits := uint32(hr)
	if bits&0xFFFF0000 != 0x80070000 {
		return 0, false
	}
	return uint16(bits & 0xFFFF), true
}

func MeansUnsupported(hr int32) bool {
	code, ok := Win32Code(hr)
	if !ok {
		return false
	}
	switch code {
	case 1, // ERROR_INVALID_FUNCTION
		50,  // ERROR_NOT_SUPPORTED
		379, // ERROR_CLOUD_FILE_NOT_SUPPORTED
		381, // ERROR_CLOUD_FILE_READ_ONLY_VOLUME
		478: // filter cannot attach (Dev Drive)
		return true
	default:
		return false
	}
}

func check(what, path string, hr int32) error {
	if hr >= 0 {
		return nil
	}
	if MeansUnsupported(hr) {
		return ErrUnsupported
	}
	if code, ok := Win32Code(hr); ok {
		return fmt.Errorf("%s %s: Windows error %d (0x%08X)", what, path, code, uint32(hr))
	}
	return fmt.Errorf("%s %s: 0x%08X", what, path, uint32(hr))
}

func Register(root string, identity string, again bool) error {
	api := getAPI()
	if api == nil {
		return ErrUnsupported
	}

	rootW := win32.UTF16Ptr(root)
	nameW := win32.UTF16Ptr("quasidock")
	verW := win32.UTF16Ptr("1")

	idBytes := []byte(identity)
	var idPtr uintptr
	if len(idBytes) > 0 {
		idPtr = uintptr(unsafe.Pointer(&idBytes[0]))
	}

	reg := SyncRegistration{
		StructSize:      uint32(unsafe.Sizeof(SyncRegistration{})),
		ProviderName:    nameW,
		ProviderVersion: verW,
		SyncRootID:      idPtr,
		SyncRootIDLen:   uint32(len(idBytes)),
		FileIdentity:    idPtr,
		FileIdentityLen: uint32(len(idBytes)),
		ProviderID:      ProviderID,
	}

	policies := SyncPolicies{
		StructSize:        uint32(unsafe.Sizeof(SyncPolicies{})),
		HydrationPrimary:  CF_HYDRATION_POLICY_PROGRESSIVE,
		PopulationPrimary: CF_POPULATION_POLICY_ALWAYS_FULL,
	}

	flags := uint32(CF_REGISTER_FLAG_DISABLE_ON_DEMAND_POPULATION_ON_ROOT | CF_REGISTER_FLAG_MARK_IN_SYNC_ON_ROOT)
	if again {
		flags |= CF_REGISTER_FLAG_UPDATE
	}

	r, _, _ := syscall.SyscallN(
		api.cfRegisterSyncRoot,
		uintptr(unsafe.Pointer(rootW)),
		uintptr(unsafe.Pointer(&reg)),
		uintptr(unsafe.Pointer(&policies)),
		uintptr(flags),
	)
	return check("registering", root, int32(r))
}

func Unregister(root string) error {
	api := getAPI()
	if api == nil {
		return nil
	}

	rootW := win32.UTF16Ptr(root)
	r, _, _ := syscall.SyscallN(
		api.cfUnregisterSyncRoot,
		uintptr(unsafe.Pointer(rootW)),
	)
	hr := int32(r)
	if code, ok := Win32Code(hr); ok {
		switch code {
		case 2, 3, 405: // not found, or not a cloud sync root
			return nil
		}
	}
	return check("unregistering", root, hr)
}

func IsSyncRoot(path string) bool {
	api := getAPI()
	if api == nil {
		return false
	}
	pathW := win32.UTF16Ptr(path)
	var info [64]byte
	var length uint32
	r, _, _ := syscall.SyscallN(
		api.cfGetSyncRootInfoByPath,
		uintptr(unsafe.Pointer(pathW)),
		CF_SYNC_ROOT_INFO_BASIC,
		uintptr(unsafe.Pointer(&info[0])),
		uintptr(len(info)),
		uintptr(unsafe.Pointer(&length)),
	)
	return int32(r) >= 0
}

func Hydrate(path string) (int32, error) {
	api := getAPI()
	if api == nil {
		return 0, ErrUnsupported
	}
	const FILE_READ_ATTRIBUTES = 0x80
	h, err := win32.CreateFile(
		path,
		FILE_READ_ATTRIBUTES,
		win32.FILE_SHARE_READ|win32.FILE_SHARE_WRITE|win32.FILE_SHARE_DELETE,
		nil,
		win32.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		errno := win32.GetLastError()
		return int32(0x80070000 | uint32(errno)), nil
	}
	defer win32.CloseHandle(h)

	r, _, _ := syscall.SyscallN(
		api.cfHydratePlaceholder,
		uintptr(h),
		0,
		uintptr(0xffffffffffffffff), // -1
		0,
		0,
	)
	return int32(r), nil
}

type Connection struct {
	Key int64
}

func (c Connection) Disconnect() {
	if api := getAPI(); api != nil && api.cfDisconnectSyncRoot != 0 {
		_, _, _ = syscall.SyscallN(api.cfDisconnectSyncRoot, uintptr(c.Key))
	}
}

type Placeholder struct {
	Name     string
	Dir      bool
	Size     uint64
	Identity uint32
}

func CreatePlaceholders(dir string, children []Placeholder) (int, error) {
	api := getAPI()
	if api == nil {
		return 0, ErrUnsupported
	}
	if len(children) == 0 {
		return 0, nil
	}

	dirW := win32.UTF16Ptr(dir)
	infos := make([]PlaceholderCreateInfo, len(children))
	identities := make([]uint32, len(children))

	var now int64
	win32.GetSystemTimeAsFileTime(&now)

	for i, child := range children {
		identities[i] = child.Identity
		attr := uint32(0x80) // FILE_ATTRIBUTE_NORMAL
		if child.Dir {
			attr = win32.FILE_ATTRIBUTE_DIRECTORY
		}

		flags := uint32(CF_PLACEHOLDER_CREATE_FLAG_MARK_IN_SYNC)
		if child.Dir {
			flags |= CF_PLACEHOLDER_CREATE_FLAG_DISABLE_ON_DEMAND_POPULATION
		}

		infos[i] = PlaceholderCreateInfo{
			RelativeName: win32.UTF16Ptr(child.Name),
			Metadata: FsMetadata{
				Creation:   now,
				Access:     now,
				Write:      now,
				Change:     now,
				Attributes: attr,
				Size:       int64(child.Size),
			},
			Identity:    uintptr(unsafe.Pointer(&identities[i])),
			IdentityLen: uint32(unsafe.Sizeof(uint32(0))),
			Flags:       flags,
		}
	}

	var done uint32
	r, _, _ := syscall.SyscallN(
		api.cfCreatePlaceholders,
		uintptr(unsafe.Pointer(dirW)),
		uintptr(unsafe.Pointer(&infos[0])),
		uintptr(len(infos)),
		0,
		uintptr(unsafe.Pointer(&done)),
	)
	if err := check("making placeholders in", dir, int32(r)); err != nil {
		return 0, err
	}

	for _, info := range infos {
		if err := check("making a placeholder in", dir, info.Result); err != nil {
			return 0, err
		}
	}

	return int(done), nil
}

func IdentityOf(info *CallbackInfo) *uint32 {
	if info.FileIdentityLen != uint32(unsafe.Sizeof(uint32(0))) || info.FileIdentity == 0 {
		return nil
	}
	return (*uint32)(unsafe.Pointer(info.FileIdentity))
}

func Transfer(info *CallbackInfo, status int32, data []byte, offset, length uint64) int32 {
	api := getAPI()
	if api == nil {
		return -2147467259 // 0x80004005 (E_FAIL)
	}

	op := OperationInfo{
		StructSize:    uint32(unsafe.Sizeof(OperationInfo{})),
		Type:          CF_OPERATION_TYPE_TRANSFER_DATA,
		ConnectionKey: info.ConnectionKey,
		TransferKey:   info.TransferKey,
		RequestKey:    info.RequestKey,
	}

	var bufPtr uintptr
	if len(data) > 0 {
		bufPtr = uintptr(unsafe.Pointer(&data[0]))
	}

	params := TransferParams{
		ParamSize:        uint32(unsafe.Sizeof(TransferParams{})),
		CompletionStatus: status,
		Buffer:           bufPtr,
		Offset:           int64(offset),
		Length:           int64(length),
	}

	r, _, _ := syscall.SyscallN(
		api.cfExecute,
		uintptr(unsafe.Pointer(&op)),
		uintptr(unsafe.Pointer(&params)),
	)
	return int32(r)
}

func ReportProgress(info *CallbackInfo, total, completed uint64) {
	if api := getAPI(); api != nil && api.cfReportProviderProgress != 0 {
		_, _, _ = syscall.SyscallN(
			api.cfReportProviderProgress,
			uintptr(info.ConnectionKey),
			uintptr(info.TransferKey),
			uintptr(total),
			uintptr(completed),
		)
	}
}

func ProcessImage(info *CallbackInfo) string {
	if info.ProcessInfo == nil || info.ProcessInfo.ImagePath == nil {
		return "?"
	}
	return win32.UTF16PtrToString(info.ProcessInfo.ImagePath)
}
