package roots

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

func TestAllowsServerAuth(t *testing.T) {
	if !(Purposes{Any: true}).Allows(serverAuth) {
		t.Error("expected Any to allow serverAuth")
	}
	if !(Purposes{OIDs: []string{"1.3.6.1.5.5.7.3.2", serverAuth}}).Allows(serverAuth) {
		t.Error("expected matching OID to allow serverAuth")
	}
	if (Purposes{OIDs: []string{"1.3.6.1.5.5.7.3.3"}}).Allows(serverAuth) {
		t.Error("expected non-matching OID not to allow serverAuth")
	}
	if (Purposes{OIDs: nil}).Allows(serverAuth) {
		t.Error("expected empty OIDs not to allow serverAuth")
	}
}

func TestCertificatePurposes(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	crypt := syscall.NewLazyDLL("crypt32.dll")
	create := crypt.NewProc("CertCreateCertificateContext")
	free := crypt.NewProc("CertFreeCertificateContext")
	setUsage := crypt.NewProc("CertSetEnhancedKeyUsage")
	for _, tc := range []struct {
		name     string
		ext      []x509.ExtKeyUsage
		property *[]string
		want     bool
	}{
		{name: "unrestricted", want: true},
		{name: "server extension", ext: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, want: true},
		{name: "client extension", ext: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
		{name: "empty property", property: &[]string{}},
		{name: "server property", property: &[]string{serverAuth}, want: true},
		{name: "property cannot widen extension", ext: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, property: &[]string{serverAuth}},
		{name: "property restricts extension", ext: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, property: &[]string{"1.3.6.1.5.5.7.3.2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: tc.ext}
			der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			ctx, _, err := create.Call(win32.X509_ASN_ENCODING, uintptr(unsafe.Pointer(&der[0])), uintptr(len(der)))
			if ctx == 0 {
				t.Fatal(err)
			}
			defer free.Call(ctx)
			if tc.property != nil {
				var encoded [][]byte
				var pointers []uintptr
				for _, oid := range *tc.property {
					b := append([]byte(oid), 0)
					encoded = append(encoded, b)
					pointers = append(pointers, uintptr(unsafe.Pointer(&b[0])))
				}
				usage := win32.CertEnhKeyUsage{Count: uint32(len(pointers))}
				if len(pointers) != 0 {
					usage.Identifiers = &pointers[0]
				}
				ok, _, err := setUsage.Call(ctx, uintptr(unsafe.Pointer(&usage)))
				runtime.KeepAlive(encoded)
				if ok == 0 {
					t.Fatal(err)
				}
			}
			purposes, err := getPurposes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := purposes.Allows(serverAuth); got != tc.want {
				t.Fatalf("allows server authentication = %v, want %v (purposes: %+v)", got, tc.want, purposes)
			}
		})
	}
}

func TestExportPEMAndFile(t *testing.T) {
	pemBytes, err := ExportPEM()
	if err != nil {
		t.Fatalf("ExportPEM failed: %v", err)
	}

	if !strings.HasPrefix(string(pemBytes), "-----BEGIN CERTIFICATE-----\n") {
		t.Fatalf("expected BEGIN CERTIFICATE prefix, got %q", string(pemBytes[:50]))
	}
	if !strings.HasSuffix(string(pemBytes), "-----END CERTIFICATE-----\n") {
		t.Fatalf("expected END CERTIFICATE suffix")
	}

	// Verify deterministic output
	pem2, err := ExportPEM()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pemBytes, pem2) {
		t.Fatal("expected ExportPEM to be byte-identical on repeat")
	}

	// Parse certificates using crypto/x509
	rest := pemBytes
	parsedCount := 0
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		_, err := x509.ParseCertificate(block.Bytes)
		if err == nil {
			parsedCount++
		}
	}

	if parsedCount == 0 {
		t.Fatalf("expected at least one valid certificate parsed from Windows store, got 0")
	}
	t.Logf("Successfully exported %d server-trusted certificates from Windows stores", parsedCount)

	// Test store integration
	tmpDir := t.TempDir()
	st, err := store.Open(tmpDir, false, "")
	if err != nil {
		t.Fatal(err)
	}

	certPath, err := File(st)
	if err != nil {
		t.Fatalf("File failed: %v", err)
	}
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("cert file not found at %s: %v", certPath, err)
	}

	if err := Prune(st); err != nil {
		t.Fatalf("Prune failed: %v", err)
	}
}
