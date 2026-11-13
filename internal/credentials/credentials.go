package credentials

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/zigsaw-io/zigsaw/internal/win32"
)

const (
	UsernameVar     = "ZIGSAW_REGISTRY_USERNAME"
	PasswordVar     = "ZIGSAW_REGISTRY_PASSWORD"
	DefaultRegistry = "ghcr.io"
)

var (
	ErrInvalidHost = errors.New("invalid registry host")
)

type Source int

const (
	SourceEnvironment Source = iota
	SourceCredentialManager
	SourceDockerConfig
	SourceDockerHelper
)

type Credential struct {
	User   string
	Secret string
	Source Source
	Helper string
}

func (c Credential) Origin() string {
	switch c.Source {
	case SourceEnvironment:
		return "in " + UsernameVar + " and " + PasswordVar
	case SourceCredentialManager:
		return "saved by `zigsaw login`"
	case SourceDockerConfig:
		return "in Docker's config.json"
	case SourceDockerHelper:
		return c.Helper
	default:
		return "unknown"
	}
}

func (c Credential) IsDocker() bool {
	return c.Source == SourceDockerConfig || c.Source == SourceDockerHelper
}

// NormalizeHost normalizes a registry host string (lowercase, no scheme/path, folding Docker Hub aliases to docker.io).
func NormalizeHost(text string) (string, error) {
	host := BareHost(text)
	if !IsValidHost(host) {
		return "", ErrInvalidHost
	}
	return strings.ToLower(host), nil
}

func BareHost(text string) string {
	host := text
	if idx := strings.Index(host, "://"); idx != -1 {
		host = host[idx+3:]
	}
	if idx := strings.IndexByte(host, '/'); idx != -1 {
		host = host[:idx]
	}
	lower := strings.ToLower(host)
	if lower == "index.docker.io" || lower == "registry-1.docker.io" {
		return "docker.io"
	}
	return host
}

// EnvAppliesTo checks whether credentials from the environment go to host.
func EnvAppliesTo(defaultRegistry, host string) bool {
	return strings.EqualFold(BareHost(defaultRegistry), BareHost(host))
}

// EnvHostOtherThan returns the bare host from defaultRegistry if env credentials exist
// but don't apply to the given host.
func EnvHostOtherThan(defaultRegistry, host string) string {
	if EnvAppliesTo(defaultRegistry, host) {
		return ""
	}
	return BareHost(defaultRegistry)
}

func IsValidHost(host string) bool {
	if len(host) == 0 {
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
		case c == '.' || c == '-' || c == '_' || c == ':' || c == '[' || c == ']':
		default:
			return false
		}
	}
	return true
}

func Lookup(host string) (*Credential, error) {
	normHost, err := NormalizeHost(host)
	if err != nil {
		return nil, err
	}

	// 1. Environment variables
	u := os.Getenv(UsernameVar)
	p := os.Getenv(PasswordVar)
	if u != "" && p != "" {
		defReg := os.Getenv("ZIGSAW_REGISTRY")
		if defReg == "" {
			defReg = DefaultRegistry
		}
		if strings.EqualFold(BareHost(defReg), BareHost(normHost)) {
			return &Credential{
				User:   u,
				Secret: p,
				Source: SourceEnvironment,
			}, nil
		}
	}

	// 2. Windows Credential Manager
	if c, err := Read(normHost); err == nil && c != nil {
		return c, nil
	}

	// 3. Docker config / helper
	return fromDocker(normHost)
}

func targetName(host string) string {
	return "zigsaw:" + host
}

// Read reads a saved credential from Windows Credential Manager.
func Read(host string) (*Credential, error) {
	cred, err := win32.CredRead(targetName(host), win32.CRED_TYPE_GENERIC)
	if err != nil {
		if errors.Is(err, syscall.Errno(win32.ERROR_NOT_FOUND)) {
			return nil, nil
		}
		return nil, err
	}
	defer win32.CredFree(uintptr(unsafe.Pointer(cred)))

	var user string
	if cred.UserName != nil {
		user = win32.UTF16PtrToString(cred.UserName)
	}

	var secret string
	if cred.CredentialBlob != nil && cred.CredentialBlobSize > 0 {
		b := make([]byte, cred.CredentialBlobSize)
		copy(b, unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize))
		secret = string(b)
	}

	return &Credential{
		User:   user,
		Secret: secret,
		Source: SourceCredentialManager,
	}, nil
}

