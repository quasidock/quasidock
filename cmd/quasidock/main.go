package main

import (
	"os"

	"github.com/quasidock/quasidock/internal/cli"
	"github.com/quasidock/quasidock/internal/win32"
)

func main() {
	code := cli.Run(os.Args[1:])
	win32.ExitProcess(code)
}
