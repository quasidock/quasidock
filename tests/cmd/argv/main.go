package main

import (
	"fmt"
	"os"

	"github.com/quasidock/quasidock/internal/win32"
)

func main() {
	args, ok := win32.UCRTArgs()
	if !ok {
		args = os.Args
	}
	for _, arg := range args[1:] {
		fmt.Printf("%x\n", arg)
	}
}
