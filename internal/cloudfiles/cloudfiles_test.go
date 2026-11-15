package cloudfiles

import (
	"testing"
	"unsafe"
)

func TestStructSizesAndOffsets(t *testing.T) {
	if sz := unsafe.Sizeof(FsMetadata{}); sz != 48 {
		t.Errorf("sizeof(FsMetadata) = %d, want 48", sz)
	}
	if sz := unsafe.Sizeof(PlaceholderCreateInfo{}); sz != 88 {
		t.Errorf("sizeof(PlaceholderCreateInfo) = %d, want 88", sz)
	}
	if off := unsafe.Offsetof(CallbackInfo{}.RequestKey); off != 144 {
		t.Errorf("offsetof(CallbackInfo.RequestKey) = %d, want 144", off)
	}
	if off := unsafe.Offsetof(FetchDataParams{}.RequiredOffset); off != 16 {
		t.Errorf("offsetof(FetchDataParams.RequiredOffset) = %d, want 16", off)
	}
	if off := unsafe.Offsetof(TransferParams{}.CompletionStatus); off != 12 {
		t.Errorf("offsetof(TransferParams.CompletionStatus) = %d, want 12", off)
	}
	if sz := unsafe.Sizeof(TransferParams{}); sz != 40 {
		t.Errorf("sizeof(TransferParams) = %d, want 40", sz)
	}
}

func asInt32(u uint32) int32 {
	return int32(u)
}

func TestWin32CodeAndMeansUnsupported(t *testing.T) {
	code, ok := Win32Code(asInt32(0x800701de))
	if !ok || code != 478 {
		t.Errorf("Win32Code(0x800701de) = (%d, %v), want (478, true)", code, ok)
	}

	_, ok2 := Win32Code(asInt32(0xC000CF0E))
	if ok2 {
		t.Errorf("expected false for NTSTATUS 0xC000CF0E")
	}

	if !MeansUnsupported(asInt32(0x800701de)) { // Dev Drive (478)
		t.Errorf("expected 478 to mean unsupported")
	}
	if !MeansUnsupported(asInt32(0x80070032)) { // ERROR_NOT_SUPPORTED (50)
		t.Errorf("expected 50 to mean unsupported")
	}
	if MeansUnsupported(asInt32(0x80070005)) { // Access denied (5)
		t.Errorf("expected 5 not to mean unsupported")
	}
	if MeansUnsupported(asInt32(0x8007017a)) { // 378 (already connected)
		t.Errorf("expected 378 not to mean unsupported")
	}
	if MeansUnsupported(0) {
		t.Errorf("expected 0 not to mean unsupported")
	}
}
