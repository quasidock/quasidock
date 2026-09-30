package main

import (
	"fmt"
	"os"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	var s []uint16
	for *p != 0 {
		s = append(s, *p)
		p = (*uint16)(unsafe.Add(unsafe.Pointer(p), 2))
	}
	return string(utf16.Decode(s))
}

func ucrtArgs() ([]string, bool) {
	ucrt := syscall.NewLazyDLL("ucrtbase.dll")
	if err := ucrt.Load(); err != nil {
		return nil, false
	}
	// _crt_argv_unexpanded_arguments = 1 parses with UCRT rules without loading shell32.dll.
	r, _, _ := ucrt.NewProc("_configure_wide_argv").Call(1)
	if r != 0 {
		return nil, false
	}
	pArgc, _, _ := ucrt.NewProc("__p___argc").Call()
	pArgv, _, _ := ucrt.NewProc("__p___wargv").Call()
	if pArgc == 0 || pArgv == 0 {
		return nil, false
	}
	argc := *(*int32)(unsafe.Pointer(pArgc))
	argv := *(***uint16)(unsafe.Pointer(pArgv))
	if argc <= 0 || argv == nil {
		return nil, false
	}
	arr := (*[4096]*uint16)(unsafe.Pointer(argv))[:argc:argc]
	out := make([]string, argc)
	for i, p := range arr {
		out[i] = utf16PtrToString(p)
	}
	return out, true
}

func main() {
	args, ok := ucrtArgs()
	if !ok {
		args = os.Args
	}
	for _, arg := range args[1:] {
		fmt.Printf("%x\n", arg)
	}
}
