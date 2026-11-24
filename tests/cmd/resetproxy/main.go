package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/quasidock/quasidock/internal/win32"
)

var answer500 = false

func readHead(r io.Reader) ([]byte, error) {
	buf := make([]byte, 64*1024)
	got := 0
	for {
		if bytes.Contains(buf[:got], []byte("\r\n\r\n")) {
			return buf[:got], nil
		}
		if got == len(buf) {
			return nil, fmt.Errorf("head too long")
		}
		n, err := r.Read(buf[got:])
		if err != nil {
			return nil, err
		}
		got += n
	}
}

func contentLength(head []byte) *int64 {
	lines := strings.Split(string(head), "\r\n")
	for _, line := range lines {
		colon := strings.IndexByte(line, ':')
		if colon == -1 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:colon]))
		if key == "content-length" {
			val, err := strconv.ParseInt(strings.TrimSpace(line[colon+1:]), 10, 64)
			if err == nil {
				return &val
			}
		}
	}
	return nil
}

func serve(client net.Conn, upstreamPort string, part string, targetBytes int64, resetDone bool) bool {
	defer func() { _ = client.Close() }()

	reqHead, err := readHead(client)
	if err != nil {
		return resetDone
	}

	firstLineEnd := bytes.Index(reqHead, []byte("\r\n"))
	if firstLineEnd == -1 {
		return resetDone
	}
	line := string(reqHead[:firstLineEnd])

	if answer500 && strings.HasPrefix(line, "GET ") && strings.Contains(line, part) {
		_, _ = client.Write([]byte("HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
		return resetDone
	}

	upstream, err := net.Dial("tcp", "127.0.0.1:"+upstreamPort)
	if err != nil {
		return resetDone
	}
	defer func() { _ = upstream.Close() }()

	_, _ = upstream.Write(reqHead)

	respHead, err := readHead(upstream)
	if err != nil {
		return resetDone
	}

	headEnd := bytes.Index(respHead, []byte("\r\n\r\n")) + 4
	head := respHead[:headEnd]
	bodyStart := respHead[headEnd:]

	statusEnd := bytes.Index(head, []byte("\r\n"))
	if statusEnd == -1 {
		return resetDone
	}

	_, _ = client.Write(head[:statusEnd+2])
	_, _ = client.Write([]byte("Connection: close\r\n"))
	_, _ = client.Write(head[statusEnd+2:])

	isHead := strings.HasPrefix(line, "HEAD ")
	length := contentLength(head)
	hit := !resetDone && strings.HasPrefix(line, "GET ") && strings.Contains(line, part) && length != nil && *length > targetBytes

	var sent int64 = 0
	body := bodyStart

	for !isHead {
		if length != nil && sent >= *length {
			break
		}
		if len(body) == 0 {
			buf := make([]byte, 16*1024)
			n, err := upstream.Read(buf)
			if err != nil || n <= 0 {
				break
			}
			body = buf[:n]
		}
		take := len(body)
		if hit && sent+int64(take) > targetBytes {
			take = int(targetBytes - sent)
		}
		_, _ = client.Write(body[:take])
		sent += int64(take)
		body = body[take:]

		if hit && sent == targetBytes {
			time.Sleep(500 * time.Millisecond)
			if tcp, ok := client.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			return true
		}
	}

	return resetDone
}

func main() {
	args := os.Args[1:]
	if len(args) != 4 && (len(args) != 5 || args[4] != "--answer-500") {
		fmt.Fprintln(os.Stderr, "usage: resetproxy <port> <upstream port> <path part> <bytes> [--answer-500]")
		win32.ExitProcess(2)
	}

	answer500 = len(args) == 5
	port := args[0]
	upstreamPort := args[1]
	part := args[2]
	targetBytes, _ := strconv.ParseInt(args[3], 10, 64)

	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen error: %v\n", err)
		win32.ExitProcess(1)
	}
	defer func() { _ = ln.Close() }()

	resetDone := false
	for {
		client, err := ln.Accept()
		if err != nil {
			continue
		}
		resetDone = serve(client, upstreamPort, part, targetBytes, resetDone)
	}
}
