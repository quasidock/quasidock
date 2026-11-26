package cli

import (
	"testing"

	"github.com/quasidock/quasidock/internal/context"
)

func TestParseUpdateOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--cpus=.5", "--memory", "1g", "--pids-limit=-1", "one", "--restart", "on-failure:3", "two"},
		{"-m1g", "--cpus", ".5", "--pids-limit", "-1", "--restart=on-failure:3", "--", "one", "two"},
	} {
		o, targets, err := parseUpdateOptions(args)
		if err != nil || len(targets) != 2 || *o.NanoCPUs != 500_000_000 || *o.Memory != 1<<30 || *o.PidsLimit != 0 || o.RestartPolicy.MaximumRetryCount != 3 {
			t.Fatalf("parse: %+v %v %v", o, targets, err)
		}
	}
	for _, args := range [][]string{{"one"}, {"--memory", "1g"}, {"--memory"}, {"--memory=-1", "one"}, {"--cpus=NaN", "one"}, {"--restart=always:2", "one"}, {"--pids-limit=-2", "one"}, {"--memory-swap=1g", "one"}, {"--memory=1g", ""}} {
		if _, _, err := parseUpdateOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	ctx := context.New()
	for _, create := range []bool{false, true} {
		args := []string{"--cpus=.5", "-m", "256m", "--pids-limit=4", "--restart=unless-stopped", "app"}
		o, err := parseRunOptions(ctx, args)
		if create {
			o, err = parseCreateOptions(ctx, args)
		}
		if err != nil || o.Resources.Memory != 256<<20 || o.RestartPolicy.Name != "unless-stopped" {
			t.Fatalf("run/create: %+v %v", o, err)
		}
		args = append([]string{"--rm"}, args...)
		if create {
			_, err = parseCreateOptions(ctx, args)
		} else {
			_, err = parseRunOptions(ctx, args)
		}
		if err == nil {
			t.Fatal("accepted --rm with restart")
		}
	}
}
