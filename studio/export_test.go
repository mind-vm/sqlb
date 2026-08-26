package studio

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// pagedWidgetsAPI serves /widgets across two pages, so export's page-looping
// (handleRowsExport) can be told apart from a handler that only reads page
// one. Two rows per page, has_more true on page 1 only.
func pagedWidgetsAPI(t *testing.T, wantToken string) *httptest.Server {
	t.Helper()
	pages := map[string][]map[string]any{
		"1": {
			{"id": "w1", "title": "First widget", "count": float64(3), "status": "draft", "note": "hello", "tags": []any{"a", "b"}},
		},
		"2": {
			{"id": "w2", "title": "Second widget", "count": float64(5), "status": "published", "note": nil, "tags": []any{}},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /widgets", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		pageNum, _ := strconv.Atoi(page)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": pages[page], "page": pageNum, "per_page": 1, "has_more": page == "1",
		})
	})
	return httptest.NewServer(mux)
}

func TestExportJSONAccumulatesEveryPage(t *testing.T) {
	const token = "secret-token"
	api := pagedWidgetsAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	resp, err := client.Get(ts.URL + "/tables/widgets/rows/export?format=json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="widgets.json"`) {
		t.Errorf("Content-Disposition = %q, want a widgets.json filename", cd)
	}
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (one from each page)", len(rows))
	}
	if rows[0]["id"] != "w1" || rows[1]["id"] != "w2" {
		t.Errorf("rows = %+v, want w1 then w2", rows)
	}
}

func TestExportCSVHeaderAndCells(t *testing.T) {
	const token = "secret-token"
	api := pagedWidgetsAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	resp, err := client.Get(ts.URL + "/tables/widgets/rows/export?format=csv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 3 { // header + 2 rows
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), body)
	}
	if lines[0] != "id,owner_id,title,count,status,note,tags" {
		t.Errorf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "First widget") || !strings.Contains(lines[1], "a, b") {
		t.Errorf("row 1 = %q, want the first widget with its comma-joined tags", lines[1])
	}
}

func TestExportSQLQuotesAndOmitsReadOnlyColumns(t *testing.T) {
	const token = "secret-token"
	api := pagedWidgetsAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	resp, err := client.Get(ts.URL + "/tables/widgets/rows/export?format=sql")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"id"`) {
		t.Errorf("SQL export named the read-only id column, want it omitted:\n%s", body)
	}
	if !strings.Contains(string(body), `INSERT INTO "widgets"`) {
		t.Errorf("SQL export missing an INSERT INTO \"widgets\":\n%s", body)
	}
	if !strings.Contains(string(body), `'First widget'`) {
		t.Errorf("SQL export missing a quoted string literal:\n%s", body)
	}
}

func TestExportRejectsUnknownFormat(t *testing.T) {
	const token = "secret-token"
	api := pagedWidgetsAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	resp, err := client.Get(ts.URL + "/tables/widgets/rows/export?format=xml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
