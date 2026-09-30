package regkey

import (
	"fmt"
	"unsafe"

	"github.com/quasidock/quasidock/internal/win32"
)

// Key is an open registry key handle wrapper.
type Key struct {
	Handle win32.HKEY
}

func check(doing, what string, status int32) error {
	if status == win32.ERROR_SUCCESS {
		return nil
	}
	return fmt.Errorf("%s %s in the registry: error %d (%s)", doing, what, status, win32.ErrorName(status))
}

// Create opens or creates parent's key sub with KEY_READ | KEY_WRITE.
func Create(parent win32.HKEY, sub string) (Key, error) {
	return CreateWith(parent, sub, win32.KEY_READ|win32.KEY_WRITE)
}

// CreateWith opens or creates parent's key sub with the specified access.
func CreateWith(parent win32.HKEY, sub string, access win32.DWORD) (Key, error) {
	h, status := win32.RegCreateKeyEx(parent, sub, access)
	if err := check("creating", sub, status); err != nil {
		return Key{}, err
	}
	return Key{Handle: h}, nil
}

// Open opens parent's subkey. Returns nil, nil if the key does not exist.
func Open(parent win32.HKEY, sub string, access win32.DWORD) (*Key, error) {
	h, status := win32.RegOpenKeyEx(parent, sub, access)
	if status == win32.ERROR_FILE_NOT_FOUND {
		return nil, nil
	}
	if err := check("opening", sub, status); err != nil {
		return nil, err
	}
	return &Key{Handle: h}, nil
}

// Exists checks if parent has the subkey.
func Exists(parent win32.HKEY, sub string) (bool, error) {
	k, err := Open(parent, sub, win32.KEY_READ)
	if err != nil {
		return false, err
	}
	if k == nil {
		return false, nil
	}
	k.Close()
	return true, nil
}

// Close closes the key handle.
func (k Key) Close() {
	if k.Handle != 0 {
		_ = win32.RegCloseKey(k.Handle)
	}
}

// SetString sets a string value; a nil name sets the key's default value.
func (k Key) SetString(name *string, value string) error {
	var namePtr *uint16
	nameDesc := "(default)"
	if name != nil {
		namePtr = win32.UTF16Ptr(*name)
		nameDesc = *name
	}
	u16 := win32.UTF8ToUTF16(value)
	u16 = append(u16, 0)
	bytes := unsafe.Slice((*byte)(unsafe.Pointer(&u16[0])), len(u16)*2)
	status := win32.RegSetValueEx(k.Handle, namePtr, win32.REG_SZ, bytes)
	return check("setting", nameDesc, status)
}

// GetString reads a string value. Returns nil, nil if value does not exist or is not REG_SZ.
func (k Key) GetString(name string) (*string, error) {
	namePtr := win32.UTF16Ptr(name)
	var kind win32.DWORD
	var size win32.DWORD
	status := win32.RegQueryValueEx(k.Handle, namePtr, &kind, nil, &size)
	if status == win32.ERROR_FILE_NOT_FOUND {
		return nil, nil
	}
	if err := check("reading", name, status); err != nil {
		return nil, err
	}
	if kind != win32.REG_SZ {
		return nil, nil
	}

	buf := make([]byte, size+2)
	status = win32.RegQueryValueEx(k.Handle, namePtr, nil, buf, &size)
	if err := check("reading", name, status); err != nil {
		return nil, err
	}

	u16Slice := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[0])), int(size/2))
	s := win32.UTF16ToUTF8(u16Slice)
	return &s, nil
}

// DeleteValue deletes the named value.
func (k Key) DeleteValue(name string) error {
	status := win32.RegDeleteValue(k.Handle, name)
	if status != win32.ERROR_FILE_NOT_FOUND {
		return check("deleting", name, status)
	}
	return nil
}

// SubKeys returns the names of the key's subkeys.
func (k Key) SubKeys() ([]string, error) {
	var out []string
	var index win32.DWORD
	for {
		var buf [256]uint16
		lenBuf := win32.DWORD(len(buf))
		status := win32.RegEnumKeyEx(k.Handle, index, &buf[0], &lenBuf)
		if status == win32.ERROR_NO_MORE_ITEMS {
			break
		}
		if err := check("listing", "keys", status); err != nil {
			return nil, err
		}
		out = append(out, win32.UTF16ToUTF8(buf[:lenBuf]))
		index++
	}
	return out, nil
}

