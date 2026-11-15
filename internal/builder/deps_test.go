package builder

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/remote"
	"github.com/quasidock/quasidock/internal/store"
)

func TestUnpinnedDependencySuggestsJSONReference(t *testing.T) {
	st, err := store.Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	name, err := store.ImageName("", "tool:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterImage(store.Ref{ID: "tool", Version: "1", Manifest: digest, Source: name}, name, true); err != nil {
		t.Fatal(err)
	}
	ctx := &context.Context{Store: st}
	for _, kind := range []DependencyKind{DependencyRuntime, DependencySDK, DependencySource} {
		t.Run(string(kind), func(t *testing.T) {
			target, err := remote.Resolve("", "tool:1")
			if err != nil {
				t.Fatal(err)
			}
			message := failUnpinned(ctx, "recipe.json", kind, "alias", "tool:1", target).Error()
			_, suggestion, ok := strings.Cut(message, "\n  ")
			if !ok {
				t.Fatal("no pin suggestion:", message)
			}
			var pin map[string]string
			if err := json.Unmarshal([]byte("{"+suggestion+"}"), &pin); err != nil {
				t.Fatalf("suggested pin is not valid JSON: %s: %v", suggestion, err)
			}
			key := "alias"
			if kind == DependencySource {
				key = "image"
			}
			if pin[key] != "tool:1@"+digest {
				t.Fatal("wrong pin suggestion:", pin)
			}
		})
	}
}
