package main

import (
	"os"

	"github.com/zigsaw-io/zigsaw/internal/cli"
	"github.com/zigsaw-io/zigsaw/internal/win32"
)

func main() {
	code := cli.Run(os.Args[1:])
	win32.ExitProcess(code)
}
