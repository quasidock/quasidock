package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zigsaw-io/zigsaw/internal/signature"
	"github.com/zigsaw-io/zigsaw/internal/store"
)

const (
	PublishedPrefix = "ghcr.io/zigsaw-io"
)

var PublishedKeys = []string{
	// The one that signs.
	`-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEUZck4bKpm1+mXkfSwwtKR7F/5wtN
Bv0hEXNn87aFzykUsp9tr5ObOFmfhMYic5jpqF770iN5rpLgiHIfhKrn9Q==
-----END PUBLIC KEY-----`,
	// The backup.
	`-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEW9tLORn/aGD3/ABpidEgKtLNAclh
MkSEg6LPrxr7b9mkhjFJ3+YTZ00H4tFj87++RLucN6Q8hy1WbefSA30v+w==
-----END PUBLIC KEY-----`,
}

type Named struct {
	Key     *signature.Key
	Prefix  string
	BuiltIn bool
}

// UnderPrefix checks whether repository (as <host>/<repository>) is under prefix by whole path components.
func UnderPrefix(prefix, repository string) bool {
	trimmed := strings.TrimRight(prefix, "/")
	if len(trimmed) == 0 || len(repository) < len(trimmed) {
		return false
	}
	if !strings.EqualFold(repository[:len(trimmed)], trimmed) {
		return false
	}
	return len(repository) == len(trimmed) || repository[len(trimmed)] == '/'
}

type Entry struct {
	Prefix string `json:"prefix"`
	Key    string `json:"key"`
}

type File struct {
	Keys []Entry `json:"keys"`
}

func trustPath(st *store.Store) string {
	return filepath.Join(st.Root, "trust.json")
}

func readTrust(st *store.Store) (*File, error) {
	p := trustPath(st)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &File{Keys: []Entry{}}, nil
		}
		return nil, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s isn't what zigsaw wrote: fix or delete it: %w", p, err)
	}
	return &f, nil
}

func writeTrust(st *store.Store, entries []Entry) error {
	p := trustPath(st)
	if len(entries) == 0 {
		err := os.Remove(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(File{Keys: entries}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0666)
}

// List prints all trusted keys.
func List(st *store.Store) error {
	keys, err := All(st)
	if err != nil {
		return err
	}
	for _, named := range keys {
		builtIn := ""
		if named.BuiltIn {
			builtIn = ", built in"
		}
		fmt.Printf("%s  key %s%s\n", named.Prefix, named.Key.Fingerprint, builtIn)
	}
	return nil
}

// All returns all known trusted keys, built-in ones first.
func All(st *store.Store) ([]Named, error) {
	var result []Named
	for _, pem := range PublishedKeys {
		k, err := signature.Parse([]byte(pem))
		if err != nil {
			panic("corrupt built-in key: " + err.Error())
		}
		result = append(result, Named{
			Key:     k,
			Prefix:  PublishedPrefix,
			BuiltIn: true,
		})
	}

	if st == nil {
		return result, nil
	}

	f, err := readTrust(st)
	if err != nil {
		return nil, err
	}

	for _, entry := range f.Keys {
		k, err := signature.Parse([]byte(entry.Key))
		if err != nil {
			return nil, fmt.Errorf("%s has a key for %s that isn't one: `zigsaw trust rm %s` removes it", trustPath(st), entry.Prefix, entry.Prefix)
		}
		result = append(result, Named{
			Key:     k,
			Prefix:  entry.Prefix,
			BuiltIn: false,
		})
	}

	return result, nil
}

// ForImage returns keys that count for repository (<host>/<repository>).
func ForImage(st *store.Store, repository string) ([]*signature.Key, error) {
	allKeys, err := All(st)
	if err != nil {
		return nil, err
	}
	var result []*signature.Key
	for _, named := range allKeys {
		if UnderPrefix(named.Prefix, repository) {
			result = append(result, named.Key)
		}
	}
	return result, nil
}

// ForRelease returns keys that count for a release of zigsaw.
func ForRelease(st *store.Store) ([]*signature.Key, error) {
	var result []*signature.Key
	if os.Getenv("ZIGSAW_UPGRADE_FROM") != "" {
		if keyFile := os.Getenv("ZIGSAW_UPGRADE_KEY"); keyFile != "" {
			data, err := os.ReadFile(keyFile)
			if err != nil {
				return nil, fmt.Errorf("reading the key %s: %w", keyFile, err)
			}
			k, err := signature.Parse(data)
			if err != nil {
				return nil, fmt.Errorf("%s isn't a P-256 public key", keyFile)
			}
			result = append(result, k)
		}
	}

	for _, pem := range PublishedKeys {
		k, err := signature.Parse([]byte(pem))
		if err != nil {
			panic("corrupt built-in key: " + err.Error())
		}
		result = append(result, k)
	}

	return result, nil
}

// IsPrefix reports whether prefix is a valid trust prefix.
func IsPrefix(prefix string) bool {
	if len(prefix) == 0 {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		if c <= ' ' || c == '@' || c == '\\' || c == '"' {
			return false
		}
	}
	return true
}

// Add adds a trusted key for prefix from keyFile.
func Add(st *store.Store, prefixArg, keyFile string) (*signature.Key, bool, error) {
	prefix := strings.TrimRight(prefixArg, "/")
	if !IsPrefix(prefix) {
		return nil, false, fmt.Errorf("%q isn't where images are: give a registry and path, like ghcr.io/me or localhost:5000", prefixArg)
	}

	pem, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", keyFile, err)
	}

	key, err := signature.Parse(pem)
	if err != nil {
		return nil, false, fmt.Errorf("%s isn't a P-256 public key, as `cosign generate-key-pair` makes in cosign.pub", keyFile)
	}

	f, err := readTrust(st)
	if err != nil {
		return nil, false, err
	}

	for _, entry := range f.Keys {
		if strings.EqualFold(entry.Prefix, prefix) {
			known, err := signature.Parse([]byte(entry.Key))
			if err != nil {
				continue
			}
			if known.Fingerprint == key.Fingerprint {
				return key, false, nil // already present
			}
		}
	}

	f.Keys = append(f.Keys, Entry{
		Prefix: prefix,
		Key:    string(pem),
	})

	if err := writeTrust(st, f.Keys); err != nil {
		return nil, false, err
	}

	return key, true, nil
}

// Remove removes trusted key(s) for prefix, optionally restricted to fingerprint.
func Remove(st *store.Store, prefixArg string, fingerprint *string) (int, error) {
	prefix := strings.TrimRight(prefixArg, "/")
	f, err := readTrust(st)
	if err != nil {
		return 0, err
	}

	var kept []Entry
	removed := 0

	for _, entry := range f.Keys {
		named := strings.EqualFold(entry.Prefix, prefix)
		same := true
		if fingerprint != nil {
			k, err := signature.Parse([]byte(entry.Key))
			if err == nil {
				same = (k.Fingerprint == *fingerprint)
			}
		}
		if named && same {
			removed++
		} else {
			kept = append(kept, entry)
		}
	}

	if removed == 0 {
		return 0, fmt.Errorf("no key was added for %s", prefix)
	}

	if err := writeTrust(st, kept); err != nil {
		return 0, err
	}

	return removed, nil
}
