package context

import (
	"testing"
)

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		ms       uint64
		expected string
	}{
		{0, "0 ms"},
		{850, "850 ms"},
		{1000, "1.0 s"},
		{12345, "12.3 s"},
		{59999, "59.9 s"},
		{60000, "1m 00s"},
		{247000, "4m 07s"},
		{3599000, "59m 59s"},
		{3780000, "1h 03m"},
	}

	for _, c := range cases {
		got := FormatDuration(c.ms)
		if got != c.expected {
			t.Errorf("FormatDuration(%d) = %q; want %q", c.ms, got, c.expected)
		}
	}
}
