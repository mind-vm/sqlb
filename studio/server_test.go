package studio

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// testManifest is a small, hand-built fixture rather than a file on disk, so
// this test exercises exactly the shapes handleRows/handleRowDetail read and
// nothing an unrelated schema change could shift underneath it.
func testManifest() *schema.Manifest {
	return &schema.Manifest{
		Version: "1",
		Module:  "widgets-app",
		Tables: []schema.TableManifest{
			{
				Name:       "widgets",
				Comment:    "A test widget.",
				PrimaryKey: "id",
				Columns: []schema.ColumnManifest{
					{Name: "id", Type: "uuid", GoType: "string", ReadOnly: true},
					{Name: "owner_id", Type: "uuid", GoType: "string", References: &schema.RefManifest{
						Relation: "owner", Table: "owners", Column: "id", Enforced: true,
					}},
					{Name: "title", Type: "text", GoType: "string", Capabilities: []string{"filter", "sort"}},
					{Name: "count", Type: "int", GoType: "int"},
					{Name: "status", Type: "enum", GoType: "string", Enum: []string{"draft", "published"}},
					{Name: "note", Type: "text", GoType: "string", Nullable: true},
					{Name: "tags", Type: "text", GoType: "[]string", Array: true},
				},
				REST: &schema.RESTManifest{
					Path:       "/widgets",
					Operations: []string{"create", "read", "update", "list"},
					Filterable: []string{"title", "count", "note"},
					Sortable:   []string{"title", "count"},
					Searchable: []string{"note"},
					Actions: []schema.ActionManifest{
						{
							Name:   "publish",
							Path:   "/widgets/{id}/publish",
							Method: "POST",
							Body:   []schema.ActionProperty{{Name: "note", Type: "text", Nullable: true}},
							Writes: []string{"status"},
						},
						{
							Name:   "purge",
							Path:   "/widgets/purge",
							Method: "POST",
						},
					},
				},
			},
			{
				Name:       "hidden",
				PrimaryKey: "id",
				Columns:    []schema.ColumnManifest{{Name: "id", Type: "uuid", GoType: "string"}},
				// No REST: not exposed, so it must never grow a "Browse data" link.
			},
		},
	}
}

// fakeAPI stands in for a running application's generated REST API: it
// checks the bearer token studio attached and serves a rest.Page[T]-shaped
// list, a bare-row detail, and an in-memory PATCH/POST — the response shapes
// and the request-body types client.go and form.go must produce.
func fakeAPI(t *testing.T, wantToken string) *httptest.Server {
	t.Helper()
	store := map[string]map[string]any{
		"w1": {"id": "w1", "owner_id": "o1", "title": "First widget", "count": float64(3), "status": "draft", "note": "hello", "tags": []any{"a", "b"}},
	}
	nextID := 2

	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /widgets", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		items := make([]map[string]any, 0, len(store))
		for _, id := range []string{"w1"} { // stable order for the assertions below
			if row, ok := store[id]; ok {
				items = append(items, row)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": items, "page": 1, "per_page": 20, "has_more": false,
		})
	})
	mux.HandleFunc("GET /widgets/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		row, ok := store[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(row)
	})
	mux.HandleFunc("PATCH /widgets/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		row, ok := store[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var changes map[string]any
		if err := json.NewDecoder(r.Body).Decode(&changes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if owner, present := changes["owner_id"]; present && owner == "missing-owner" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"detail":"owner does not exist"}`))
			return
		}
		if count, present := changes["count"]; present {
			if _, isNumber := count.(float64); !isNumber {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"detail":"count must be a number, not a string"}`))
				return
			}
		}
		if tags, present := changes["tags"]; present {
			if _, isArray := tags.([]any); !isArray {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"detail":"tags must be an array, not a string"}`))
				return
			}
		}
		for k, v := range changes {
			row[k] = v
		}
		_ = json.NewEncoder(w).Encode(row)
	})
	mux.HandleFunc("POST /widgets/{id}/publish", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		row, ok := store[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		row["status"] = "published"
		if note, ok := body["note"]; ok {
			row["note"] = note
		}
		_ = json.NewEncoder(w).Encode(row)
	})
	mux.HandleFunc("POST /widgets/purge", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"purged": true})
	})
	mux.HandleFunc("POST /widgets", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if title, _ := body["title"].(string); title == "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"detail":"title is required"}`))
			return
		}
		id := fmt.Sprintf("w%d", nextID)
		nextID++
		body["id"] = id
		store[id] = body
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(body)
	})
	return httptest.NewServer(mux)
}

func newTestClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestIndexListsExposedAndUnexposedTables(t *testing.T) {
	srv, err := NewServer(testManifest(), "")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "widgets") || !strings.Contains(string(body), "hidden") {
		t.Fatalf("index missing a table name:\n%s", body)
	}
	if !strings.Contains(string(body), "not exposed") {
		t.Fatalf("index did not mark the REST-less table as not exposed:\n%s", body)
	}
}

func TestRowsRedirectsToLoginWithoutAToken(t *testing.T) {
	api := fakeAPI(t, "irrelevant")
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := newTestClient(t)
	resp, err := client.Get(ts.URL + "/tables/widgets/rows")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location = %q, want a /login redirect", loc)
	}
}

func TestRowsRendersDataAfterLogin(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := newTestClient(t)
	client.CheckRedirect = nil // follow the post-login redirect this time

	loginResp, err := client.PostForm(ts.URL+"/login", map[string][]string{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()

	resp, err := client.Get(ts.URL + "/tables/widgets/rows")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "First widget") {
		t.Fatalf("grid missing the fake row's title:\n%s", body)
	}
	if !strings.Contains(string(body), `href="/tables/widgets/rows/w1"`) {
		t.Fatalf("grid's first cell did not link to the row by its primary key:\n%s", body)
	}

	detail, err := client.Get(ts.URL + "/tables/widgets/rows/w1")
	if err != nil {
		t.Fatal(err)
	}
	defer detail.Body.Close()
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d, want 200", detail.StatusCode)
	}
	detailBody, err := io.ReadAll(detail.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(detailBody), `href="/tables/owners/rows/o1"`) {
		t.Fatalf("detail page did not render owner_id as a link to the referenced table:\n%s", detailBody)
	}
}

func TestStaleTokenRedirectsBackToLogin(t *testing.T) {
	api := fakeAPI(t, "the-real-token")
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := newTestClient(t)
	loginResp, err := client.PostForm(ts.URL+"/login", map[string][]string{"token": {"wrong-token"}})
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()

	resp, err := client.Get(ts.URL + "/tables/widgets/rows")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 back to login on a 401 from the API", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location = %q, want a /login redirect", loc)
	}
}

