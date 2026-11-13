package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/zigsaw-io/zigsaw/internal/signature"
)

func TestP256SignAndVerify(t *testing.T) {
	tmpDir := t.TempDir()
	pubPath := filepath.Join(tmpDir, "cosign.pub")
	msgPath := filepath.Join(tmpDir, "msg.txt")

	message := []byte("hello signature world")
	if err := os.WriteFile(msgPath, message, 0644); err != nil {
		t.Fatal(err)
	}

	priv := deriveKey("testword")
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: der,
	})
	if err := os.WriteFile(pubPath, pemBytes, 0644); err != nil {
		t.Fatal(err)
	}

	h := sha256.Sum256(message)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, h[:])
	if err != nil {
		t.Fatal(err)
	}
	b64Sig := base64.StdEncoding.EncodeToString(sig)

	key, err := signature.Parse(pemBytes)
	if err != nil {
		t.Fatalf("signature.Parse failed: %v", err)
	}

	if !key.Verifies(string(message), b64Sig) {
		t.Fatal("expected signature to verify successfully")
	}
}
