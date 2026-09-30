package signature

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	PayloadMediaType    = "application/vnd.dev.cosign.simplesigning.v1+json"
	SignatureAnnotation = "dev.cosignproject.cosign/signature"
	PayloadType         = "cosign container image signature"
	MaxPayloadSize      = 64 * 1024
)

var (
	ErrNotAKey         = errors.New("not a valid P-256 public key")
	ErrInvalidDigest   = errors.New("invalid digest")
	ErrNotAPayload     = errors.New("not a valid cosign signature payload")
	ErrOtherImage      = errors.New("signature is for another image")
	ErrOtherRepository = errors.New("signature is for another repository")
)

type Key struct {
	Public      *ecdsa.PublicKey
	Fingerprint string // 16 hex chars (8 bytes) of sha256 of PKIX DER
}

// Parse parses a PEM-encoded P-256 public key (as cosign generate-key-pair creates).
func Parse(pemBytes []byte) (*Key, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, ErrNotAKey
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, ErrNotAKey
	}

	ecdsaPub, ok := pub.(*ecdsa.PublicKey)
	if !ok || ecdsaPub.Curve != elliptic.P256() {
		return nil, ErrNotAKey
	}

	h := sha256.Sum256(block.Bytes)
	fp := hex.EncodeToString(h[:])[:16]

	return &Key{
		Public:      ecdsaPub,
		Fingerprint: fp,
	}, nil
}

// Verifies checks whether signature (base64-encoded ASN.1 DER) is this key's signature over message.
func (k *Key) Verifies(message, signature string) bool {
	sigStr := strings.TrimSpace(signature)
	der, err := base64.StdEncoding.DecodeString(sigStr)
	if err != nil || len(der) == 0 {
		return false
	}

	h := sha256.Sum256([]byte(message))
	return ecdsa.VerifyASN1(k.Public, h[:], der)
}

// TagFor returns the .sig tag name for an image digest.
func TagFor(digest string) (string, error) {
	colon := strings.IndexByte(digest, ':')
	if colon == -1 {
		return "", ErrInvalidDigest
	}
	return fmt.Sprintf("%s-%s.sig", digest[:colon], digest[colon+1:]), nil
}

// MakePayload creates the cosign simple-signing payload JSON.
func MakePayload(repository, digest string) string {
	return fmt.Sprintf(`{"critical":{"identity":{"docker-reference":"%s"},"image":{"docker-manifest-digest":"%s"},"type":"%s"},"optional":null}`,
		repository, digest, PayloadType)
}

type Payload struct {
	Repository string
	Digest     string
}

// ParsePayload parses a cosign simple-signing JSON payload.
func ParsePayload(data []byte) (*Payload, error) {
	if len(data) == 0 || len(data) > MaxPayloadSize {
		return nil, ErrNotAPayload
	}

	// Verify no duplicate keys
	if err := checkNoDuplicateKeys(data); err != nil {
		return nil, ErrNotAPayload
	}

	var root struct {
		Critical struct {
			Identity struct {
				DockerReference string `json:"docker-reference"`
			} `json:"identity"`
			Image struct {
				DockerManifestDigest string `json:"docker-manifest-digest"`
			} `json:"image"`
			Type string `json:"type"`
		} `json:"critical"`
	}

	if err := json.Unmarshal(data, &root); err != nil {
		return nil, ErrNotAPayload
	}

	if root.Critical.Type != PayloadType {
		return nil, ErrNotAPayload
	}

	if root.Critical.Identity.DockerReference == "" || root.Critical.Image.DockerManifestDigest == "" {
		return nil, ErrNotAPayload
	}

	return &Payload{
		Repository: root.Critical.Identity.DockerReference,
		Digest:     root.Critical.Image.DockerManifestDigest,
	}, nil
}

func checkNoDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var stack []map[string]bool

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		switch t := tok.(type) {
		case json.Delim:
			if t == '{' {
				stack = append(stack, make(map[string]bool))
			} else if t == '}' {
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
		case string:
			if len(stack) > 0 && dec.More() {
				// In an object, token is a key if stack is non-empty and there's a following value
				// Actually, dec.Token() returns alternating keys and values in an object.
				// But simpler: track key counts at current depth.
			}
		}
	}
	return checkObjectDuplicates(data)
}

func checkObjectDuplicates(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return scanJSONValue(dec)
}

func scanJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return ErrNotAPayload
			}
			if seen[key] {
				return ErrNotAPayload
			}
			seen[key] = true
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		// consume closing '}'
		_, err := dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		// consume closing ']'
		_, err := dec.Token()
		return err
	}
	return nil
}

// CheckPayload checks that payload is valid, matches digest, and matches repository.
func CheckPayload(data []byte, repository, digest string) error {
	p, err := ParsePayload(data)
	if err != nil {
		return err
	}
	if p.Digest != digest {
		return ErrOtherImage
	}
	if !strings.EqualFold(p.Repository, repository) {
		return ErrOtherRepository
	}
	return nil
}
