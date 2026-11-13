package winres

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Params contains information to embed in the Windows resource.
type Params struct {
	Version     string // e.g. "0.78.0"
	Description string // e.g. "zigsaw"
	FileName    string // e.g. "zigsaw.exe"
	ManifestXML []byte // raw XML bytes of manifest
}

// GenerateSYSO creates a COFF object file (.syso) for windows/amd64
// containing the RT_VERSION (16) and RT_MANIFEST (24) resources.
func GenerateSYSO(p Params) ([]byte, error) {
	versionBytes, err := buildVersionInfo(p)
	if err != nil {
		return nil, err
	}

	manifestBytes := p.ManifestXML
	if len(manifestBytes) == 0 {
		return nil, fmt.Errorf("manifest XML is required")
	}

	return buildCOFF(versionBytes, manifestBytes)
}

// WriteSYSO generates and writes a .syso file for the given parameters.
func WriteSYSO(path string, p Params) error {
	data, err := GenerateSYSO(p)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0666)
}

func parseVersion(v string) (major, minor, patch uint16, err error) {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("invalid semantic version: %q", v)
	}
	maj, err := strconv.ParseUint(parts[0], 10, 16)
	if err != nil {
		return 0, 0, 0, err
	}
	min, err := strconv.ParseUint(parts[1], 10, 16)
	if err != nil {
		return 0, 0, 0, err
	}
	pat, err := strconv.ParseUint(parts[2], 10, 16)
	if err != nil {
		return 0, 0, 0, err
	}
	return uint16(maj), uint16(min), uint16(pat), nil
}

func utf16Z(s string) []byte {
	u := utf16.Encode([]rune(s))
	u = append(u, 0)
	buf := make([]byte, len(u)*2)
	for i, v := range u {
		binary.LittleEndian.PutUint16(buf[i*2:], v)
	}
	return buf
}

func pad4(buf *bytes.Buffer) {
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}

