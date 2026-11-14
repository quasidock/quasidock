package trust

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/quasidock/quasidock/internal/signature"
	"github.com/quasidock/quasidock/internal/store"
)

const QuasidockPrefix = "ghcr.io/quasidock/quasidock"

// QuasidockPublicKey is populated during the documented signing-key setup.
// An empty file disables built-in trust; CI refuses to publish.
//
//go:embed quasidock.pub
var QuasidockPublicKey string

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
		return nil, fmt.Errorf("%s isn't what quasidock wrote: fix or delete it: %w", p, err)
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
	if strings.TrimSpace(QuasidockPublicKey) != "" {
		key, err := signature.Parse([]byte(QuasidockPublicKey))
		if err != nil {
			return nil, fmt.Errorf("invalid bundled Quasidock key: %w", err)
		}
		result = append(result, Named{Key: key, Prefix: QuasidockPrefix, BuiltIn: true})
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
			return nil, fmt.Errorf("%s has a key for %s that isn't one: `quasidock trust rm %s` removes it", trustPath(st), entry.Prefix, entry.Prefix)
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
