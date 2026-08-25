package studio

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jryannel/sqlb/schema"
)

// createInputManifest is issue #309's case as a manifest: a resource whose
// create body carries one property that is not a column. The column it feeds —
// a bcrypt digest — is Hidden, so it is absent from Columns entirely, which is
// exactly why a form built from the columns alone cannot be completed.
func createInputManifest() *schema.Manifest {
	return &schema.Manifest{
		Version: "1",
		Module:  "school-app",
		Tables: []schema.TableManifest{
			{
				Name:       "children",
				Comment:    "A child, who signs in with a PIN.",
				PrimaryKey: "id",
				Columns: []schema.ColumnManifest{
					{Name: "id", Type: "uuid", GoType: "string", ReadOnly: true},
					{Name: "name", Type: "text", GoType: "string"},
					{Name: "age", Type: "int", GoType: "int"},
				},
				REST: &schema.RESTManifest{
					Path:       "/children",
					Operations: []string{"create", "read", "update", "list"},
					CreateInput: []schema.BodyProperty{
						{Name: "pin", Type: "varchar"},
					},
				},
			},
		},
	}
}

// childrenAPI stands in for the generated create: it refuses a body without
// the declared property the way the real one does — Huma rejects a required
// property with a 422 naming it — and records what it was sent, so the test
// can assert the key studio spelled rather than only that the call succeeded.
func childrenAPI(t *testing.T, token string) (*httptest.Server, func() map[string]any) {
	t.Helper()
	var (
		mu   sync.Mutex
		last map[string]any
	)
	store := map[string]map[string]any{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /children", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// A copy: the handler goes on to mutate body, and last is what studio
		// sent rather than what this fixture made of it.
		mu.Lock()
		last = make(map[string]any, len(body))
		for k, v := range body {
			last[k] = v
		}
		mu.Unlock()
		if pin, _ := body["pin"].(string); pin == "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"detail":"expected required property pin to be present"}`))
			return
		}
		body["id"] = "c1"
		delete(body, "pin") // a create input is absent from every response
		store["c1"] = body
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("GET /children/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		row, ok := store[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(row)
	})

	ts := httptest.NewServer(mux)
	return ts, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func TestCreateFormOffersADeclaredInputAndSendsIt(t *testing.T) {
	const token = "secret-token"
	api, lastBody := childrenAPI(t, token)
	defer api.Close()
	srv, err := NewServer(createInputManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	form, err := client.Get(ts.URL + "/tables/children/rows/new")
	if err != nil {
		t.Fatal(err)
	}
	defer form.Body.Close()
	page, err := io.ReadAll(form.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), `name="pin"`) {
		t.Fatalf("create form does not offer the declared property, so the POST it builds is refused:\n%s", page)
	}

	resp, err := client.PostForm(ts.URL+"/tables/children/rows/new", url.Values{
		"name": {"Lena"},
		"age":  {"9"},
		"pin":  {"4242"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("create submit status = %d, want 302", resp.StatusCode)
	}

	// Spelled as declared, beside the columns rather than instead of them.
	got := lastBody()
	if got["pin"] != "4242" {
		t.Fatalf("create body did not carry the declared property under its own name: %#v", got)
	}
	if got["name"] != "Lena" {
		t.Fatalf("create body lost a column while carrying the property: %#v", got)
	}
}

// The edit form is the other half of the same distinction: there is no
// UpdateInput, so a PATCH body is the columns and a create input has no place
// on it. A form that offered one would collect a value nothing sends.
func TestEditFormDoesNotOfferACreateInput(t *testing.T) {
	const token = "secret-token"
	api, _ := childrenAPI(t, token)
	defer api.Close()
	srv, err := NewServer(createInputManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	// Create one first, so the edit form has a row to fetch.
	resp, err := client.PostForm(ts.URL+"/tables/children/rows/new", url.Values{
		"name": {"Lena"}, "age": {"9"}, "pin": {"4242"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	edit, err := client.Get(ts.URL + "/tables/children/rows/c1/edit")
	if err != nil {
		t.Fatal(err)
	}
	defer edit.Body.Close()
	if edit.StatusCode != http.StatusOK {
		t.Fatalf("edit form status = %d, want 200", edit.StatusCode)
	}
	page, err := io.ReadAll(edit.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), `name="pin"`) {
		t.Fatalf("edit form offered a create-only property:\n%s", page)
	}
}

// A rejected create is the path where the distinction is easiest to drop: the
// redisplay rebuilds the fields from the submitted form rather than from the
// row, and rebuilding them from the columns alone would take the property's
// input away on the attempt that most needs it.
func TestRejectedCreateRedisplaysTheDeclaredInput(t *testing.T) {
	const token = "secret-token"
	api, _ := childrenAPI(t, token)
	defer api.Close()
	srv, err := NewServer(createInputManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	resp, err := client.PostForm(ts.URL+"/tables/children/rows/new", url.Values{
		"name": {"Lena"},
		"age":  {"9"},
		// no pin: the API refuses it, naming the property
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (form redisplayed, not redirected)", resp.StatusCode)
	}
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(page)
	if !strings.Contains(got, "expected required property pin to be present") {
		t.Fatalf("redisplayed form missing the API's error:\n%s", got)
	}
	if !strings.Contains(got, `name="pin"`) {
		t.Fatalf("redisplayed form dropped the property the error is about:\n%s", got)
	}
	if !strings.Contains(got, `value="Lena"`) {
		t.Fatalf("redisplayed form lost what the operator typed:\n%s", got)
	}
}

// A declared property is spelled on the wire exactly as it was declared, even
// in a schema whose columns are renamed around it. WireCase is a function of a
// *column* name — every emitter writes a body property's name verbatim
// (codegen's renderBodyProps) — so a studio that derived one would send
// pinCode to a handler that only knows pin_code.
func TestADeclaredPropertyKeepsItsDeclaredSpelling(t *testing.T) {
	tbl := &schema.TableManifest{
		Name:       "children",
		PrimaryKey: "id",
		Columns: []schema.ColumnManifest{
			{Name: "display_name", Wire: "displayName", Type: "text"},
		},
		REST: &schema.RESTManifest{
			Path:        "/children",
			Operations:  []string{"create"},
			CreateInput: []schema.BodyProperty{{Name: "pin_code", Type: "varchar"}},
		},
	}
	form := url.Values{"display_name": {"Lena"}, "pin_code": {"4242"}}

	body, err := parseFormBody(tbl, createInput(tbl), form)
	if err != nil {
		t.Fatal(err)
	}
	// The column takes the wire spelling the manifest precomputed; the
	// property keeps its own, in the same object.
	if body["displayName"] != "Lena" {
		t.Fatalf("column did not use its wire spelling: %#v", body)
	}
	if body["pin_code"] != "4242" {
		t.Fatalf("declared property did not keep its declared spelling: %#v", body)
	}

	// The same rule, reached from the other body that declares properties.
	act, err := parseActionBody([]schema.BodyProperty{{Name: "pin_code", Type: "varchar"}}, form)
	if err != nil {
		t.Fatal(err)
	}
	if act["pin_code"] != "4242" {
		t.Fatalf("action body property did not keep its declared spelling: %#v", act)
	}
}
