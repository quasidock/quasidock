package run

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/quasidock/quasidock/internal/win32"
)

func resolveContainerUser(account string) (string, error) {
	if account == "" {
		return "", nil
	}
	var sid *syscall.SID
	if strings.HasPrefix(strings.ToUpper(account), "S-") {
		sid, _ = syscall.StringToSid(account)
	}
	if sid == nil {
		var err error
		sid, _, _, err = syscall.LookupSID("", account)
		if err != nil {
			return "", fmt.Errorf("--user: resolving Windows account %q: %w", account, err)
		}
	}
	want, err := sid.String()
	if err != nil {
		return "", fmt.Errorf("--user: converting Windows SID: %w", err)
	}
	if err := verifyContainerUser(0, want); err != nil {
		return "", err
	}
	return want, nil
}

func verifyContainerUser(handle win32.HANDLE, want string) error {
	if want == "" {
		return nil
	}
	token := syscall.Token(handle)
	if token == 0 {
		var err error
		token, err = syscall.OpenCurrentProcessToken()
		if err != nil {
			return fmt.Errorf("--user: opening Windows process token: %w", err)
		}
		defer token.Close()
	}
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("--user: reading Windows token user: %w", err)
	}
	actual, err := user.User.Sid.String()
	if err != nil {
		return fmt.Errorf("--user: converting Windows token SID: %w", err)
	}
	if actual != want {
		return fmt.Errorf("--user requires the current Windows account: requested SID %s, token has %s", want, actual)
	}
	return nil
}
