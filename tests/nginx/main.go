// Local HTTP/FastCGI backends and a verifying HTTP/2 client for nginx.sh.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/fcgi"
	"os"
	"syscall"
	"time"
)

func run() error {
	if len(os.Args) == 3 && os.Args[1] == "shortpath" {
		path, err := syscall.UTF16PtrFromString(os.Args[2])
		if err != nil {
			return err
		}
		buffer := make([]uint16, 32768)
		n, err := syscall.GetShortPathName(path, &buffer[0], uint32(len(buffer)))
		if err != nil {
			return err
		}
		if n >= uint32(len(buffer)) {
			return fmt.Errorf("short path exceeds buffer")
		}
		fmt.Println(syscall.UTF16ToString(buffer[:n]))
		return nil
	}
	if len(os.Args) != 4 {
		return fmt.Errorf("usage: nginx-probe serve HTTP-ADDRESS FCGI-ADDRESS | h2 URL CA-FILE")
	}
	if os.Args[1] == "h2" {
		pem, err := os.ReadFile(os.Args[3])
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return fmt.Errorf("invalid test CA")
		}
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		response, err := client.Get(os.Args[2])
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.ProtoMajor != 2 || response.StatusCode != http.StatusOK {
			return fmt.Errorf("expected HTTP/2 200, got %s %s", response.Proto, response.Status)
		}
		_, err = io.Copy(os.Stdout, response.Body)
		return err
	}
	if os.Args[1] != "serve" {
		return fmt.Errorf("unknown probe command %q", os.Args[1])
	}
	httpListener, err := net.Listen("tcp", os.Args[2])
	if err != nil {
		return err
	}
	defer httpListener.Close()
	fcgiListener, err := net.Listen("tcp", os.Args[3])
	if err != nil {
		return err
	}
	defer fcgiListener.Close()
	errors := make(chan error, 2)
	go func() {
		server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(w, "proxy %s %s %s", r.Method, r.URL.Path, body)
		})}
		errors <- server.Serve(httpListener)
	}()
	go func() {
		errors <- fcgi.Serve(fcgiListener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprintf(w, "fastcgi %s %s", r.Method, r.URL.Path)
		}))
	}()
	fmt.Println("ready")
	return <-errors
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
