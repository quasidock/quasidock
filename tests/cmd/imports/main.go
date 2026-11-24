package main

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/quasidock/quasidock/internal/win32"
)

type Counts struct {
	Bound   int
	Unbound int
}

type Section struct {
	VA     uint32
	Size   uint32
	Offset uint32
}

func count(data []byte) (Counts, error) {
	if len(data) < 64 || string(data[:2]) != "MZ" {
		return Counts{}, fmt.Errorf("not PE")
	}
	nt := binary.LittleEndian.Uint32(data[0x3c:0x40])
	if int(nt)+24 > len(data) || string(data[nt:nt+4]) != "PE\x00\x00" {
		return Counts{}, fmt.Errorf("not PE")
	}
	fileHeader := nt + 4
	sectionCount := binary.LittleEndian.Uint16(data[fileHeader+2 : fileHeader+4])
	optionalSize := binary.LittleEndian.Uint16(data[fileHeader+16 : fileHeader+18])
	optional := fileHeader + 20
	sectionsAt := optional + uint32(optionalSize)
	if int(sectionsAt)+int(sectionCount)*40 > len(data) {
		return Counts{}, fmt.Errorf("truncated")
	}
	magic := binary.LittleEndian.Uint16(data[optional : optional+2])
	var directories uint32
	var thunk uint32
	switch magic {
	case 0x20b:
		directories = optional + 112
		thunk = 8
	case 0x10b:
		directories = optional + 96
		thunk = 4
	default:
		return Counts{}, fmt.Errorf("not PE")
	}

	importRVA := binary.LittleEndian.Uint32(data[directories+8 : directories+12])
	iatRVA := binary.LittleEndian.Uint32(data[directories+12*8 : directories+12*8+4])
	iatSize := binary.LittleEndian.Uint32(data[directories+12*8+4 : directories+12*8+8])

	if importRVA == 0 {
		return Counts{Bound: 0, Unbound: 0}, nil
	}
	if iatRVA == 0 {
		return Counts{}, fmt.Errorf("no IAT")
	}

	sections := make([]Section, sectionCount)
	for i := uint32(0); i < uint32(sectionCount); i++ {
		s := data[sectionsAt+i*40 : sectionsAt+(i+1)*40]
		vSize := binary.LittleEndian.Uint32(s[8:12])
		rSize := binary.LittleEndian.Uint32(s[16:20])
		size := vSize
		if rSize > size {
			size = rSize
		}
		va := binary.LittleEndian.Uint32(s[12:16])
		offset := binary.LittleEndian.Uint32(s[20:24])
		sections[i] = Section{VA: va, Size: size, Offset: offset}
	}

	offsetOf := func(rva uint32) (uint32, error) {
		for _, s := range sections {
			if rva >= s.VA && rva < s.VA+s.Size {
				return s.Offset + (rva - s.VA), nil
			}
		}
		return 0, fmt.Errorf("RVA outside sections: 0x%x", rva)
	}

	slotVal := func(rva uint32) (uint64, error) {
		off, err := offsetOf(rva)
		if err != nil {
			return 0, err
		}
		if thunk == 8 {
			if int(off)+8 > len(data) {
				return 0, fmt.Errorf("truncated")
			}
			return binary.LittleEndian.Uint64(data[off : off+8]), nil
		}
		if int(off)+4 > len(data) {
			return 0, fmt.Errorf("truncated")
		}
		return uint64(binary.LittleEndian.Uint32(data[off : off+4])), nil
	}

	numSlots := int(iatSize / thunk)
	reached := make([]bool, numSlots)
	bound := 0

	d, err := offsetOf(importRVA)
	if err != nil {
		return Counts{}, err
	}

	for int(d)+20 <= len(data) {
		desc := data[d : d+20]
		firstThunk := binary.LittleEndian.Uint32(desc[16:20])
		name := binary.LittleEndian.Uint32(desc[12:16])
		if firstThunk == 0 && name == 0 {
			break
		}
		rva := firstThunk
		for {
			v, err := slotVal(rva)
			if err != nil || v == 0 {
				break
			}
			bound++
			if rva >= iatRVA && rva < iatRVA+iatSize {
				reached[(rva-iatRVA)/thunk] = true
			}
			rva += thunk
		}
		d += 20
	}

	unbound := 0
	for i, r := range reached {
		if !r {
			v, _ := slotVal(iatRVA + uint32(i)*thunk)
			if v != 0 {
				unbound++
			}
		}
	}

	return Counts{Bound: bound, Unbound: unbound}, nil
}

func main() {
	args := os.Args[1:]
	if len(args) < 1 {
		fmt.Println("usage: quasidock-imports FILE...")
		win32.ExitProcess(2)
	}

	failed := false
	for _, path := range args {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("%s: %v\n", path, err)
			failed = true
			continue
		}
		if len(data) < 2 || string(data[:2]) != "MZ" {
			fmt.Printf("%s: not a PE file, skipped\n", path)
			continue
		}
		c, err := count(data)
		if err != nil {
			fmt.Printf("%s: not a PE file with imports we can read (%v)\n", path, err)
			failed = true
			continue
		}
		fmt.Printf("%s: %d imports, %d unbound\n", path, c.Bound, c.Unbound)
		if c.Unbound > 0 {
			failed = true
		}
	}

	if failed {
		win32.ExitProcess(1)
	}
	win32.ExitProcess(0)
}
