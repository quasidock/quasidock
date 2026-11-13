package roots

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/store"
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
