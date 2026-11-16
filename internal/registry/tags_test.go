package registry

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListTagsPaginationAndOrigin(t *testing.T) {
	for _, hostile := range []bool{false, true} {
		t.Run(fmt.Sprint(hostile), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("last") == "1" {
					fmt.Fprint(w, `{"tags":["24.2.0"]}`)
					return
				}
				link := `</v2/node/tags/list?last=1>; rel="next"`
				if hostile {
					link = `<https://example.com/v2/node/tags/list>; rel="next"`
				}
				w.Header().Set("Link", link)
				fmt.Fprint(w, `{"tags":["24.1.0"]}`)
			}))
			defer server.Close()
			ref, err := ParseReference(strings.TrimPrefix(server.URL, "http://") + "/node")
			if err != nil {
				t.Fatal(err)
			}
			r, err := New(ref, "pull", false)
			if err != nil {
				t.Fatal(err)
			}
			tags, err := r.ListTags()
			if hostile {
				if err == nil {
					t.Fatal("accepted cross-origin link")
				}
			} else if err != nil || len(tags) != 2 || tags[1] != "24.2.0" {
				t.Fatalf("%v %v", tags, err)
			}
		})
	}
}
