package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ListTags follows registry pagination without forwarding credentials off-origin.
func (r *Registry) ListTags() ([]string, error) {
	next := r.URL("/v2/%s/tags/list?n=100", r.Ref.Repository)
	seen := map[string]bool{}
	var tags []string
	for next != "" {
		if seen[next] {
			return nil, fmt.Errorf("registry repeated a tags page")
		}
		seen[next] = true
		res, err := r.send(request{Method: "GET", URL: next})
		if err != nil {
			return nil, err
		}
		if res.Status != http.StatusOK {
			return nil, r.failStatus(res, "listing image versions")
		}
		var page struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(res.Body, &page); err != nil {
			return nil, err
		}
		tags = append(tags, page.Tags...)
		link := res.Link
		previous := next
		next = ""
		if link == "" {
			continue
		}
		for _, item := range strings.Split(link, ",") {
			if !strings.Contains(item, `rel="next"`) && !strings.Contains(item, "rel=next") {
				continue
			}
			start, end := strings.Index(item, "<"), strings.Index(item, ">")
			if start < 0 || end <= start {
				return nil, fmt.Errorf("invalid registry pagination link")
			}
			next, err = resolveURL(previous, item[start+1:end])
			if err != nil {
				return nil, err
			}
			u, err := url.Parse(next)
			if err != nil {
				return nil, err
			}
			origin, _ := url.Parse(r.Ref.BaseURL())
			if u.Scheme != origin.Scheme || u.Host != origin.Host || u.Path != fmt.Sprintf("/v2/%s/tags/list", r.Ref.Repository) {
				return nil, fmt.Errorf("registry tags link leaves its repository")
			}
		}
	}
	return tags, nil
}
