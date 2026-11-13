package pe

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

type Subsystem int

const (
	SubsystemUnknown Subsystem = 0
	SubsystemGUI     Subsystem = 2 // IMAGE_SUBSYSTEM_WINDOWS_GUI
	SubsystemConsole Subsystem = 3 // IMAGE_SUBSYSTEM_WINDOWS_CUI
)

// ntLen is the PE signature (4), the file header (20), and the optional header
// up to and including Subsystem (68 bytes in + 2 bytes for subsystem field = 70 bytes).
const ntLen = 4 + 20 + 70

// SubsystemOfBytes reads the subsystem from the initial bytes of a PE file.
// Returns (subsystem, true) if valid GUI or Console, or (SubsystemUnknown, false) otherwise.
func SubsystemOfBytes(bytes []byte) (Subsystem, bool) {
	if len(bytes) < 64 {
		return SubsystemUnknown, false
	}
	offset, ok := ntHeaderOffset(bytes[:64])
	if !ok {
		return SubsystemUnknown, false
	}
	if int(offset) > len(bytes) || len(bytes)-int(offset) < ntLen {
		return SubsystemUnknown, false
	}
	return fromNtHeaders(bytes[offset : offset+ntLen])
}

// SubsystemOfFile reads the subsystem of the executable at path.
// Returns (subsystem, true, nil) if valid GUI or Console, (SubsystemUnknown, false, nil)
// if not a PE or not GUI/Console, or an error if opening or reading fails with an I/O error.
func SubsystemOfFile(path string) (Subsystem, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return SubsystemUnknown, false, err
	}
	defer f.Close()

	var dos [64]byte
	if _, err := io.ReadFull(f, dos[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return SubsystemUnknown, false, nil
		}
		return SubsystemUnknown, false, err
	}
	offset, ok := ntHeaderOffset(dos[:])
	if !ok {
		return SubsystemUnknown, false, nil
	}

	nt := make([]byte, ntLen)
	if _, err := f.ReadAt(nt, int64(offset)); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return SubsystemUnknown, false, nil
		}
		return SubsystemUnknown, false, err
	}
	sub, ok := fromNtHeaders(nt)
	return sub, ok, nil
}

func ntHeaderOffset(dos []byte) (uint32, bool) {
	if len(dos) < 0x40 || dos[0] != 'M' || dos[1] != 'Z' {
		return 0, false
	}
	return binary.LittleEndian.Uint32(dos[0x3c:0x40]), true
}

func fromNtHeaders(nt []byte) (Subsystem, bool) {
	if len(nt) < ntLen || nt[0] != 'P' || nt[1] != 'E' || nt[2] != 0 || nt[3] != 0 {
		return SubsystemUnknown, false
	}
	subVal := binary.LittleEndian.Uint16(nt[4+20+68 : 4+20+70])
	switch subVal {
	case 2:
		return SubsystemGUI, true
	case 3:
		return SubsystemConsole, true
	default:
		return SubsystemUnknown, false
	}
}
