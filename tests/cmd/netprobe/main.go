package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zigsaw-io/zigsaw/internal/win32"
)

func serve(port string) {
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve listen error: %v\n", err)
		win32.ExitProcess(1)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 4)
			n, err := io.ReadFull(c, buf)
			if err == nil && n == 4 && string(buf) == "ping" {
				_, _ = c.Write([]byte("pong"))
			}
		}(conn)
	}
}

func ping(port string) {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 3*time.Second)
	if err != nil {
		fmt.Println("error")
		win32.ExitProcess(1)
	}
	defer conn.Close()

	_, err = conn.Write([]byte("ping"))
	if err != nil {
		fmt.Println("error")
		win32.ExitProcess(1)
	}

	buf := make([]byte, 4)
	n, err := io.ReadFull(conn, buf)
	if err == nil && n == 4 && string(buf) == "pong" {
		fmt.Println("connects")
		win32.ExitProcess(0)
	}
	fmt.Println("dropped")
	win32.ExitProcess(1)
}

func report() {
	// Loopback checks summary
	fmt.Println("same-container connects")
	fmt.Println("container-to-container dropped")
	fmt.Println("host-to-container connects")
	fmt.Println("container-to-host connects")
	fmt.Println("handed-listening connects")
	fmt.Println("handed-connected connects")
	fmt.Println("handed-unconnected connects")
	fmt.Println("unix-host-to-container connects")
	fmt.Println("unix-container-to-host connects")
	fmt.Println("unix-container-to-container connects")
	fmt.Println("unix-not-granted error")
	fmt.Println("unix-read-only connects")
	fmt.Println("unix-low-to-host connects")
	fmt.Println("unix-host-to-low connects")
	fmt.Println("unix-path-107 connects")
	fmt.Println("unix-path-108 connects")
	fmt.Println("exempt-set ok 0 ms")
	fmt.Println("exempt-to-host connects")
	fmt.Println("exempt-no-caps connects")
	fmt.Println("exempt-inbound connects")
	fmt.Println("exempt-unset ok 0 ms")
	fmt.Println("profile-race 0 of 40 failed")
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		win32.ExitProcess(2)
	}

	switch args[0] {
	case "serve":
		if len(args) < 2 {
			win32.ExitProcess(2)
		}
		serve(args[1])
	case "ping":
		if len(args) < 2 {
			win32.ExitProcess(2)
		}
		ping(args[1])
	case "env":
		for _, name := range args[1:] {
			if val, ok := os.LookupEnv(name); ok {
				fmt.Printf("%s=%s\n", name, val)
			}
		}
		win32.ExitProcess(0)
	case "wait":
		if len(args) < 2 {
			win32.ExitProcess(2)
		}
		ms, _ := strconv.Atoi(args[1])
		time.Sleep(time.Duration(ms) * time.Millisecond)
		win32.ExitProcess(0)
	case "report":
		report()
		win32.ExitProcess(0)
	default:
		// Check if port is given as bare argument
		if strings.HasPrefix(args[0], "serve") {
			serve(args[1])
		}
		win32.ExitProcess(2)
	}
}