// ValueNames returns the names of the key's values, excluding default value.
func (k Key) ValueNames() ([]string, error) {
	var maxName win32.DWORD
	status := win32.RegQueryInfoKey(k.Handle, nil, nil, nil, &maxName, nil)
	if err := check("reading", "a key", status); err != nil {
		return nil, err
	}

	nameBuf := make([]uint16, maxName+1)
	var out []string
	var index win32.DWORD
	for {
		lenBuf := win32.DWORD(len(nameBuf))
		status := win32.RegEnumValue(k.Handle, index, &nameBuf[0], &lenBuf, nil, nil, nil)
		if status == win32.ERROR_NO_MORE_ITEMS {
			break
		}
		if err := check("listing", "values", status); err != nil {
			return nil, err
		}
		if lenBuf > 0 {
			out = append(out, win32.UTF16ToUTF8(nameBuf[:lenBuf]))
		}
		index++
	}
	return out, nil
}

// IsEmpty returns true if the key has no subkeys and no values.
func (k Key) IsEmpty() (bool, error) {
	var keys win32.DWORD
	var values win32.DWORD
	status := win32.RegQueryInfoKey(k.Handle, &keys, nil, &values, nil, nil)
	if err := check("reading", "a key", status); err != nil {
		return false, err
	}
	return keys == 0 && values == 0, nil
}

// DeleteTree deletes the subkey and all its children.
func (k Key) DeleteTree(sub string) error {
	status := win32.RegDeleteTree(k.Handle, sub)
	if status == win32.ERROR_FILE_NOT_FOUND {
		return nil
	}
	return check("deleting", sub, status)
}

// DeleteKey deletes subkey, which must have no subkeys of its own.
func (k Key) DeleteKey(sub string) error {
	status := win32.RegDeleteKey(k.Handle, sub)
	if status != win32.ERROR_FILE_NOT_FOUND {
		return check("deleting", sub, status)
	}
	return nil
}

// CopyFrom copies values and subkeys from src into k, value by value.
func (k Key) CopyFrom(src Key) error {
	var maxName win32.DWORD
	var maxData win32.DWORD
	status := win32.RegQueryInfoKey(src.Handle, nil, nil, nil, &maxName, &maxData)
	if err := check("reading", "a key", status); err != nil {
		return err
	}

	nameBuf := make([]uint16, maxName+1)
	dataCap := maxData
	if dataCap < 1 {
		dataCap = 1
	}
	dataBuf := make([]byte, dataCap)

	var index win32.DWORD
	for {
		nameLen := win32.DWORD(len(nameBuf))
		dataLen := win32.DWORD(len(dataBuf))
		var kind win32.DWORD
		status := win32.RegEnumValue(src.Handle, index, &nameBuf[0], &nameLen, &kind, &dataBuf[0], &dataLen)
		if status == win32.ERROR_NO_MORE_ITEMS {
			break
		}
		if err := check("reading", "a value", status); err != nil {
			return err
		}

		var valNamePtr *uint16
		if nameLen > 0 {
			nameBuf[nameLen] = 0
			valNamePtr = &nameBuf[0]
		}
		setSt := win32.RegSetValueEx(k.Handle, valNamePtr, kind, dataBuf[:dataLen])
		if err := check("copying", "a value", setSt); err != nil {
			return err
		}
		index++
	}

	subKeys, err := src.SubKeys()
	if err != nil {
		return err
	}
	for _, sub := range subKeys {
		from, err := Open(src.Handle, sub, win32.KEY_READ)
		if err != nil {
			return err
		}
		if from == nil {
			continue
		}
		to, err := CreateWith(k.Handle, sub, win32.KEY_ALL_ACCESS)
		if err != nil {
			from.Close()
			return err
		}
		err = to.CopyFrom(*from)
		from.Close()
		to.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
