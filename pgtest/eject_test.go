package pgtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/blog"
	_ "github.com/mind-vm/sqlb/example/blog/blogschema"
	"github.com/mind-vm/sqlb/example/blog/ejected"
	"github.com/mind-vm/sqlb/rest"
)

// The exit, tested against the thing it replaces.
//
// `sqlb eject` is an answer to an adoption objection rather than a feature
// (issue #19), and the only version of that answer worth anything is one that
// runs. So this file stands the ejected package up beside the generated
// resources it came from, points both at the same database, and sends both the
// same requests.
//
// Two properties are asserted, and the second matters as much as the first:
//
//   - what came out behaves identically — same status, same JSON, down to the
//     envelope;
//   - what did not come out is *refused by name*. A client that keeps sending
//     ?expand=author gets a 400 saying expansion is not served here, rather than
//     a 200 with a field quietly missing, which is the failure mode that would
//     make an exit worse than no exit.

// ejectedServers builds the two servers over one database: the generated
// resources, and the ejected handlers.
func ejectedServers(t *testing.T) (sqlbSrv, ejectedSrv http.Handler, pool *pgxpool.Pool) {
	t.Helper()
	pool = freshDB(t)

	// The schema comes from the ejected package's own schema.sql, which is the
	// first claim this file makes: the DDL in the exit is the DDL the project
	// was applying.
	mustExec(t, pool, ejected.Schema)

	// The generated half. posts declares a soft delete, so the resource does
	// not mount until a hook confines it (ADR-0030).
	hooks := sqlb.NewRegistry()
	sqlb.On[blog.Post](hooks).BeforeQuery(func(_ context.Context, q *sqlb.Builder[blog.Post]) error {
		q.Where(sqlb.F("deleted_at").IsNull())
		return nil
	})
	db := sqlb.New(pool).WithHooks(hooks)

	srv := rest.NewServer(rest.Config{Title: "Blog", Version: "1.0.0"})
	if err := blog.Register(srv.API, db); err != nil {
		t.Fatalf("mounting the generated resources: %v", err)
	}

	// The ejected half. The same obligation, satisfied by the same predicate —
	// the seam is a function field instead of a hook registration, and that is
	// the whole difference.
	mux := http.NewServeMux()
	err := ejected.Register(mux, pool, ejected.Options{
		Posts: ejected.PostsHooks{
			Confine: func(*http.Request) ([]ejected.Condition, error) {
				return []ejected.Condition{{Column: "deleted_at", Op: ejected.OpIsNull}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("mounting the ejected resources: %v", err)
	}
	return srv.Handler, mux, pool
}

// seedForEject fills both servers' database with rows to read back.
func seedForEject(t *testing.T, pool *pgxpool.Pool) (orgID, authorID string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (name, slug) VALUES ('Acme', 'acme') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seeding the org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO authors (org_id, email, name, password_hash)
		 VALUES ($1, 'ada@example.com', 'Ada', 'x') RETURNING id`,
		orgID).Scan(&authorID); err != nil {
		t.Fatalf("seeding the author: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO authors (org_id, email, name, password_hash)
		 VALUES ($1, 'grace@example.com', 'Grace', 'x')`,
		orgID); err != nil {
		t.Fatalf("seeding the second author: %v", err)
	}
	for _, p := range []struct {
		title, status string
		published     any
	}{
		{"Hello", "published", "2024-01-01T00:00:00Z"},
		{"Draft one", "draft", nil},
		{"Second post", "published", "2024-02-01T00:00:00Z"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO posts (org_id, author_id, title, body, status, published_at)
			 VALUES ($1, $2, $3, 'body', $4, $5)`,
			orgID, authorID, p.title, p.status, p.published); err != nil {
			t.Fatalf("seeding %q: %v", p.title, err)
		}
	}
	return orgID, authorID
}

// ejGet sends one request to a handler and returns the status and the decoded body.
func ejGet(t *testing.T, h http.Handler, target string) (int, any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Code, ejDecodeJSON(t, rec.Body.String())
}

func ejDecodeJSON(t *testing.T, body string) any {
	t.Helper()
	if strings.TrimSpace(body) == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	return v
}

// ejNormalise removes the fields the exit deliberately does not produce, so that
// what is left is compared rather than the known difference.
//
// There are exactly two. next_cursor, because keyset paging did not come out —
// which the emitted README states and TestEjectedRefusesWhatItDoesNotServe
// asserts. And $schema, which is huma's own hyperlink to the response schema
// rather than anything sqlb decided; the exit has no OpenAPI document to point
// at. Any *other* difference is a real one, and this comparison is the thing
// that has to notice it.
func ejNormalise(v any) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return v
	}
	delete(obj, "next_cursor")
	delete(obj, "$schema")
	return obj
}

// The read path, request for request.
func TestEjectedServesTheSameReads(t *testing.T) {
	t.Parallel()
	generated, exit, pool := ejectedServers(t)
	_, authorID := seedForEject(t, pool)

	for _, target := range []string{
		"/authors",
		"/authors?sort=name",
		"/authors?sort=-name&per_page=1",
		"/authors?name=contains.ad",
		"/authors?email=eq.ada@example.com",
		"/authors?search=grace",
		"/authors?count=exact",
		"/authors/" + authorID,
		"/posts",
		"/posts?status=published",
		"/posts?status=in.draft,review",
		"/posts?sort=-published_at",
		"/posts?published_at=isnull",
		"/posts?view_count=lte.0&count=exact",
		"/orgs",
	} {
		t.Run(target, func(t *testing.T) {
			wantCode, wantBody := ejGet(t, generated, target)
			gotCode, gotBody := ejGet(t, exit, target)

			if gotCode != wantCode {
				t.Fatalf("status = %d, the generated resource said %d\n%v", gotCode, wantCode, gotBody)
			}
			want, _ := json.Marshal(ejNormalise(wantBody))
			got, _ := json.Marshal(ejNormalise(gotBody))
			if string(got) != string(want) {
				t.Errorf("the exit answered differently\n  ejected:   %s\n  generated: %s", got, want)
			}
		})
	}
}

// A rejection is behaviour too: a column that never declared Filterable is not
// filterable in the exit either, and an id that matches nothing is a 404 in
// both.
func TestEjectedRefusesTheSameRequests(t *testing.T) {
	t.Parallel()
	generated, exit, pool := ejectedServers(t)
	seedForEject(t, pool)

	for _, tc := range []struct {
		target string
		status int
	}{
		{"/authors?password_hash=eq.x", http.StatusBadRequest},
		{"/authors?nonexistent=1", http.StatusBadRequest},
		{"/authors?sort=email", http.StatusBadRequest},
		// deleted_at is read-only and was never made filterable, so neither
		// serves a filter on it. body *is* filterable — Searchable implies it —
		// which is the kind of thing this comparison is here to keep straight.
		{"/posts?deleted_at=isnull", http.StatusBadRequest},
		{"/authors/00000000-0000-0000-0000-000000000000", http.StatusNotFound},
	} {
		t.Run(tc.target, func(t *testing.T) {
			wantCode, _ := ejGet(t, generated, tc.target)
			gotCode, body := ejGet(t, exit, tc.target)
			if wantCode != tc.status {
				t.Fatalf("the generated resource answered %d, the test expected %d", wantCode, tc.status)
			}
			if gotCode != wantCode {
				t.Errorf("the exit answered %d, the generated resource %d\n%v", gotCode, wantCode, body)
			}
		})
	}
}

// The adversarial trio. TestEjectedRefusesTheSameRequests compares happy-path
// answers request by request, which is why two budget gaps and a live wildcard
// shipped in the exit unnoticed (#69): the emitted parser enforced neither the
// list cap nor the value-length cap, and ?search left % and _ live, so a request
// the API answered 400 was accepted here — 100 000 bind parameters against pgx's
// 65535 limit, or a caller-controlled scan-cost lever. Bind discipline was
// intact in both cases; the eject contract is "same requests, same refusals",
// and these were the spots where it was not.
func TestEjectedRefusesTheSameOversizedRequests(t *testing.T) {
	t.Parallel()
	generated, exit, pool := ejectedServers(t)
	seedForEject(t, pool)

	longValue := strings.Repeat("x", 300)
	bigList := make([]string, 200)
	for i := range bigList {
		bigList[i] = fmt.Sprintf("v%d", i)
	}

	for _, tc := range []struct{ name, target string }{
		{"oversized in list", "/authors?email=in." + strings.Join(bigList, ",")},
		{"oversized value", "/authors?email=eq." + longValue},
		{"oversized search term", "/posts?search=" + longValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantCode, _ := ejGet(t, generated, tc.target)
			if wantCode != http.StatusBadRequest {
				t.Fatalf("the generated resource answered %d; this test assumes it refuses", wantCode)
			}
			if gotCode, body := ejGet(t, exit, tc.target); gotCode != wantCode {
				t.Errorf("the exit answered %d, the generated resource %d\n%v", gotCode, wantCode, body)
			}
		})
	}
}

// And the wildcard. `?search=50%` is a search for a literal percent sign, and
// the exit used to build the operand `%50%%` from it, matching everything with
// a 5 followed by a 0 followed by anything. A silent behaviour change between
// the API and its replacement, on the one pattern path that was not escaped.
func TestEjectedSearchEscapesItsWildcards(t *testing.T) {
	t.Parallel()
	generated, exit, pool := ejectedServers(t)
	seedForEject(t, pool)

	target := "/posts?search=" + url.QueryEscape("%")
	wantCode, want := ejGet(t, generated, target)
	if wantCode != http.StatusOK {
		t.Fatalf("the generated resource answered %d", wantCode)
	}
	gotCode, got := ejGet(t, exit, target)
	if gotCode != wantCode {
		t.Fatalf("the exit answered %d, the generated resource %d\n%v", gotCode, wantCode, got)
	}

	// A bare % matches nothing literally, so both must return an empty page.
	// Before the escape the exit's operand was `%%%`, which matches everything.
	wantItems, _ := ejNormalise(want).(map[string]any)["items"].([]any)
	gotItems, _ := ejNormalise(got).(map[string]any)["items"].([]any)
	if len(gotItems) != len(wantItems) {
		t.Errorf("the exit matched %d rows for a literal %%, the generated resource %d",
			len(gotItems), len(wantItems))
	}
}

// What did not come out is refused by name. This is the assertion that keeps
// the exit honest: silence here would mean a client's ?expand quietly returning
// less than it asked for.
func TestEjectedRefusesWhatItDoesNotServe(t *testing.T) {
	t.Parallel()
	generated, exit, pool := ejectedServers(t)
	seedForEject(t, pool)

	// The cursor is taken from a real page rather than made up, so that the
	// generated resource genuinely serves the request the exit refuses.
	_, first := ejGet(t, generated, "/posts?per_page=1")
	cursor, _ := first.(map[string]any)["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("the generated resource returned no cursor to compare against")
	}

	for _, tc := range []struct{ target, says string }{
		{"/posts?expand=author", "expansion"},
		{"/posts?select=id,title", "projection"},
		{"/posts?cursor=" + url.QueryEscape(cursor), "keyset"},
		{"/posts?filter=" + url.QueryEscape(`{"op":"eq","field":"status","value":"draft"}`), "JSON filter tree"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			// The generated resource serves it — that is what makes this a gap
			// rather than a shared limitation.
			if code, _ := ejGet(t, generated, tc.target); code != http.StatusOK {
				t.Fatalf("the generated resource answered %d; this test assumes it serves the parameter", code)
			}
			code, body := ejGet(t, exit, tc.target)
			if code != http.StatusBadRequest {
				t.Fatalf("the exit answered %d, want 400 naming the gap\n%v", code, body)
			}
			raw, _ := json.Marshal(body)
			if !strings.Contains(string(raw), tc.says) {
				t.Errorf("the refusal does not say what is missing (%q):\n%s", tc.says, raw)
			}
		})
	}
}

// The write path. Creating through the exit and reading back through the
// generated resource is the strongest form of this comparison: the two agree
// about the row, not merely about their own output.
func TestEjectedWritesAreReadableByTheGeneratedResource(t *testing.T) {
	t.Parallel()
	generated, exit, pool := ejectedServers(t)
	orgID, _ := seedForEject(t, pool)

	body := fmt.Sprintf(`{"org_id":%q,"email":"linus@example.com","name":"Linus"}`, orgID)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/authors", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	exit.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST through the exit = %d, want 201\n%s", rec.Code, rec.Body.String())
	}
	created, ok := ejDecodeJSON(t, rec.Body.String()).(map[string]any)
	if !ok {
		t.Fatalf("create response is not an object: %s", rec.Body.String())
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("the create response carries no id: %s", rec.Body.String())
	}

	// The same row, through the generated resource.
	code, read := ejGet(t, generated, "/authors/"+id)
	if code != http.StatusOK {
		t.Fatalf("reading the ejected write back = %d", code)
	}
	want, _ := json.Marshal(read)
	got, _ := json.Marshal(created)
	if string(got) != string(want) {
		t.Errorf("the two disagree about the row\n  ejected create: %s\n  generated read: %s", got, want)
	}

	// And a patch, the other way round: written by the exit, read by sqlb.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/authors/"+id, strings.NewReader(`{"name":"Linus T"}`))
	req.Header.Set("Content-Type", "application/json")
	exit.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH through the exit = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if _, read := ejGet(t, generated, "/authors/"+id); read.(map[string]any)["name"] != "Linus T" {
		t.Errorf("the patch did not land: %v", read)
	}

	// A delete, and then a 404 from both.
	rec = httptest.NewRecorder()
	exit.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/authors/"+id, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE through the exit = %d, want 204\n%s", rec.Code, rec.Body.String())
	}
	if code, _ := ejGet(t, generated, "/authors/"+id); code != http.StatusNotFound {
		t.Errorf("the generated resource still sees the deleted row: %d", code)
	}
	if code, _ := ejGet(t, exit, "/authors/"+id); code != http.StatusNotFound {
		t.Errorf("the exit still sees the deleted row: %d", code)
	}
}

// A write the database refuses answers the same status through both. Before the
// eject a duplicate unique value was a 409 and a foreign-key violation a 422,
// classified off SQLSTATE class 23; the exit answered 500 to both, so a
// client-side retry loop keyed on 409 broke quietly on the day of the eject
// (#70). Status parity is the property clients branch on, so it is the property
// asserted.
func TestEjectedAnswersAConstraintViolationTheSameWay(t *testing.T) {
	t.Parallel()
	generated, exit, pool := ejectedServers(t)
	orgID, _ := seedForEject(t, pool)

	post := func(h http.Handler, path, body string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// email is Unique. The first create succeeds through the exit; the second,
	// through either server, is the same duplicate.
	dup := fmt.Sprintf(`{"org_id":%q,"email":"dup@example.com","name":"First"}`, orgID)
	if code := post(exit, "/authors", dup); code != http.StatusCreated {
		t.Fatalf("seeding the duplicate = %d, want 201", code)
	}

	// org_id references orgs, so a uuid that names no org is a foreign-key
	// violation — the 422 half of the mapping.
	const noSuchOrg = "00000000-0000-0000-0000-000000000000"
	orphan := fmt.Sprintf(`{"org_id":%q,"email":"orphan@example.com","name":"Orphan"}`, noSuchOrg)

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"duplicate unique value", dup, http.StatusConflict},
		{"foreign key violation", orphan, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := post(generated, "/authors", tc.body); code != tc.want {
				t.Fatalf("the generated resource answered %d, this test expects %d", code, tc.want)
			}
			if code := post(exit, "/authors", tc.body); code != tc.want {
				t.Errorf("the exit answered %d, the generated resource %d", code, tc.want)
			}
		})
	}
}

// The obligation survives the exit. A resource whose table declared a soft
// delete refuses to register without the hook that confines it — ADR-0030,
// with the machinery removed and the property kept.
func TestEjectedRefusesToMountWithoutItsHook(t *testing.T) {
	t.Parallel()
	err := ejected.Register(http.NewServeMux(), nil, ejected.Options{})
	if err == nil {
		t.Fatal("registering a soft-deleting resource with no Confine should fail")
	}
	for _, want := range []string{"/posts", "Confine", "deleted_at"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}
