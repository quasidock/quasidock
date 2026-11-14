// clifixture exercises foreground CLI behavior without downloaded toolchains.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "env":
			for _, name := range args[1:] {
				value, ok := os.LookupEnv(name)
				fmt.Printf("%s=%s:%t\n", name, value, ok)
			}
			return
		case "exit":
			code, _ := strconv.Atoi(args[1])
			os.Exit(code)
		case "hold":
			if err := os.WriteFile(args[1], []byte("ready"), 0o644); err != nil {
				panic(err)
			}
			for {
				time.Sleep(time.Second)
			}
		case "write":
			if err := os.WriteFile(os.Getenv("HOME")+"\\saved.txt", []byte(args[1]), 0o644); err != nil {
				panic(err)
			}
			return
		case "read":
			data, err := os.ReadFile(os.Getenv("HOME") + "\\saved.txt")
			if err != nil {
				os.Exit(3)
			}
			fmt.Print(string(data))
			return
		case "stdin":
			data, err := io.ReadAll(os.Stdin)
			if err != nil || len(data) == 0 {
				os.Exit(4)
			}
			_, _ = os.Stdout.Write(data)
			return
		case "log":
			if len(args) > 1 {
				fmt.Fprintln(os.Stdout, args[1])
			}
			if len(args) > 2 {
				fmt.Fprintln(os.Stderr, args[2])
			}
			return
		case "lines":
			for _, line := range args[1:] {
				fmt.Fprintln(os.Stdout, line)
			}
			return
		}
	}
	json.NewEncoder(os.Stdout).Encode(args)
}
