package store

import (
	"strings"
	"testing"
)

func TestImportConcurrentNameConflict(t *testing.T) {
	st, err := Open(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := st.Lock(false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	first, second := catalogFixture(t, st, "1"), catalogFixture(t, st, "2")
	start := make(chan struct{})
	results := make(chan error, 2)
	name := "example.test/app:conflict"
	for _, ref := range []Ref{first, second} {
		go func() {
			<-start
			results <- st.ImportImages(ImageImport{
				Images: map[string]Ref{ref.Manifest: ref},
				Names:  map[string]string{name: ref.Manifest},
			}, false)
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case strings.Contains(err.Error(), "--force"):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	idx, err := st.ImageCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 || len(idx.Images) != 1 || len(idx.Names) != 1 || idx.Defaults[strings.ToLower(first.ID)] != idx.Names[name] {
		t.Fatalf("concurrent import was not atomic: successes=%d conflicts=%d catalog=%+v", successes, conflicts, idx)
	}
}
