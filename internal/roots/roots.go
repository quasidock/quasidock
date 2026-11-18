package roots

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"

	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/win32"
)

const serverAuth = "1.3.6.1.5.5.7.3.1"

type Purposes struct {
	Any  bool
	OIDs []string
}

func (p Purposes) Allows(purpose string) bool {
	if p.Any {
		return true
	}
	for _, oid := range p.OIDs {
		if oid == purpose {
			return true
		}
	}
	return false
}

// File exports Windows' trusted root certificates to <store>/certs/roots-<hex>.pem
// and returns its path.
func File(st *store.Store) (string, error) {
	pemBytes, err := ExportPEM()
	if err != nil {
		return "", err
	}

	h := sha256.Sum256(pemBytes)
	fileName := fmt.Sprintf("roots-%s.pem", hex.EncodeToString(h[:4]))
	dest := st.Path("certs", fileName)

	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	tmp := st.TmpPath("roots")
	if err := os.WriteFile(tmp, pemBytes, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		if _, statErr := os.Stat(dest); statErr == nil {
			return dest, nil
		}
		return "", err
	}
	return dest, nil
}

// Prune deletes obsolete root certificate files in <store>/certs.
func Prune(st *store.Store) error {
	pemBytes, err := ExportPEM()
	if err != nil {
		return err
	}
	h := sha256.Sum256(pemBytes)
	current := fmt.Sprintf("roots-%s.pem", hex.EncodeToString(h[:4]))

	entries, err := os.ReadDir(st.Path("certs"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "roots-") || !strings.HasSuffix(e.Name(), ".pem") {
			continue
		}
		if e.Name() != current {
			_ = os.Remove(st.Path("certs", e.Name()))
		}
	}
	return nil
}

type certItem struct {
	hash [32]byte
	der  []byte
}

var locations = []win32.DWORD{
	win32.CERT_SYSTEM_STORE_LOCAL_MACHINE_ID,
	win32.CERT_SYSTEM_STORE_LOCAL_MACHINE_ENTERPRISE_ID,
	win32.CERT_SYSTEM_STORE_LOCAL_MACHINE_GROUP_POLICY_ID,
	win32.CERT_SYSTEM_STORE_CURRENT_USER_ID,
	win32.CERT_SYSTEM_STORE_CURRENT_USER_GROUP_POLICY_ID,
	win32.CERT_SYSTEM_STORE_SERVICES_ID,
	win32.CERT_SYSTEM_STORE_USERS_ID,
}

var storeNames = []string{"CA", "ROOT"}

// ExportPEM exports all server-trusted root and intermediate certificates from the Windows certificate store.
func ExportPEM() ([]byte, error) {
	var certs []certItem

	for _, name := range storeNames {
		for _, location := range locations {
			flags := win32.CERT_STORE_READONLY_FLAG | win32.CertSystemStore(location)
			hStore, err := win32.CertOpenSystemStore(name, flags)
			if err != nil || hStore == 0 {
				continue
			}

			var next *syscall.CertContext
			for {
				next, _ = win32.CertEnumCertificates(hStore, next)
				if next == nil {
					break
				}
				if next.EncodingType&win32.X509_ASN_ENCODING == 0 {
					continue
				}

				purp, err := getPurposes(next)
				if err != nil || !purp.Allows(serverAuth) {
					continue
				}

				der := win32.CertEncodedBytes(next)

				h := sha256.Sum256(der)
				certs = append(certs, certItem{hash: h, der: der})
			}
			_ = win32.CertCloseStore(hStore, 0)
		}
	}

	sort.Slice(certs, func(i, j int) bool {
		return bytes.Compare(certs[i].hash[:], certs[j].hash[:]) < 0
	})

	var out bytes.Buffer
	for i, c := range certs {
		if i > 0 && c.hash == certs[i-1].hash {
			continue
		}
		_ = pem.Encode(&out, &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: c.der,
		})
	}
	return out.Bytes(), nil
}

func getPurposes(certCtx *syscall.CertContext) (Purposes, error) {
	// Windows intersects the extension and store property when flags are zero.
	return getUsage(certCtx, 0)
}

func getUsage(certCtx *syscall.CertContext, flags win32.DWORD) (Purposes, error) {
	oids, unrestricted, err := win32.CertEnhancedKeyUsageOIDs(certCtx, flags)
	if err != nil {
		return Purposes{}, err
	}
	if unrestricted {
		return Purposes{Any: true}, nil
	}
	return Purposes{Any: false, OIDs: oids}, nil
}