// loggedInClient returns a cookie-carrying client already signed in with
// token. Redirects are NOT auto-followed (same default as newTestClient) —
// a caller that wants the 302 status of a subsequent POST needs to see it,
// not the 200 of wherever it points.
func loggedInClient(t *testing.T, tsURL, token string) *http.Client {
	t.Helper()
	client := newTestClient(t)
	resp, err := client.PostForm(tsURL+"/login", map[string][]string{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return client
}

func TestEditUpdatesRowEncodesNumbersAndClearsNullable(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	resp, err := client.PostForm(ts.URL+"/tables/widgets/rows/w1/edit", url.Values{
		"title":       {"First widget"},
		"count":       {"42"},
		"status":      {"published"},
		"note__clear": {"on"},
		"owner_id":    {"o1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("edit submit status = %d, want 302", resp.StatusCode)
	}

	detail, err := client.Get(ts.URL + "/tables/widgets/rows/w1")
	if err != nil {
		t.Fatal(err)
	}
	defer detail.Body.Close()
	body, err := io.ReadAll(detail.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	// "42" and not "42.000000" or a quoted string: proves scalarValue sent a
	// JSON number, which the fake API's PATCH handler type-checks.
	if !strings.Contains(got, "<dd class=\"col-9\">\n        42\n") {
		t.Fatalf("count did not round-trip as 42:\n%s", got)
	}
	if !strings.Contains(got, "published") {
		t.Fatalf("status did not update to published:\n%s", got)
	}
	if !strings.Contains(got, "<dd class=\"col-9\">\n        —\n") {
		t.Fatalf("note__clear did not clear note to null:\n%s", got)
	}
}

func TestItemActionInvokeUpdatesRowAndShowsResult(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	form, err := client.Get(ts.URL + "/tables/widgets/rows/w1/actions/publish")
	if err != nil {
		t.Fatal(err)
	}
	defer form.Body.Close()
	if form.StatusCode != http.StatusOK {
		t.Fatalf("action form status = %d, want 200", form.StatusCode)
	}
	formBody, err := io.ReadAll(form.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(formBody), `name="note"`) {
		t.Fatalf("action form missing its declared body field:\n%s", formBody)
	}

	resp, err := client.PostForm(ts.URL+"/tables/widgets/rows/w1/actions/publish", url.Values{"note": {"ready to ship"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("action submit status = %d, want 200 (result rendered inline)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Action succeeded") || !strings.Contains(string(body), "published") {
		t.Fatalf("action page did not show the succeeded response:\n%s", body)
	}

	detail, err := client.Get(ts.URL + "/tables/widgets/rows/w1")
	if err != nil {
		t.Fatal(err)
	}
	defer detail.Body.Close()
	detailBody, err := io.ReadAll(detail.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(detailBody), "published") {
		t.Fatalf("row's status was not actually persisted by the action:\n%s", detailBody)
	}
}

func TestCollectionActionInvoke(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	resp, err := client.PostForm(ts.URL+"/tables/widgets/actions/purge", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// html/template escapes the quotes in the pretty-printed JSON response.
	if !strings.Contains(string(body), `&#34;purged&#34;: true`) {
		t.Fatalf("collection action's response missing from the page:\n%s", body)
	}
}

func TestActionRouteShapeMismatchIs404(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	// purge is a collection action; reaching it through a row's URL is not
	// the route it declared.
	resp, err := client.Get(ts.URL + "/tables/widgets/rows/w1/actions/purge")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a collection action reached via a row URL", resp.StatusCode)
	}

	// publish is an item action; reaching it without a row id is not either.
	resp2, err := client.Get(ts.URL + "/tables/widgets/actions/publish")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an item action reached without a row id", resp2.StatusCode)
	}
}

func TestArrayColumnRoundTripsThroughEditAsCommaSeparated(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	form, err := client.Get(ts.URL + "/tables/widgets/rows/w1/edit")
	if err != nil {
		t.Fatal(err)
	}
	defer form.Body.Close()
	formBody, err := io.ReadAll(form.Body)
	if err != nil {
		t.Fatal(err)
	}
	// The stored value is ["a","b"]; the edit field must show it the way its
	// own submit path expects to parse it back, not as a JSON array literal.
	if !strings.Contains(string(formBody), `value="a, b"`) {
		t.Fatalf("edit form did not render tags as comma-separated text:\n%s", formBody)
	}

	resp, err := client.PostForm(ts.URL+"/tables/widgets/rows/w1/edit", url.Values{
		"title": {"First widget"},
		"tags":  {"a, b, c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("edit submit status = %d, want 302 (the fake API's array type-check should pass)", resp.StatusCode)
	}

	detail, err := client.Get(ts.URL + "/tables/widgets/rows/w1")
	if err != nil {
		t.Fatal(err)
	}
	defer detail.Body.Close()
	detailBody, err := io.ReadAll(detail.Body)
	if err != nil {
		t.Fatal(err)
	}
	// html/template escapes the quotes in the JSON array literal; this is
	// that escaped form, not an unescaped one this test should have expected.
	if !strings.Contains(string(detailBody), `[&#34;a&#34;,&#34;b&#34;,&#34;c&#34;]`) {
		t.Fatalf("tags did not round-trip as a 3-element array:\n%s", detailBody)
	}
}

func TestEditRejectedByAPIRedisplaysFormWithError(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	// A blank field is "no change" on an edit (parseFormBody's own contract),
	// so this has to trigger the API's validation with a value it actually
	// sends — count=99 alongside an owner_id the fake API's PATCH handler
	// rejects, the way a real FK violation would surface.
	resp, err := client.PostForm(ts.URL+"/tables/widgets/rows/w1/edit", url.Values{
		"title":    {"Still first widget"},
		"owner_id": {"missing-owner"},
		"count":    {"99"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (form redisplayed, not redirected)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "owner does not exist") {
		t.Fatalf("redisplayed form missing the API's error:\n%s", got)
	}
	if !strings.Contains(got, `value="99"`) {
		t.Fatalf("redisplayed form lost the operator's other input (count=99):\n%s", got)
	}
}

func TestEditWithUnparsableNumberRedisplaysFormWithoutCallingTheAPI(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	resp, err := client.PostForm(ts.URL+"/tables/widgets/rows/w1/edit", url.Values{
		"title": {"First widget"},
		"count": {"not-a-number"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (form redisplayed)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "not a number") {
		t.Fatalf("redisplayed form missing the local encoding error:\n%s", body)
	}
}

// TestBasePathMountsUnderAPrefix is the issue-217 case: mount Handler() on a
// real ServeMux at "/studio/" — the way rest.Server.Mux would, with no
// http.StripPrefix — and prove every link, redirect and asset reference
// carries the prefix, then drive a full sign-in/browse/edit round trip
// through the mount to prove it isn't just strings that look right in
// isolation. Run this against basePath="" mounted the same way (or the
// pre-#217 server) and it fails on the very first assertion — every href
// below is root-absolute rather than basePath-absolute, so the browser's own
// "/" is what a click resolves to, not "/studio/".
func TestBasePathMountsUnderAPrefix(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL, "/studio")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/studio/", srv.Handler())
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/studio/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `href="/studio/tables/widgets"`) {
		t.Fatalf("index page's table link is not prefixed with basePath:\n%s", got)
	}
	if strings.Contains(got, `href="/tables/widgets"`) {
		t.Fatalf("index page still has a bare, unprefixed table link:\n%s", got)
	}
	if !strings.Contains(got, `href="/studio/static/css/tabler.min.css"`) {
		t.Fatalf("index page's stylesheet reference is not prefixed with basePath:\n%s", got)
	}
	if !strings.Contains(got, `href="/studio/login"`) {
		t.Fatalf("index page's sign-in link is not prefixed with basePath:\n%s", got)
	}

	// The static asset itself resolves through the prefixed mount, not just
	// the href pointing at it.
	css, err := http.Get(ts.URL + "/studio/static/css/tabler.min.css")
	if err != nil {
		t.Fatal(err)
	}
	css.Body.Close()
	if css.StatusCode != http.StatusOK {
		t.Fatalf("static asset status = %d, want 200", css.StatusCode)
	}

	// An unauthenticated data page redirects to the prefixed login, not the
	// bare root one.
	client := newTestClient(t)
	loginRedirect, err := client.Get(ts.URL + "/studio/tables/widgets/rows")
	if err != nil {
		t.Fatal(err)
	}
	loginRedirect.Body.Close()
	if loginRedirect.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", loginRedirect.StatusCode)
	}
	if loc := loginRedirect.Header.Get("Location"); !strings.HasPrefix(loc, "/studio/login?next=") {
		t.Fatalf("Location = %q, want a /studio/login redirect", loc)
	}

	// End to end: sign in, follow the redirect to the grid, and confirm the
	// row link and a form submission both work mounted under the prefix.
	client.CheckRedirect = nil
	loginResp, err := client.PostForm(ts.URL+"/studio/login", url.Values{
		"token": {token},
		"next":  {"/studio/tables/widgets/rows"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("post-login page status = %d, want 200 (grid, after following the redirect)", loginResp.StatusCode)
	}
	gridBody, err := io.ReadAll(loginResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gridBody), "First widget") {
		t.Fatalf("grid after following the login redirect missing the fake row:\n%s", gridBody)
	}
	if !strings.Contains(string(gridBody), `href="/studio/tables/widgets/rows/w1"`) {
		t.Fatalf("grid's row link is not prefixed with basePath:\n%s", gridBody)
	}

	editResp, err := client.PostForm(ts.URL+"/studio/tables/widgets/rows/w1/edit", url.Values{
		"title": {"First widget"},
		"count": {"5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer editResp.Body.Close()
	if editResp.StatusCode != http.StatusOK {
		t.Fatalf("edit submit (after following its redirect) status = %d, want 200", editResp.StatusCode)
	}
	editBody, err := io.ReadAll(editResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(editBody), "<dd class=\"col-9\">\n        5\n") {
		t.Fatalf("edit submitted through the prefixed mount did not persist:\n%s", editBody)
	}
}

func TestNewRowCreatesAndRedirectsToItsDetailPage(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	resp, err := client.PostForm(ts.URL+"/tables/widgets/rows/new", url.Values{
		"title":    {"Brand new widget"},
		"owner_id": {"o1"},
		"count":    {"7"},
		"status":   {"draft"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("create submit status = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/tables/widgets/rows/w") {
		t.Fatalf("Location = %q, want a redirect to the new row's detail page", loc)
	}

	detail, err := client.Get(ts.URL + loc)
	if err != nil {
		t.Fatal(err)
	}
	defer detail.Body.Close()
	body, err := io.ReadAll(detail.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Brand new widget") {
		t.Fatalf("new row's detail page missing its own title:\n%s", body)
	}
}

// queryCapturingWidgetsAPI is a minimal stand-in for fakeAPI's own /widgets
// route: it serves the same empty rest.Page[T] shape, but records the raw
// query string /widgets was called with, which is what the tests below need
// to assert on and fakeAPI has no way to report back through client.List's
// decoded response.
func queryCapturingWidgetsAPI(t *testing.T, wantToken string) (*httptest.Server, *string) {
	t.Helper()
	var lastQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /widgets", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		lastQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{}, "page": 1, "per_page": 20, "has_more": false,
		})
	})
	return httptest.NewServer(mux), &lastQuery
}

func TestRowsForwardsFilterSortSearch(t *testing.T) {
	const token = "secret-token"
	api, lastQuery := queryCapturingWidgetsAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	resp, err := client.Get(ts.URL + "/tables/widgets/rows?" +
		"f_title_op=ilike&f_title_val=widget&" +
		"f_count_op=gte&f_count_val=3&" +
		"sort=-count&search=hello")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got, err := url.ParseQuery(*lastQuery)
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{
		"title":  {"ilike.widget"},
		"count":  {"gte.3"},
		"sort":   {"-count"},
		"search": {"hello"},
		"page":   {"1"},
	}
	for k, v := range want {
		if got.Get(k) != v[0] {
			t.Errorf("query param %q = %q, want %q (full query: %s)", k, got.Get(k), v[0], *lastQuery)
		}
	}
}

func TestRowsOmitsBlankFilterValues(t *testing.T) {
	const token = "secret-token"
	api, lastQuery := queryCapturingWidgetsAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	resp, err := client.Get(ts.URL + "/tables/widgets/rows?f_title_op=eq&f_title_val=")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got, err := url.ParseQuery(*lastQuery)
	if err != nil {
		t.Fatal(err)
	}
	if got.Has("title") {
		t.Errorf("query carries %q for a blank filter value, want it omitted (full query: %s)", got.Get("title"), *lastQuery)
	}
}

func TestPageURLPreservesFilters(t *testing.T) {
	r := httptest.NewRequest("GET", "/tables/widgets/rows?f_title_op=ilike&f_title_val=widget&page=1", nil)
	got := pageURL(r, 2)
	q, err := url.ParseQuery(strings.TrimPrefix(got, "?"))
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("page") != "2" {
		t.Errorf("pageURL page = %q, want 2", q.Get("page"))
	}
	if q.Get("f_title_val") != "widget" {
		t.Errorf("pageURL dropped the applied filter: %s", got)
	}
}

func TestOperatorsForColumn(t *testing.T) {
	tests := []struct {
		name string
		col  schema.ColumnManifest
		want []string
	}{
		{"text", schema.ColumnManifest{Type: "text"}, []string{"eq", "ne", "in", "nin", "contains", "startswith", "endswith", "like", "ilike"}},
		{"ordered", schema.ColumnManifest{Type: "int"}, []string{"eq", "ne", "in", "nin", "gt", "gte", "lt", "lte", "between"}},
		{"nullable ordered", schema.ColumnManifest{Type: "int", Nullable: true}, []string{"eq", "ne", "in", "nin", "gt", "gte", "lt", "lte", "between", "isnull", "notnull"}},
		{"enum", schema.ColumnManifest{Type: "enum", Enum: []string{"a", "b"}}, []string{"eq", "ne", "in", "nin"}},
		{"uuid", schema.ColumnManifest{Type: "uuid"}, []string{"eq", "ne", "in", "nin"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := operatorsFor(tt.col)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("operatorsFor(%+v) = %v, want %v", tt.col, got, tt.want)
			}
		})
	}
}
