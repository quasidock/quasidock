package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"

	"github.com/quasidock/quasidock/internal/signature"
	"github.com/quasidock/quasidock/internal/win32"
)

func deriveKey(word string) *ecdsa.PrivateKey {
	seed := sha256.Sum256([]byte(word))
	c := elliptic.P256()
	d := new(big.Int).SetBytes(seed[:])
	// Ensure 1 <= d < order
	order := c.Params().N
	d.Mod(d, new(big.Int).Sub(order, big.NewInt(1)))
	d.Add(d, big.NewInt(1))

	var raw [32]byte
	d.FillBytes(raw[:])
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw[:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ParseRawPrivateKey: %v\n", err)
		win32.ExitProcess(1)
	}
	return priv
}

func main() {
	args := os.Args
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: quasidock-p256sign key|sign <word> <file>")
		win32.ExitProcess(2)
	}

	mode := args[1]
	word := args[2]
	file := args[3]

	priv := deriveKey(word)

	if mode == "key" {
		der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "MarshalPKIXPublicKey: %v\n", err)
			win32.ExitProcess(1)
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{
			Type:  "PUBLIC KEY",
			Bytes: der,
		})
		if err := os.WriteFile(file, pemBytes, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "WriteFile: %v\n", err)
			win32.ExitProcess(1)
		}
		win32.ExitProcess(0)
	}

	if mode == "sign" {
		data, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ReadFile: %v\n", err)
			win32.ExitProcess(1)
		}
		h := sha256.Sum256(data)
		sig, err := ecdsa.SignASN1(rand.Reader, priv, h[:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "SignASN1: %v\n", err)
			win32.ExitProcess(1)
		}
		fmt.Print(base64.StdEncoding.EncodeToString(sig))
		win32.ExitProcess(0)
	}

	fmt.Fprintln(os.Stderr, "usage: quasidock-p256sign key|sign <word> <file>")
	win32.ExitProcess(2)
}

// Suppress unused warning in tests
var _ = signature.Parse
