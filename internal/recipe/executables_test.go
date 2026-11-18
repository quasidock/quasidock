package recipe

import (
	"strings"
	"testing"
)

func TestExecutableMetadataValidation(t *testing.T) {
	for _, scope := range []string{"root", "unit"} {
		for _, tc := range []struct {
			paths []string
			valid bool
		}{
			{[]string{"out/server", "bin/helper.exe"}, true},
			{[]string{"."}, false},
			{[]string{"/server"}, false},
			{[]string{"../server"}, false},
			{[]string{"out/*.exe"}, false},
			{[]string{"server", "SERVER"}, false},
			{[]string{"server", "server.exe"}, false},
			{[]string{"server", "server/child"}, false},
			{[]string{"server", "server.exe/child"}, false},
		} {
			t.Run(scope+"="+strings.Join(tc.paths, ","), func(t *testing.T) {
				r := buildRecipe()
				if scope == "root" {
					r.Executables = tc.paths
				} else {
					r.Builds[0].Executables = tc.paths
				}
				_, err := parseBuildRecipe(r)
				if (err == nil) != tc.valid {
					t.Fatalf("valid=%t: %v", tc.valid, err)
				}
			})
		}
	}
}
