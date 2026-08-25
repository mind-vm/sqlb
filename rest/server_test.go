package rest_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mind-vm/sqlb/rest"
)

// TestNewServerMountsAndServes is the batteries-included path: NewServer builds
// the huma API and the mux, Resource mounts onto it, and the resource, the
// OpenAPI document and the docs page are all reachable over HTTP without the
// caller wiring a router or an adapter.
func TestNewServerMountsAndServes(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})

	srv := rest.NewServer(rest.Config{
		Title:       "Test",
		Version:     "1.2.3",
		Description: "A server built by NewServer.",
	})
	if err := rest.Resource[Post, PostCreate, PostUpdate](srv.API, db.db, postOptions()); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}

	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	t.Run("the resource is reachable", func(t *testing.T) {
		body := get(t, ts, "/posts")
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("decoding list body: %v\n%s", err, body)
		}
		if len(page.Items) != 1 {
			t.Fatalf("want 1 item, got %d: %s", len(page.Items), body)
		}
	})

	t.Run("the document is served and names the resource", func(t *testing.T) {
		body := get(t, ts, "/openapi.json")
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("the document is not valid JSON: %v", err)
		}
		paths, _ := doc["paths"].(map[string]any)
		if _, ok := paths["/posts"]; !ok {
			t.Errorf("the document does not describe /posts: %s", body)
		}
		info, _ := doc["info"].(map[string]any)
		if info["title"] != "Test" || info["version"] != "1.2.3" {
			t.Errorf("the document's info does not carry the config: %v", info)
		}
	})

	t.Run("the docs page is served", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/docs")
		if err != nil {
			t.Fatalf("GET /docs: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /docs: status %d", resp.StatusCode)
		}
	})
}

// TestNewServerDefaultsAndCustomize covers the zero-ish config and the escape
// hatch: Title/Version default, and Customize sees the huma.Config so a security
// scheme set there reaches the document.
func TestNewServerDefaultsAndCustomize(t *testing.T) {
	srv := rest.NewServer(rest.Config{
		Customize: func(c *huma.Config) {
			c.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
				"bearer": {Type: "http", Scheme: "bearer"},
			}
		},
	})

	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	body := get(t, ts, "/openapi.json")
	if !strings.Contains(string(body), "\"bearer\"") {
		t.Errorf("Customize did not reach the document: %s", body)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("document not valid JSON: %v", err)
	}
	if info, _ := doc["info"].(map[string]any); info["title"] != "API" || info["version"] != "1.0.0" {
		t.Errorf("defaults not applied: %v", info)
	}
}

func get(t *testing.T, ts *httptest.Server, path string) []byte {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d\n%s", path, resp.StatusCode, body)
	}
	return body
}
