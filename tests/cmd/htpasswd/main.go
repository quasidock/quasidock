package main

import (
	"fmt"
	"os"

	"github.com/quasidock/quasidock/internal/win32"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	args := os.Args
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: htpasswd <user> <password>")
		win32.ExitProcess(2)
	}

	user := args[1]
	password := args[2]

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bcrypt failed: %v\n", err)
		win32.ExitProcess(1)
	}

	fmt.Printf("%s:%s\n", user, hash)
	win32.ExitProcess(0)
}
