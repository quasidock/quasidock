package prompt_test

import (
	"testing"

	"github.com/quasidock/quasidock/internal/prompt"
)

func TestParseAnswer(t *testing.T) {
	cases := []struct {
		input string
		want  *prompt.Answer
	}{
		{"", ptr(prompt.AnswerYes)},
		{"Y\r", ptr(prompt.AnswerYes)},
		{" yes ", ptr(prompt.AnswerYes)},
		{"n", ptr(prompt.AnswerNo)},
		{"NO", ptr(prompt.AnswerNo)},
		{"maybe", nil},
		{"   ", ptr(prompt.AnswerYes)},
	}

	for _, c := range cases {
		got := prompt.ParseAnswer(c.input)
		if c.want == nil {
			if got != nil {
				t.Errorf("ParseAnswer(%q) = %v, want nil", c.input, *got)
			}
		} else {
			if got == nil || *got != *c.want {
				t.Errorf("ParseAnswer(%q) = %v, want %v", c.input, got, *c.want)
			}
		}
	}
}

func ptr[T any](v T) *T {
	return &v
}
