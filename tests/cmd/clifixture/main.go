// clifixture exercises foreground CLI behavior without downloaded toolchains.
package main

import (
	"encoding/json"
	"fmt"
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
		}
	}
	json.NewEncoder(os.Stdout).Encode(args)
}