func buildVersionInfo(p Params) ([]byte, error) {
	maj, min, pat, err := parseVersion(p.Version)
	if err != nil {
		return nil, err
	}

	// VS_FIXEDFILEINFO (52 bytes)
	var fixed [52]byte
	binary.LittleEndian.PutUint32(fixed[0:4], 0xFEEF04BD) // dwSignature
	binary.LittleEndian.PutUint32(fixed[4:8], 0x00010000) // dwStrucVersion
	// dwFileVersionMS: major.minor
	binary.LittleEndian.PutUint32(fixed[8:12], (uint32(maj)<<16)|uint32(min))
	// dwFileVersionLS: patch.0
	binary.LittleEndian.PutUint32(fixed[12:16], uint32(pat)<<16)
	// dwProductVersionMS: major.minor
	binary.LittleEndian.PutUint32(fixed[16:20], (uint32(maj)<<16)|uint32(min))
	// dwProductVersionLS: patch.0
	binary.LittleEndian.PutUint32(fixed[20:24], uint32(pat)<<16)
	binary.LittleEndian.PutUint32(fixed[24:28], 0x3F)       // dwFileFlagsMask
	binary.LittleEndian.PutUint32(fixed[28:32], 0)          // dwFileFlags
	binary.LittleEndian.PutUint32(fixed[32:36], 0x00040004) // dwFileOS = VOS_NT_WINDOWS32
	binary.LittleEndian.PutUint32(fixed[36:40], 1)          // dwFileType = VFT_APP
	binary.LittleEndian.PutUint32(fixed[40:44], 0)          // dwFileSubtype
	binary.LittleEndian.PutUint32(fixed[44:48], 0)          // dwFileDateMS
	binary.LittleEndian.PutUint32(fixed[48:52], 0)          // dwFileDateLS

	// StringFileInfo
	stringPairs := [][2]string{
		{"CompanyName", "Bradley Tran"},
		{"FileDescription", p.Description},
		{"FileVersion", p.Version},
		{"InternalName", p.FileName},
		{"LegalCopyright", "Copyright (c) 2026, Bradley Tran. BSD 3-Clause License."},
		{"OriginalFilename", p.FileName},
		{"ProductName", "zigsaw"},
		{"ProductVersion", p.Version},
	}

	var strTableBuf bytes.Buffer
	for _, pair := range stringPairs {
		k := pair[0]
		val := pair[1]
		valUTF16 := utf16Z(val)
		valCharCount := uint16(len(valUTF16) / 2) // length in 16-bit words including null

		var sBuf bytes.Buffer
		sBuf.Write([]byte{0, 0, 0, 0, 1, 0}) // placeholder length, valLength, wType=1
		sBuf.Write(utf16Z(k))
		pad4(&sBuf)
		sBuf.Write(valUTF16)

		sBytes := sBuf.Bytes()
		binary.LittleEndian.PutUint16(sBytes[0:2], uint16(len(sBytes)))
		binary.LittleEndian.PutUint16(sBytes[2:4], valCharCount)
		pad4(&strTableBuf)
		strTableBuf.Write(sBytes)
	}

	var strTableWrapper bytes.Buffer
	strTableWrapper.Write([]byte{0, 0, 0, 0, 1, 0}) // length, valLength=0, wType=1
	strTableWrapper.Write(utf16Z("040904b0"))
	pad4(&strTableWrapper)
	strTableWrapper.Write(strTableBuf.Bytes())
	strTableBytes := strTableWrapper.Bytes()
	binary.LittleEndian.PutUint16(strTableBytes[0:2], uint16(len(strTableBytes)))

	var sfiBuf bytes.Buffer
	sfiBuf.Write([]byte{0, 0, 0, 0, 1, 0})
	sfiBuf.Write(utf16Z("StringFileInfo"))
	pad4(&sfiBuf)
	sfiBuf.Write(strTableBytes)
	sfiBytes := sfiBuf.Bytes()
	binary.LittleEndian.PutUint16(sfiBytes[0:2], uint16(len(sfiBytes)))

	// VarFileInfo
	var vfiBuf bytes.Buffer
	vfiBuf.Write([]byte{0, 0, 0, 0, 1, 0})
	vfiBuf.Write(utf16Z("VarFileInfo"))
	pad4(&vfiBuf)

	var varBuf bytes.Buffer
	varBuf.Write([]byte{0, 0, 4, 0, 0, 0}) // length, valLen=4 bytes, wType=0 (binary)
	varBuf.Write(utf16Z("Translation"))
	pad4(&varBuf)
	varBuf.Write([]byte{0x09, 0x04, 0xb0, 0x04}) // 0x0409, 0x04b0
	varBytes := varBuf.Bytes()
	binary.LittleEndian.PutUint16(varBytes[0:2], uint16(len(varBytes)))

	vfiBuf.Write(varBytes)
	vfiBytes := vfiBuf.Bytes()
	binary.LittleEndian.PutUint16(vfiBytes[0:2], uint16(len(vfiBytes)))

	// VS_VERSIONINFO top-level
	var rootBuf bytes.Buffer
	rootBuf.Write([]byte{0, 0, 52, 0, 0, 0}) // length, valLen=52, wType=0
	rootBuf.Write(utf16Z("VS_VERSION_INFO"))
	pad4(&rootBuf)
	rootBuf.Write(fixed[:])
	pad4(&rootBuf)
	rootBuf.Write(sfiBytes)
	pad4(&rootBuf)
	rootBuf.Write(vfiBytes)

	rootBytes := rootBuf.Bytes()
	binary.LittleEndian.PutUint16(rootBytes[0:2], uint16(len(rootBytes)))
	return rootBytes, nil
}

