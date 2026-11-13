package main

import (
	"fmt"
	"os"

	"github.com/zigsaw-io/zigsaw/internal/win32"
)

func main() {
	args := os.Args
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: htpasswd <user> <password>")
		win32.ExitProcess(2)
	}

	user := args[1]
	password := args[2]

	hash, err := bcryptHash([]byte(password), 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bcryptHash failed: %v\n", err)
		win32.ExitProcess(1)
	}

	fmt.Printf("%s:%s\n", user, hash)
	win32.ExitProcess(0)
}
