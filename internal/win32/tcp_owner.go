package win32

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sync"
	"syscall"
	"unsafe"
)

var getExtendedTCPTable = sync.OnceValues(func() (*syscall.LazyProc, error) {
	path, err := SystemProgram("iphlpapi.dll")
	if err != nil {
		return nil, err
	}
	proc := syscall.NewLazyDLL(path).NewProc("GetExtendedTcpTable")
	return proc, proc.Find()
})

// TCPConnectionOwner returns the process on the local side of an existing TCP
// connection. Looking up the connected tuple also covers wildcard listeners.
func TCPConnectionOwner(local, remote netip.AddrPort) (uint32, error) {
	proc, err := getExtendedTCPTable()
	if err != nil {
		return 0, err
	}
	const ownerPIDConnections = 4
	af := AF_INET
	rowSize, localAddr, localPort, remoteAddr, remotePort, pid := 24, 4, 8, 12, 16, 20
	if local.Addr().Is6() {
		af = AF_INET6
		rowSize, localAddr, localPort, remoteAddr, remotePort, pid = 56, 0, 20, 24, 44, 52
	}
	var size uint32
	var buf []byte
	for attempt := 0; attempt < 4; attempt++ {
		result, _, _ := proc.Call(uintptr(unsafe.Pointer(unsafe.SliceData(buf))), uintptr(unsafe.Pointer(&size)), 0, uintptr(af), ownerPIDConnections, 0)
		switch result {
		case 0:
			if len(buf) < 4 {
				return 0, nil
			}
			count := int(binary.LittleEndian.Uint32(buf))
			rows := buf[4:]
			if count > len(rows)/rowSize {
				return 0, fmt.Errorf("invalid Windows TCP table size")
			}
			a, b := local.Addr().AsSlice(), remote.Addr().AsSlice()
			for i := 0; i < count; i++ {
				row := rows[i*rowSize:][:rowSize]
				if binary.BigEndian.Uint16(row[localPort:]) == local.Port() &&
					binary.BigEndian.Uint16(row[remotePort:]) == remote.Port() &&
					bytes.Equal(row[localAddr:localAddr+len(a)], a) &&
					bytes.Equal(row[remoteAddr:remoteAddr+len(b)], b) {
					return binary.LittleEndian.Uint32(row[pid:]), nil
				}
			}
			return 0, nil
		case 122: // ERROR_INSUFFICIENT_BUFFER
			buf = make([]byte, size)
		default:
			return 0, fmt.Errorf("GetExtendedTcpTable returned Windows error %d", result)
		}
	}
	return 0, fmt.Errorf("Windows TCP table changed during lookup")
}
