package run

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/quasidock/quasidock/internal/win32"
)

// testMitigation reads the experimental process mitigation hooks applied to a
// run's main process:
//
//	QUASIDOCK_TEST_MITIGATION=<policy>[,<policy2>]   PROCESS_CREATION_MITIGATION_POLICY(2) bits
//	QUASIDOCK_TEST_CHILD_POLICY=<flags>              PROCESS_CREATION_CHILD_PROCESS_* flags
//
// Values are parsed with strconv's base prefixes (0x..., 0b..., decimal).
func testMitigation() ([2]uint64, win32.DWORD, error) {
	var policy [2]uint64
	if val := os.Getenv("QUASIDOCK_TEST_MITIGATION"); val != "" {
		words := strings.Split(val, ",")
		if len(words) > 2 {
			return policy, 0, fmt.Errorf("QUASIDOCK_TEST_MITIGATION: want at most two words, got %q", val)
		}
		for i, word := range words {
			n, err := strconv.ParseUint(strings.TrimSpace(word), 0, 64)
			if err != nil {
				return policy, 0, fmt.Errorf("QUASIDOCK_TEST_MITIGATION: %w", err)
			}
			policy[i] = n
		}
	}
	var child win32.DWORD
	if val := os.Getenv("QUASIDOCK_TEST_CHILD_POLICY"); val != "" {
		n, err := strconv.ParseUint(strings.TrimSpace(val), 0, 32)
		if err != nil {
			return policy, 0, fmt.Errorf("QUASIDOCK_TEST_CHILD_POLICY: %w", err)
		}
		child = win32.DWORD(n)
	}
	return policy, child, nil
}
