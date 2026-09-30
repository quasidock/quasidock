package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/quasidock/quasidock/internal/win32"
)

type CredResponse struct {
	ServerURL string
	Username  string
	Secret    string
}

func main() {
	args := os.Args
	if len(args) != 2 || args[1] != "get" {
		win32.ExitProcess(2)
	}

	stdinBytes, err := io.ReadAll(os.Stdin)
	if err != nil {
		win32.ExitProcess(1)
	}
	server := strings.Trim(string(stdinBytes), " \r\n")

	if askedPath := os.Getenv("QUASIDOCK_TEST_HELPER_ASKED"); askedPath != "" {
		_ = os.WriteFile(askedPath, []byte(server), 0644)
	}

	known := os.Getenv("QUASIDOCK_TEST_HELPER_SERVER")
	user := os.Getenv("QUASIDOCK_TEST_HELPER_USER")
	secret := os.Getenv("QUASIDOCK_TEST_HELPER_SECRET")

	if server == known && user != "" {
		resp := CredResponse{
			ServerURL: server,
			Username:  user,
			Secret:    secret,
		}
		data, _ := json.Marshal(resp)
		_, _ = os.Stdout.Write(data)
		win32.ExitProcess(0)
	}

	fmt.Fprintln(os.Stdout, "credentials not found in native keychain")
	win32.ExitProcess(1)
}