// Save saves credentials in Windows Credential Manager.
func Save(host, user, secret string) error {
	if len(secret) > win32.CRED_MAX_CREDENTIAL_BLOB_SIZE {
		return fmt.Errorf("the password is %d bytes; Windows Credential Manager holds at most %d", len(secret), win32.CRED_MAX_CREDENTIAL_BLOB_SIZE)
	}

	tgtU16 := win32.UTF16Ptr(targetName(host))
	usrU16 := win32.UTF16Ptr(user)
	cmtU16 := win32.UTF16Ptr("Registry login saved by zigsaw login")

	var secretPtr *byte
	if len(secret) > 0 {
		secretBytes := []byte(secret)
		secretPtr = &secretBytes[0]
	}

	c := &win32.CREDENTIALW{
		Type:               win32.CRED_TYPE_GENERIC,
		TargetName:         tgtU16,
		Comment:            cmtU16,
		CredentialBlobSize: win32.DWORD(len(secret)),
		CredentialBlob:     secretPtr,
		Persist:            win32.CRED_PERSIST_LOCAL_MACHINE,
		UserName:           usrU16,
	}

	return win32.CredWrite(c, 0)
}

// Remove deletes credentials for host from Windows Credential Manager.
func Remove(host string) (bool, error) {
	err := win32.CredDelete(targetName(host), win32.CRED_TYPE_GENERIC, 0)
	if err != nil {
		if errors.Is(err, syscall.Errno(win32.ERROR_NOT_FOUND)) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Docker Config Parser

type dockerConfig struct {
	Auths       map[string]dockerAuth `json:"auths"`
	CredsStore  string                `json:"credsStore"`
	CredHelpers map[string]string     `json:"credHelpers"`
}

type dockerAuth struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

func fromDocker(host string) (*Credential, error) {
	configDir := os.Getenv("DOCKER_CONFIG")
	if configDir == "" {
		userProfile := os.Getenv("USERPROFILE")
		if userProfile == "" {
			return nil, nil
		}
		configDir = filepath.Join(userProfile, ".docker")
	}
	configPath := filepath.Join(configDir, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil
	}

	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil
	}

	serverURL := host
	if host == "docker.io" {
		serverURL = "https://index.docker.io/v1/"
	}

	var helper string
	if cfg.CredHelpers != nil {
		for key, name := range cfg.CredHelpers {
			h, err := NormalizeHost(key)
			if err == nil && h == host && name != "" {
				helper = name
				serverURL = key
				break
			}
		}
	}
	if helper == "" && cfg.CredsStore != "" {
		helper = cfg.CredsStore
	}

	if helper != "" {
		c, err := fromHelper(helper, serverURL)
		if err == nil && c != nil {
			return c, nil
		}
	}

	// Look in auths
	if cfg.Auths != nil {
		for key, a := range cfg.Auths {
			h, err := NormalizeHost(key)
			if err == nil && h == host {
				if a.Auth != "" {
					decoded, err := base64.StdEncoding.DecodeString(a.Auth)
					if err == nil {
						colon := bytes.IndexByte(decoded, ':')
						if colon > 0 {
							return &Credential{
								User:   string(decoded[:colon]),
								Secret: string(decoded[colon+1:]),
								Source: SourceDockerConfig,
							}, nil
						}
					}
				}
				if a.Username != "" && a.Password != "" {
					return &Credential{
						User:   a.Username,
						Secret: a.Password,
						Source: SourceDockerConfig,
					}, nil
				}
			}
		}
	}

	return nil, nil
}

func fromHelper(helper, serverURL string) (*Credential, error) {
	prog := "docker-credential-" + helper
	path, err := exec.LookPath(prog)
	if err != nil {
		return nil, nil
	}

	cmd := exec.Command(path, "get")
	cmd.Stdin = strings.NewReader(serverURL)
	var out bytes.Buffer
	cmd.Stdout = &out

	timer := time.AfterFunc(30*time.Second, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	defer timer.Stop()

	if err := cmd.Run(); err != nil {
		return nil, nil
	}

	var res struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return nil, nil
	}
	if res.Username == "" || res.Username == "<token>" {
		return nil, nil
	}

	return &Credential{
		User:   res.Username,
		Secret: res.Secret,
		Source: SourceDockerHelper,
		Helper: "from " + prog,
	}, nil
}
