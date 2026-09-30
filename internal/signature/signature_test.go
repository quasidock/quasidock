package signature

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const (
	goldenKey = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE25CnZWjylTihKLSvIVi2voJCcrYd
5bmfo16AbdRzcoLV9fqesFy5iCxGV+TXhyz4gCL5ehJlVHsocjAYSg6WZQ==
-----END PUBLIC KEY-----`

	goldenPayload = `{"critical":{"identity":{"docker-reference":"localhost:5000/gate/app"},"image":{"docker-manifest-digest":"sha256:58214bd6d7325459615fb1b8d6cac46e935cd890832e0b7573da78b80d9b155b"},"type":"cosign container image signature"},"optional":null}`

	goldenSignature = `MEYCIQCjZv6rbwcpc5mNwT+x3jiTUvioyKJVI1LuFva0qnrvHgIhAOqUB4i7AP5RM40MeJLMmzG5hvG+QZaU27bZ8afXL+7v`

	goldenRepository = "localhost:5000/gate/app"
	goldenDigest     = "sha256:58214bd6d7325459615fb1b8d6cac46e935cd890832e0b7573da78b80d9b155b"
)

func TestCosignSignatureVerification(t *testing.T) {
	key, err := Parse([]byte(goldenKey))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if !key.Verifies(goldenPayload, goldenSignature) {
		t.Errorf("expected signature to verify")
	}

	if err := CheckPayload([]byte(goldenPayload), goldenRepository, goldenDigest); err != nil {
		t.Errorf("CheckPayload error: %v", err)
	}

	made := MakePayload(goldenRepository, goldenDigest)
	if made != goldenPayload {
		t.Errorf("MakePayload:\ngot:  %s\nwant: %s", made, goldenPayload)
	}
}

func TestTamperingFails(t *testing.T) {
	key, err := Parse([]byte(goldenKey))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Mutate payload
	msgBytes := []byte(goldenPayload)
	for i := 0; i < len(msgBytes); i += 37 {
		msgBytes[i] ^= 1
		if key.Verifies(string(msgBytes), goldenSignature) {
			t.Errorf("expected tampered message at %d to fail", i)
		}
		msgBytes[i] ^= 1
	}

	// Mutate signature
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(goldenSignature))
	if err != nil {
		t.Fatal(err)
	}

	derCopy := make([]byte, len(der))
	copy(derCopy, der)
	for i := 0; i < len(derCopy); i += 7 {
		derCopy[i] ^= 1
		sig := base64.StdEncoding.EncodeToString(derCopy)
		if key.Verifies(goldenPayload, sig) {
			t.Errorf("expected tampered signature at %d to fail", i)
		}
		derCopy[i] ^= 1
	}

	// Trailing byte
	longer := append(derCopy, 0)
	if key.Verifies(goldenPayload, base64.StdEncoding.EncodeToString(longer)) {
		t.Errorf("expected signature with trailing bytes to fail")
	}

	// Truncated
	if key.Verifies(goldenPayload, base64.StdEncoding.EncodeToString(derCopy[:len(derCopy)-1])) {
		t.Errorf("expected truncated signature to fail")
	}

	if key.Verifies(goldenPayload, "") {
		t.Errorf("expected empty signature to fail")
	}
	if key.Verifies(goldenPayload, "not base64!") {
		t.Errorf("expected invalid base64 signature to fail")
	}
}

func TestAnotherKeyDoesNotVerify(t *testing.T) {
	otherPEM := `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEXThf29L0MZDKJ0XmGZOnsa9bTfnm
GwTc6JcpcVqMxsROLSOmJYvylIJpsGBkl2Vq9GfVSO/1BQ3E0/+r5f69cA==
-----END PUBLIC KEY-----`

	other, err := Parse([]byte(otherPEM))
	if err != nil {
		t.Fatal(err)
	}

	if other.Verifies(goldenPayload, goldenSignature) {
		t.Errorf("expected other key signature verification to fail")
	}

	k1, _ := Parse([]byte(goldenKey))
	if other.Fingerprint == k1.Fingerprint {
		t.Errorf("expected different fingerprints")
	}
}

func TestCheckPayload(t *testing.T) {
	otherDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := CheckPayload([]byte(goldenPayload), goldenRepository, otherDigest); !errors.Is(err, ErrOtherImage) {
		t.Errorf("expected ErrOtherImage, got %v", err)
	}

	if err := CheckPayload([]byte(goldenPayload), "localhost:5000/gate/another", goldenDigest); !errors.Is(err, ErrOtherRepository) {
		t.Errorf("expected ErrOtherRepository, got %v", err)
	}

	// Case insensitive repo host
	if err := CheckPayload([]byte(goldenPayload), "LOCALHOST:5000/gate/app", goldenDigest); err != nil {
		t.Errorf("expected uppercase host to match, got %v", err)
	}

	// Invalid payloads
	if err := CheckPayload([]byte(""), goldenRepository, goldenDigest); !errors.Is(err, ErrNotAPayload) {
		t.Errorf("expected ErrNotAPayload for empty, got %v", err)
	}
	if err := CheckPayload([]byte("{}"), goldenRepository, goldenDigest); !errors.Is(err, ErrNotAPayload) {
		t.Errorf("expected ErrNotAPayload for empty obj, got %v", err)
	}

	wrongType := `{"critical":{"identity":{"docker-reference":"r"},"image":{"docker-manifest-digest":"d"},"type":"something else"}}`
	if err := CheckPayload([]byte(wrongType), "r", "d"); !errors.Is(err, ErrNotAPayload) {
		t.Errorf("expected ErrNotAPayload for wrong type, got %v", err)
	}

	// Duplicate fields
	dupField := `{"critical":{"identity":{"docker-reference":"r"},"image":{"docker-manifest-digest":"d","docker-manifest-digest":"e"},"type":"cosign container image signature"}}`
	if err := CheckPayload([]byte(dupField), "r", "e"); !errors.Is(err, ErrNotAPayload) {
		t.Errorf("expected ErrNotAPayload for duplicate fields, got %v", err)
	}
}

func TestInvalidKeys(t *testing.T) {
	badKeys := []string{
		"",
		"-----BEGIN PUBLIC KEY-----\n-----END PUBLIC KEY-----",
		"-----BEGIN ENCRYPTED SIGSTORE PRIVATE KEY-----\nabcd\n-----END ENCRYPTED SIGSTORE PRIVATE KEY-----",
		// Ed25519 in PKIX form
		"-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAGb9ECWmEzf6FQbrBZ9w7lshQhqowtrbLDFw4rXAxZuE=\n-----END PUBLIC KEY-----",
	}

	for _, k := range badKeys {
		if _, err := Parse([]byte(k)); err == nil {
			t.Errorf("expected Parse(%q) to fail", k)
		}
	}
}

func TestTagFor(t *testing.T) {
	tag, err := TagFor(goldenDigest)
	if err != nil {
		t.Fatal(err)
	}
	expected := "sha256-58214bd6d7325459615fb1b8d6cac46e935cd890832e0b7573da78b80d9b155b.sig"
	if tag != expected {
		t.Errorf("TagFor = %s, want %s", tag, expected)
	}

	if _, err := TagFor("invalid-digest"); err == nil {
		t.Errorf("expected error for invalid digest")
	}
}