// buildCOFF builds a COFF object file with a .rsrc section for x86_64 containing
// RT_VERSION (16) and RT_MANIFEST (24).
func buildCOFF(versionData, manifestData []byte) ([]byte, error) {
	// A standard 3-level resource directory:
	// Level 1: Root Directory (Types: 16 = Version, 24 = Manifest)
	// Level 2: Type Directories (ID: 1)
	// Level 3: ID Directories (Lang: 0x0409 = 1033)
	// Leaves: Data Entries (offset to data, size, codepage=0, reserved=0)

	// Resource IDs
	const (
		RT_VERSION  = 16
		RT_MANIFEST = 24
		LANG_EN_US  = 0x0409
	)

	// Directory header size = 16 bytes
	// Directory entry size = 8 bytes
	// Root dir: 2 entries (Type 16, Type 24 in ascending numerical order)
	// Size = 16 + 2*8 = 32 bytes

	// Type 16 dir: 1 entry (ID 1) -> 16 + 8 = 24 bytes
	// Type 24 dir: 1 entry (ID 1) -> 16 + 8 = 24 bytes

	// ID 1 dir (Version): 1 entry (Lang 0x0409) -> 16 + 8 = 24 bytes
	// ID 1 dir (Manifest): 1 entry (Lang 0x0409) -> 16 + 8 = 24 bytes

	// Total directory tree size: 32 + 24 + 24 + 24 + 24 = 128 bytes.
	// Data entry size = 16 bytes.
	// 2 Data Entries -> 32 bytes.
	// Total metadata size = 128 + 32 = 160 bytes.

	offsetRoot := uint32(0)
	offsetTypeVersion := offsetRoot + 16 + 2*8 // 32
	offsetTypeManifest := offsetTypeVersion + 16 + 8 // 56
	offsetIDVersion := offsetTypeManifest + 16 + 8   // 80
	offsetIDManifest := offsetIDVersion + 16 + 8     // 104
	offsetDataEntryVersion := offsetIDManifest + 16 + 8 // 128
	offsetDataEntryManifest := offsetDataEntryVersion + 16 // 144
	offsetRawData := offsetDataEntryManifest + 16 // 160

	// Raw data alignment to 8 bytes
	rawVersionOffset := offsetRawData
	rawVersionSize := uint32(len(versionData))
	rawManifestOffset := (rawVersionOffset + rawVersionSize + 7) &^ 7
	rawManifestSize := uint32(len(manifestData))
	totalSectionSize := (rawManifestOffset + rawManifestSize + 7) &^ 7

	rsrcBuf := make([]byte, totalSectionSize)

	// Write Root Dir (0)
	// 2 ID entries (16, 24)
	binary.LittleEndian.PutUint16(rsrcBuf[offsetRoot+14:], 2)
	// Entry 0: Type 16
	binary.LittleEndian.PutUint32(rsrcBuf[offsetRoot+16:], RT_VERSION)
	binary.LittleEndian.PutUint32(rsrcBuf[offsetRoot+20:], 0x80000000|offsetTypeVersion)
	// Entry 1: Type 24
	binary.LittleEndian.PutUint32(rsrcBuf[offsetRoot+24:], RT_MANIFEST)
	binary.LittleEndian.PutUint32(rsrcBuf[offsetRoot+28:], 0x80000000|offsetTypeManifest)

	// Write Type Version Dir (offsetTypeVersion)
	binary.LittleEndian.PutUint16(rsrcBuf[offsetTypeVersion+14:], 1)
	binary.LittleEndian.PutUint32(rsrcBuf[offsetTypeVersion+16:], 1) // ID 1
	binary.LittleEndian.PutUint32(rsrcBuf[offsetTypeVersion+20:], 0x80000000|offsetIDVersion)

	// Write Type Manifest Dir (offsetTypeManifest)
	binary.LittleEndian.PutUint16(rsrcBuf[offsetTypeManifest+14:], 1)
	binary.LittleEndian.PutUint32(rsrcBuf[offsetTypeManifest+16:], 1) // ID 1
	binary.LittleEndian.PutUint32(rsrcBuf[offsetTypeManifest+20:], 0x80000000|offsetIDManifest)

	// Write ID Version Dir (offsetIDVersion)
	binary.LittleEndian.PutUint16(rsrcBuf[offsetIDVersion+14:], 1)
	binary.LittleEndian.PutUint32(rsrcBuf[offsetIDVersion+16:], LANG_EN_US)
	binary.LittleEndian.PutUint32(rsrcBuf[offsetIDVersion+20:], offsetDataEntryVersion)

	// Write ID Manifest Dir (offsetIDManifest)
	binary.LittleEndian.PutUint16(rsrcBuf[offsetIDManifest+14:], 1)
	binary.LittleEndian.PutUint32(rsrcBuf[offsetIDManifest+16:], LANG_EN_US)
	binary.LittleEndian.PutUint32(rsrcBuf[offsetIDManifest+20:], offsetDataEntryManifest)

	// Data Entries
	// Version Data Entry: OffsetToData (will be relocated), Size, CodePage=0, Reserved=0
	binary.LittleEndian.PutUint32(rsrcBuf[offsetDataEntryVersion:], rawVersionOffset)
	binary.LittleEndian.PutUint32(rsrcBuf[offsetDataEntryVersion+4:], rawVersionSize)

	// Manifest Data Entry: OffsetToData (will be relocated), Size, CodePage=0, Reserved=0
	binary.LittleEndian.PutUint32(rsrcBuf[offsetDataEntryManifest:], rawManifestOffset)
	binary.LittleEndian.PutUint32(rsrcBuf[offsetDataEntryManifest+4:], rawManifestSize)

	// Copy Raw Data
	copy(rsrcBuf[rawVersionOffset:], versionData)
	copy(rsrcBuf[rawManifestOffset:], manifestData)

	// Now build COFF Object File
	// COFF Header (20 bytes)
	// 1 Section Header (40 bytes)
	// Section Data (totalSectionSize)
	// Relocations: 2 relocations (10 bytes each)
	// Symbol Table: 1 symbol (18 bytes)

	var coff bytes.Buffer

	// File Header
	const IMAGE_FILE_MACHINE_AMD64 = 0x8664
	hdr := make([]byte, 20)
	binary.LittleEndian.PutUint16(hdr[0:2], IMAGE_FILE_MACHINE_AMD64)
	binary.LittleEndian.PutUint16(hdr[2:4], 1) // 1 section (.rsrc)
	binary.LittleEndian.PutUint32(hdr[4:8], 0) // TimeDateStamp = 0 (deterministic)

	offsetSectionData := uint32(20 + 40)
	offsetRelocs := offsetSectionData + totalSectionSize
	offsetSymbols := offsetRelocs + 2*10

	binary.LittleEndian.PutUint32(hdr[8:12], offsetSymbols) // PointerToSymbolTable
	binary.LittleEndian.PutUint32(hdr[12:16], 1)            // NumberOfSymbols
	binary.LittleEndian.PutUint16(hdr[16:18], 0)            // SizeOfOptionalHeader
	binary.LittleEndian.PutUint16(hdr[18:20], 0)            // Characteristics
	coff.Write(hdr)

	// Section Header (40 bytes)
	sec := make([]byte, 40)
	copy(sec[0:8], []byte(".rsrc\x00\x00\x00"))
	binary.LittleEndian.PutUint32(sec[8:12], 0)                // VirtualSize
	binary.LittleEndian.PutUint32(sec[12:16], 0)               // VirtualAddress
	binary.LittleEndian.PutUint32(sec[16:20], totalSectionSize) // SizeOfRawData
	binary.LittleEndian.PutUint32(sec[20:24], offsetSectionData) // PointerToRawData
	binary.LittleEndian.PutUint32(sec[24:28], offsetRelocs)      // PointerToRelocations
	binary.LittleEndian.PutUint32(sec[28:32], 0)                // PointerToLinenumbers
	binary.LittleEndian.PutUint16(sec[32:34], 2)                // NumberOfRelocations
	binary.LittleEndian.PutUint16(sec[34:36], 0)                // NumberOfLinenumbers
	// IMAGE_SCN_CNT_INITIALIZED_DATA | IMAGE_SCN_MEM_READ | IMAGE_SCN_MEM_WRITE
	binary.LittleEndian.PutUint32(sec[36:40], 0x40000040)
	coff.Write(sec)

	// Write Section Data
	coff.Write(rsrcBuf)

	// Write Relocations (2 relocations for AMD64)
	// IMAGE_REL_AMD64_ADDR32NB = 3
	// VirtualAddress uint32, SymbolTableIndex uint32, Type uint16
	reloc1 := make([]byte, 10)
	binary.LittleEndian.PutUint32(reloc1[0:4], offsetDataEntryVersion) // rva of Version Data Entry's OffsetToData
	binary.LittleEndian.PutUint32(reloc1[4:8], 0)                      // Symbol 0 (.rsrc)
	binary.LittleEndian.PutUint16(reloc1[8:10], 3)                      // IMAGE_REL_AMD64_ADDR32NB
	coff.Write(reloc1)

	reloc2 := make([]byte, 10)
	binary.LittleEndian.PutUint32(reloc2[0:4], offsetDataEntryManifest) // rva of Manifest Data Entry's OffsetToData
	binary.LittleEndian.PutUint32(reloc2[4:8], 0)                       // Symbol 0 (.rsrc)
	binary.LittleEndian.PutUint16(reloc2[8:10], 3)                       // IMAGE_REL_AMD64_ADDR32NB
	coff.Write(reloc2)

	// Symbol Table (1 symbol: 18 bytes)
	// Name (8 bytes): .rsrc\0\0\0
	// Value (4 bytes): 0
	// SectionNumber (2 bytes): 1
	// Type (2 bytes): 0
	// StorageClass (1 byte): 3 (IMAGE_SYM_CLASS_STATIC)
	// NumberOfAuxSymbols (1 byte): 0
	sym := make([]byte, 18)
	copy(sym[0:8], []byte(".rsrc\x00\x00\x00"))
	binary.LittleEndian.PutUint32(sym[8:12], 0)
	binary.LittleEndian.PutUint16(sym[12:14], 1)
	binary.LittleEndian.PutUint16(sym[14:16], 0)
	sym[16] = 3 // IMAGE_SYM_CLASS_STATIC
	sym[17] = 0 // No aux symbols
	coff.Write(sym)

	// String table: minimum size is 4 bytes holding uint32(4)
	coff.Write([]byte{4, 0, 0, 0})

	return coff.Bytes(), nil
}
