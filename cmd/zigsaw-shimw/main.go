package main

import (
	"github.com/zigsaw-io/zigsaw/internal/shim"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

func main() {
	code := shim.Run(true)
	win32.ExitProcess(code)
}
