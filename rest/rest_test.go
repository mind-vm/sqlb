package rest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

func postOptions() rest.Options {
	return rest.Options{
		Path:            "/posts",
		Name:            "post",
		Ops:             rest.CRUD | rest.OpList,
		DefaultPageSize: 2,
		MaxPageSize:     10,
	}
}

// mount registers the Post resource against a test API backed by db.
func mount(t *testing.T, db sqlb.Executor, opts rest.Options) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[Post, PostCreate, PostUpdate](api, db, opts); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}
	return api
}

// decode reads a JSON body, failing the test rather than the caller.
func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	return out
}

func TestListCompilesFiltersIntoSQL(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	api := mount(t, db.db, postOptions())

	resp := api.Get("/posts?status=eq.draft&view_count=gte.3&sort=-created_at")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}

	stmt := db.lastStatement()
	for _, want := range []string{`"status" = $1`, `"view_count" >= $2`, `ORDER BY "created_at" DESC`} {
		if !strings.Contains(stmt, want) {
			t.Errorf("statement missing %q:\n%s", want, stmt)
		}
	}
	// The hidden column must not reach the projection, whatever the request
	// asked for.
	if strings.Contains(stmt, "secret") {
		t.Errorf("hidden column reached the query:\n%s", stmt)
	}
}

func TestListPaginationReportsMoreWithoutCounting(t *testing.T) {
	// Three rows come back for a page size of two, which is how the handler
	// learns there is another page.
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{
		postRow("p1", "One"), postRow("p2", "Two"), postRow("p3", "Three"),
	}})
	api := mount(t, db.db, postOptions())

	resp := api.Get("/posts")
	body := decode(t, resp.Body.Bytes())

	items, ok := body["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %v, want 2 rows", body["items"])
	}
	if body["has_more"] != true {
		t.Errorf("has_more = %v, want true", body["has_more"])
	}
	if _, present := body["total"]; present {
		t.Error("total should be absent unless ?count=exact was given")
	}
	if got := len(db.statements()); got != 1 {
		t.Errorf("issued %d statements, want 1: counting must stay opt-in", got)
	}
	// The page query asks for one row beyond the page.
	if !strings.Contains(db.lastStatement(), "LIMIT 3") {
		t.Errorf("expected the page query to over-fetch by one:\n%s", db.lastStatement())
	}
}

func TestListCountExactAddsTotal(t *testing.T) {
	db := newFakeDB(t,
		reply{match: "count(", cols: []string{"count"}, rows: [][]any{{int64(42)}}},
		reply{cols: postCols(), rows: [][]any{postRow("p1", "One")}},
	)
	api := mount(t, db.db, postOptions())

	body := decode(t, api.Get("/posts?count=exact").Body.Bytes())
	if body["total"] != float64(42) {
		t.Errorf("total = %v, want 42", body["total"])
	}
	stmts := db.statements()
	if len(stmts) != 2 {
		t.Fatalf("issued %d statements, want 2 (page and count)", len(stmts))
	}
	// The count is of everything matching the filter, not of the page, so the
	// over-fetch limit must not survive into it.
	var count string
	for _, s := range stmts {
		if strings.Contains(s, "count(") {
			count = s
		}
	}
	if count == "" {
		t.Fatalf("no count statement was issued: %v", stmts)
	}
	if strings.Contains(count, "LIMIT") {
		t.Errorf("the count query is capped by the page limit, so total would be wrong:\n%s", count)
	}
}

func TestListRejectionNamesTheAllowedColumns(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols()})
	api := mount(t, db.db, postOptions())

	resp := api.Get("/posts?sort=body")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.Code, resp.Body)
	}

	body := decode(t, resp.Body.Bytes())
	details, ok := body["errors"].([]any)
	if !ok || len(details) != 1 {
		t.Fatalf("errors = %v, want one detail", body["errors"])
	}
	detail, ok := details[0].(map[string]any)
	if !ok {
		t.Fatalf("detail is %T, want an object", details[0])
	}
	allowed, ok := detail["allowed"].([]any)
	if !ok || len(allowed) == 0 {
		t.Fatalf("allowed = %v, want the sortable columns", detail["allowed"])
	}
	// ADR-0011: the rejection names what would have worked, and never names a
	// hidden column.
	for _, name := range allowed {
		if name == "secret" {
			t.Error("the allow-list must not disclose a hidden column")
		}
	}
	if detail["location"] != "query.sort" {
		t.Errorf("location = %v, want query.sort", detail["location"])
	}
}

func TestListRejectionReportsEveryProblemAtOnce(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols()})
	api := mount(t, db.db, postOptions())

	body := decode(t, api.Get("/posts?sort=body&nonesuch=1").Body.Bytes())
	details, _ := body["errors"].([]any)
	if len(details) != 2 {
		t.Errorf("reported %d problems, want 2: a malformed request should take one round trip to fix", len(details))
	}
}

func TestSelectShapesTheResponseObject(t *testing.T) {
	db := newFakeDB(t, reply{
		cols: []string{"id", "title"},
		rows: [][]any{{"p1", "Hello"}},
	})
	api := mount(t, db.db, postOptions())

	body := decode(t, api.Get("/posts?select=title").Body.Bytes())
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want one row", body["items"])
	}
	item, _ := items[0].(map[string]any)

	// An unselected column is absent, not present and empty: a zero value from
	// a partial scan is indistinguishable from a real one.
	if _, present := item["body"]; present {
		t.Errorf("unselected column body is present in %v", item)
	}
	// The primary key is added back, since a row that cannot address itself is
	// of little use.
	if item["id"] != "p1" {
		t.Errorf("id = %v, want p1 — the projection should keep the key", item["id"])
	}
	if item["title"] != "Hello" {
		t.Errorf("title = %v, want Hello", item["title"])
	}
}

func TestHiddenColumnIsNeverSerialised(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	api := mount(t, db.db, postOptions())

	raw := api.Get("/posts").Body.String()
	if strings.Contains(raw, "secret") {
		t.Errorf("response mentions the hidden column: %s", raw)
	}
}

func TestReadNotFound(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols()})
	api := mount(t, db.db, postOptions())

	resp := api.Get("/posts/p1")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body)
	}
}

func TestReadRejectsStrayQueryParameters(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	api := mount(t, db.db, postOptions())

	// Silently ignoring an unknown parameter would answer a question the client
	// did not ask.
	resp := api.Get("/posts/p1?select=title")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 for an undeclared parameter: %s", resp.Code, resp.Body)
	}
}

// Create and update declare no query parameter, so anything in the query string
// is a mistake and must be named. Every other operation already refused; these
// two accepted silently, so the same typo answered 400 on a GET and 201/200 on a
// write.
func TestWritesRefuseAnUnknownQueryParameter(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(api humatest.TestAPI) *httptest.ResponseRecorder
	}{
		{"create", func(api humatest.TestAPI) *httptest.ResponseRecorder {
			return api.Post("/posts?exapnd=author", map[string]any{
				"org_id": "acme", "title": "Hello", "body": "text",
			})
		}},
		{"update", func(api humatest.TestAPI) *httptest.ResponseRecorder {
			return api.Patch("/posts/p1?sort=title", map[string]any{"title": "Changed"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
			api := mount(t, db.db, postOptions())

			if resp := tc.do(api); resp.Code < 400 {
				t.Errorf("status = %d, want a refusal for an undeclared parameter: %s", resp.Code, resp.Body)
			}
		})
	}
}

func TestCreateOmitsReadOnlyColumns(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	api := mount(t, db.db, postOptions())

	resp := api.Post("/posts", map[string]any{
		"org_id": "acme", "title": "Hello", "body": "text",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}

	stmt := db.lastStatement()
	for _, forbidden := range []string{`"id"`, `"view_count"`, `"created_at"`} {
		if strings.Contains(strings.SplitN(stmt, "VALUES", 2)[0], forbidden) {
			t.Errorf("read-only column %s reached the insert:\n%s", forbidden, stmt)
		}
	}
	if !strings.Contains(stmt, `"title"`) {
		t.Errorf("writable column title missing from the insert:\n%s", stmt)
	}
	// status is defaulted and was not given, so the database supplies it.
	if strings.Contains(strings.SplitN(stmt, "VALUES", 2)[0], `"status"`) {
		t.Errorf("a defaulted column left unset should be omitted:\n%s", stmt)
	}
}

func TestUpdateWritesOnlyTheNamedColumns(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Changed")}})
	api := mount(t, db.db, postOptions())

	resp := api.Patch("/posts/p1", map[string]any{"title": "Changed"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	stmt := db.lastStatement()
	if !strings.Contains(stmt, `SET "title" = $1`) {
		t.Errorf("expected a single-column update:\n%s", stmt)
	}
	if strings.Contains(stmt, `"body"`) && strings.Contains(stmt, "SET") &&
		strings.Contains(strings.SplitN(stmt, "WHERE", 2)[0], `"body"`) {
		t.Errorf("an absent field must not be written:\n%s", stmt)
	}
}

func TestUpdateRefusesImmutableColumn(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols()})
	api := mount(t, db.db, postOptions())

	resp := api.Patch("/posts/p1", map[string]any{"org_id": "other"})
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", resp.Code, resp.Body)
	}
	body := decode(t, resp.Body.Bytes())
	details, _ := body["errors"].([]any)
	if len(details) != 1 {
		t.Fatalf("errors = %v, want one detail", body["errors"])
	}
	detail, _ := details[0].(map[string]any)
	if !strings.Contains(detail["message"].(string), "cannot be changed") {
		t.Errorf("message = %v, want it to name immutability", detail["message"])
	}
	if len(db.statements()) != 0 {
		t.Error("a rejected update must not reach the database")
	}
}

func TestUpdateWithNoFieldsIsRejected(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols()})
	api := mount(t, db.db, postOptions())

	resp := api.Patch("/posts/p1", map[string]any{})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.Code, resp.Body)
	}
	body := decode(t, resp.Body.Bytes())
	details, _ := body["errors"].([]any)
	detail, _ := details[0].(map[string]any)
	if _, ok := detail["allowed"].([]any); !ok {
		t.Errorf("the rejection should name the writable columns: %v", detail)
	}
}

func TestDelete(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	api := mount(t, db.db, postOptions())

	resp := api.Delete("/posts/p1")
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", resp.Code, resp.Body)
	}
	if !strings.Contains(db.lastStatement(), `DELETE FROM "posts"`) {
		t.Errorf("unexpected statement: %s", db.lastStatement())
	}
}

func TestDeleteMissingRowIs404(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols()})
	api := mount(t, db.db, postOptions())

	if code := api.Delete("/posts/p1").Code; code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

func TestBeforeQueryHookAppliesToTheRESTSurface(t *testing.T) {
	reg := sqlb.NewRegistry()
	sqlb.On[Post](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[Post]) error {
		q.Where(sqlb.F("org_id").Eq("acme"))
		return nil
	})

	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	api := mount(t, sqlb.New(db.db).WithHooks(reg), postOptions())

	api.Get("/posts")
	if !strings.Contains(db.lastStatement(), `"org_id" = $1`) {
		t.Errorf("the tenant scope did not reach the list query:\n%s", db.lastStatement())
	}
}

// TestSoftDeleteColumnIsInertUntilAHookUsesIt pins the half of schema.SoftDelete
// that is easy to assume, and that this comment once claimed: the REST layer
// does not know deleted_at exists. Lists return the soft-deleted rows and DELETE
// removes them, until a BeforeQuery registration says otherwise.
//
// This test fires against the behaviour we chose not to build. If deleted_at
// ever becomes load-bearing in the runtime, this is what fails first, and
// ADR-0008 — which records soft-delete filtering as one hook registration — is
// what has to change with it.
func TestSoftDeleteColumnIsInertUntilAHookUsesIt(t *testing.T) {
	archivedOptions := func() rest.Options {
		return rest.Options{
			Path:            "/archived",
			Name:            "archived",
			Ops:             rest.OpList | rest.OpDelete,
			DefaultPageSize: 10,
			MaxPageSize:     10,
		}
	}
	mountArchived := func(t *testing.T, db sqlb.Executor) humatest.TestAPI {
		t.Helper()
		_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
		err := rest.Resource[Archived, rest.None[Archived], rest.None[Archived]](
			api, db, archivedOptions())
		if err != nil {
			t.Fatalf("mounting the resource: %v", err)
		}
		return api
	}

	t.Run("list does not filter the deleted rows out", func(t *testing.T) {

		db := newFakeDB(t, reply{cols: archivedCols(), rows: [][]any{
			archivedRow("a1", "Gone"),
		}})
		api := mountArchived(t, db.db)

		resp := api.Get("/archived")
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
		}
		// deleted_at is in the projection, where it belongs — the column is
		// readable. What must not appear is a predicate over it, and with no
		// filter in the request there should be no WHERE clause at all.
		stmt := db.lastStatement()
		if strings.Contains(stmt, `"deleted_at" IS NULL`) {
			t.Errorf("the list query filtered on deleted_at, which nothing should:\n%s", stmt)
		}
		if strings.Contains(stmt, "WHERE") {
			t.Errorf("an unfiltered request compiled a predicate:\n%s", stmt)
		}

		// The row carries a non-null deleted_at and still comes back, which is
		// the observable half of the same fact.
		body := decode(t, resp.Body.Bytes())
		items, ok := body["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("items = %v, want the soft-deleted row", body["items"])
		}
	})

	t.Run("delete removes the row rather than stamping the column", func(t *testing.T) {

		db := newFakeDB(t, reply{cols: archivedCols(), rows: [][]any{
			archivedRow("a1", "Gone"),
		}})
		api := mountArchived(t, db.db)

		if code := api.Delete("/archived/a1").Code; code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", code)
		}
		stmt := db.lastStatement()
		if !strings.Contains(stmt, `DELETE FROM "archived"`) {
			t.Errorf("generated DELETE is not a delete:\n%s", stmt)
		}
		if strings.Contains(stmt, "UPDATE") || strings.Contains(stmt, "deleted_at") {
			t.Errorf("generated DELETE was rewritten as a soft delete:\n%s", stmt)
		}
	})

	t.Run("a BeforeQuery registration is what filters", func(t *testing.T) {
		reg := sqlb.NewRegistry()
		sqlb.On[Archived](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[Archived]) error {
			q.Where(sqlb.F("deleted_at").IsNull())
			return nil
		})

		db := newFakeDB(t, reply{cols: archivedCols()})
		api := mountArchived(t, sqlb.New(db.db).WithHooks(reg))

		api.Get("/archived")
		if stmt := db.lastStatement(); !strings.Contains(stmt, `"deleted_at" IS NULL`) {
			t.Errorf("the documented path does not reach the list query:\n%s", stmt)
		}
	})
}

func TestResourceRefusesSingleRowOpsWithoutAPrimaryKey(t *testing.T) {
	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	err := rest.Resource[Keyless, rest.None[Keyless], rest.None[Keyless]](api, db.db, rest.Options{
		Path: "/keyless",
		Ops:  rest.OpList | rest.OpRead,
	})
	if err == nil {
		t.Fatal("expected mounting to fail: a row cannot be addressed without a key")
	}
	if !strings.Contains(err.Error(), "primary key") {
		t.Errorf("error = %v, want it to name the missing primary key", err)
	}
}

// ?expand parses cleanly and performs no join, so a resource that advertised it
// would answer 200 with the relation missing — a failure no client can detect.
// Startup is the last point that can see the discrepancy, so it fails there.
// A resource declaring a relation its model does not have fails at mount. This
// replaces an earlier test that asserted mounting failed because expansion was
// unimplemented — it kept passing after expansion landed, for the wrong reason:
// Post declares no relations, so the refusal it saw was this one all along.
func TestExpandableOnAModelWithNoRelationsIsRefused(t *testing.T) {
	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	opts := postOptions()
	opts.Expandable = []string{"author"}

	err := rest.Resource[Post, PostCreate, PostUpdate](api, db.db, opts)
	if err == nil {
		t.Fatal("expected mounting to fail: Post has no expandable relation")
	}
	if !strings.Contains(err.Error(), "author") {
		t.Errorf("error = %v, want it to name the relation that was asked for", err)
	}
}

func TestResourceRefusesAHiddenColumnThatWouldSerialise(t *testing.T) {
	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	err := rest.Resource[Leaky, rest.None[Leaky], rest.None[Leaky]](api, db.db, rest.Options{
		Path: "/leaky",
		Ops:  rest.OpList,
	})
	if err == nil || !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("error = %v, want a complaint about the hidden column's json tag", err)
	}
}

// The WriteOnly analogue: the row struct's tag has to be json:"-" too, since a
// write-only column is exactly as absent from the response as a hidden one.
func TestResourceRefusesAWriteOnlyColumnThatWouldSerialise(t *testing.T) {
	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	err := rest.Resource[LeakyWriteOnly, rest.None[LeakyWriteOnly], rest.None[LeakyWriteOnly]](api, db.db, rest.Options{
		Path: "/leaky-write-only",
		Ops:  rest.OpList,
	})
	if err == nil || !strings.Contains(err.Error(), "write-only") {
		t.Fatalf("error = %v, want a complaint about the write-only column's json tag", err)
	}
}

// The worked case #195 was filed over: is_correct is set at create and never
// comes back on the create response.
func TestWriteOnlyColumnIsSettableButNeverServed(t *testing.T) {
	db := newFakeDB(t, reply{cols: []string{"id", "body"}, rows: [][]any{{"q1", "2+2=4"}}})
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[QuizOption, QuizOptionCreate, rest.None[QuizOption]](api, db.db, rest.Options{
		Path: "/quiz-options",
		Name: "quiz-option",
		Ops:  rest.OpCreate | rest.OpRead | rest.OpList,
	}); err != nil {
		t.Fatalf("mounting: %v", err)
	}

	resp := api.Post("/quiz-options", map[string]any{"body": "2+2=4", "isCorrect": true})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}

	stmt := db.lastStatement()
	if !strings.Contains(strings.SplitN(stmt, "VALUES", 2)[0], `"is_correct"`) {
		t.Errorf("write-only column did not reach the insert:\n%s", stmt)
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if _, ok := body["isCorrect"]; ok {
		t.Errorf("write-only column was served in the create response: %s", resp.Body)
	}
	if _, ok := body["is_correct"]; ok {
		t.Errorf("write-only column was served in the create response: %s", resp.Body)
	}
}

func TestResourceRefusesAnEmptyOpSet(t *testing.T) {
	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	err := rest.Resource[Post, PostCreate, PostUpdate](api, db.db, rest.Options{Path: "/posts"})
	if err == nil {
		t.Fatal("a resource exposing nothing should not mount")
	}
}

// CRUD reads as the complete set, and every one of them names create, read,
// update and delete — a caller reaching for "the fully exposed collection"
// this constant's name suggests gets a 405 on GET /posts instead, discovered
// by testing rather than by anything failing at mount (#193).
func TestResourceRefusesBareCRUDWithNoOpList(t *testing.T) {
	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	err := rest.Resource[Post, PostCreate, PostUpdate](api, db.db, rest.Options{
		Path: "/posts",
		Ops:  rest.CRUD,
	})
	if err == nil {
		t.Fatal("expected mounting to fail: CRUD alone has no collection route")
	}
	if !strings.Contains(err.Error(), "OpList") {
		t.Errorf("error = %v, want it to name the missing OpList", err)
	}
}

// TestCreateKeepsWhatAHookPutInAReadOnlyColumn is the regression test for the
// bug this pattern hides behind.
//
// ReadOnly means "a request may not write this; the database or a BeforeCreate
// hook owns it" — a claim the README, this package and the generated request
// bodies all make. The create path used to honour the first half by calling
// Insert.Omit, which also defeated the second: hooks run inside Exec, after the
// omit set is fixed, so a tenant id a hook had just filled in never reached the
// statement and the row was written with a NULL.
//
// Both halves are asserted here, because a fix for either one alone is easy and
// wrong.
func TestCreateKeepsWhatAHookPutInAReadOnlyColumn(t *testing.T) {
	hooks := sqlb.NewRegistry()
	sqlb.On[Tenanted](hooks).BeforeCreate(func(_ context.Context, row *Tenanted) error {
		row.TenantID = "acme"
		return nil
	})

	fake := newFakeDB(t, reply{
		cols: tenantedCols(),
		rows: [][]any{tenantedRow("t1", "acme", "Hello")},
	})
	db := sqlb.New(fake.db).WithHooks(hooks)

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[Tenanted, TenantedCreate, rest.None[Tenanted]](api, db, rest.Options{
		Path: "/tenanted", Name: "tenanted", Ops: rest.OpCreate | rest.OpList,
	}); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}

	resp := api.Post("/tenanted", map[string]any{"title": "Hello"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}

	stmt := fake.lastStatement()
	columns := strings.SplitN(stmt, "VALUES", 2)[0]
	if !strings.Contains(columns, `"tenant_id"`) {
		t.Errorf("the hook's read-only column did not reach the insert:\n%s", stmt)
	}
	// id is read-only too, and defaulted, and no hook filled it — so Insert
	// still omits it and the database supplies it. Clearing must not have cost
	// that.
	if strings.Contains(columns, `"id"`) {
		t.Errorf("a defaulted read-only column nothing filled reached the insert:\n%s", stmt)
	}
}

// TestCreateClearsAReadOnlyColumnTheBodySet is the other half: the guarantee
// against the request, which is what made Omit look correct in the first place.
func TestCreateClearsAReadOnlyColumnTheBodySet(t *testing.T) {
	fake := newFakeDB(t, reply{
		cols: tenantedCols(),
		rows: [][]any{tenantedRow("t1", "", "Hello")},
	})

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[Tenanted, SmugglingCreate, rest.None[Tenanted]](api, fake.db, rest.Options{
		Path: "/tenanted", Name: "tenanted", Ops: rest.OpCreate | rest.OpList,
	}); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}

	resp := api.Post("/tenanted", map[string]any{"title": "Hello", "tenant_id": "someone-else"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}

	// The column is still written — it has no default, so there is nothing else
	// to write — but with the zero value rather than the one the body chose.
	// What must not happen is the request's value reaching the arguments.
	for _, arg := range fake.lastArgs() {
		if arg == "someone-else" {
			t.Errorf("a request wrote a read-only column: %v\n%s", fake.lastArgs(), fake.lastStatement())
		}
	}
}

// mountDocs registers the expandable Doc resource.
func mountDocs(t *testing.T, db sqlb.Executor, expandable []string) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[Doc, rest.None[Doc], rest.None[Doc]](api, db, rest.Options{
		Path: "/documents", Name: "document", Ops: rest.OpRead | rest.OpList,
		Expandable: expandable,
	}); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}
	return api
}

func TestExpandJoinsAndNestsTheRelation(t *testing.T) {
	db := newFakeDB(t, reply{cols: docCols(), rows: [][]any{
		docRow("d1", "Hello", []byte(`{"id":"acme","name":"Acme"}`)),
	}})
	api := mountDocs(t, db.db, []string{"org"})

	resp := api.Get("/documents?expand=org")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}

	stmt := db.lastStatement()
	if !strings.Contains(stmt, `LEFT JOIN "orgs" AS "__ex_org"`) {
		t.Errorf("no join in the statement:\n%s", stmt)
	}
	// Hidden survives the join.
	if strings.Contains(stmt, "secret") {
		t.Errorf("a hidden column of the target reached the statement:\n%s", stmt)
	}

	body := decode(t, resp.Body.Bytes())
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %v", body["items"])
	}
	item, _ := items[0].(map[string]any)
	org, ok := item["org"].(map[string]any)
	if !ok {
		t.Fatalf("the expansion is not nested under its relation name: %v", item)
	}
	if org["name"] != "Acme" {
		t.Errorf("expanded org = %v", org)
	}
	// The foreign key is still its own property.
	if item["org_id"] != "acme" {
		t.Errorf("org_id = %v, want acme", item["org_id"])
	}
}

// Not asking for the expansion must leave the response and the SQL alone.
func TestWithoutExpandNothingIsJoinedOrNested(t *testing.T) {
	db := newFakeDB(t, reply{cols: docCols()[:3], rows: [][]any{
		{"d1", "acme", "Hello"},
	}})
	api := mountDocs(t, db.db, []string{"org"})

	resp := api.Get("/documents")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	if strings.Contains(db.lastStatement(), "LEFT JOIN") {
		t.Errorf("an unexpanded request joined anyway:\n%s", db.lastStatement())
	}
	items, _ := decode(t, resp.Body.Bytes())["items"].([]any)
	item, _ := items[0].(map[string]any)
	if _, present := item["org"]; present {
		t.Errorf("an unexpanded response carries the relation: %v", item)
	}
}

// A relation the model does not declare must fail at mount, not at request
// time, where it would parse cleanly and answer 200 without the relation.
func TestExpandableIsCheckedAgainstTheModelAtStartup(t *testing.T) {
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.Resource[Doc, rest.None[Doc], rest.None[Doc]](api, newFakeDB(t).db, rest.Options{
		Path: "/documents", Name: "document", Ops: rest.OpList,
		Expandable: []string{"owner"},
	})
	if err == nil {
		t.Fatal("a resource declared an expandable relation the model does not have")
	}
	if !strings.Contains(err.Error(), "org") {
		t.Errorf("the refusal does not name the declared relations: %v", err)
	}
}

// ?expand naming a relation the resource did not expose is a 400 with the
// allow-list, like every other rejected parameter (ADR-0011).
func TestExpandRejectionNamesWhatWouldHaveWorked(t *testing.T) {
	db := newFakeDB(t, reply{cols: docCols(), rows: nil})
	api := mountDocs(t, db.db, []string{"org"})

	resp := api.Get("/documents?expand=owner")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.Code, resp.Body)
	}
	if !strings.Contains(resp.Body.String(), "org") {
		t.Errorf("the rejection does not name the expandable relations: %s", resp.Body)
	}
}

// ?expand on the item endpoint. The list endpoint had it from the start; the
// item one refused every query parameter, so a client that fetched a row after
// creating it had to fetch the relation separately or re-list.

func TestExpandOnTheItemEndpoint(t *testing.T) {
	db := newFakeDB(t, reply{cols: docCols(), rows: [][]any{
		docRow("d1", "Hello", []byte(`{"id":"acme","name":"Acme"}`)),
	}})
	api := mountDocs(t, db.db, []string{"org"})

	resp := api.Get("/documents/d1?expand=org")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}

	stmt := db.lastStatement()
	if !strings.Contains(stmt, `LEFT JOIN "orgs" AS "__ex_org"`) {
		t.Errorf("no join in the statement:\n%s", stmt)
	}
	// The item query addresses the row by primary key, and `id` is exactly the
	// column both tables have. Unqualified it is not a wrong predicate, it is
	// not a query — see ADR-0025.
	if !strings.Contains(stmt, `WHERE "docs"."id" = $1`) {
		t.Errorf("the key predicate is not qualified, so the join makes it ambiguous:\n%s", stmt)
	}
	// Hidden survives the join here too.
	if strings.Contains(stmt, "secret") {
		t.Errorf("a hidden column of the target reached the statement:\n%s", stmt)
	}

	item := decode(t, resp.Body.Bytes())
	org, ok := item["org"].(map[string]any)
	if !ok {
		t.Fatalf("the expansion is not nested under its relation name: %v", item)
	}
	if org["name"] != "Acme" {
		t.Errorf("expanded org = %v", org)
	}
	if item["org_id"] != "acme" {
		t.Errorf("org_id = %v, want acme", item["org_id"])
	}
}

func TestItemWithoutExpandJoinsNothing(t *testing.T) {
	db := newFakeDB(t, reply{cols: docCols()[:3], rows: [][]any{
		{"d1", "acme", "Hello"},
	}})
	api := mountDocs(t, db.db, []string{"org"})

	resp := api.Get("/documents/d1")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	if strings.Contains(db.lastStatement(), "LEFT JOIN") {
		t.Errorf("an unexpanded item request joined anyway:\n%s", db.lastStatement())
	}
	if _, present := decode(t, resp.Body.Bytes())["org"]; present {
		t.Errorf("an unexpanded response carries the relation: %s", resp.Body)
	}
}

// The rejection is the list endpoint's, not a second copy of it: same status,
// same allow-list (ADR-0011).
func TestItemExpandRejectionNamesWhatWouldHaveWorked(t *testing.T) {
	db := newFakeDB(t, reply{cols: docCols(), rows: nil})
	api := mountDocs(t, db.db, []string{"org"})

	resp := api.Get("/documents/d1?expand=owner")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.Code, resp.Body)
	}
	if !strings.Contains(resp.Body.String(), "org") {
		t.Errorf("the rejection does not say what would have worked: %s", resp.Body)
	}
}

// A resource with nothing expandable must refuse ?expand as an unknown
// parameter rather than accepting it and answering without the relation. This
// is why the parameter is declared per-resource instead of living on the input
// struct, where it would exist on every resource.
func TestItemRefusesExpandWhenTheResourceDeclaresNone(t *testing.T) {
	db := newFakeDB(t, reply{cols: docCols()[:3], rows: [][]any{
		{"d1", "acme", "Hello"},
	}})
	api := mountDocs(t, db.db, nil)

	resp := api.Get("/documents/d1?expand=org")
	if resp.Code == http.StatusOK {
		t.Fatalf("a resource with no expandable relation answered ?expand with 200: %s", resp.Body)
	}
	// And an ordinary read still works.
	if code := api.Get("/documents/d1").Code; code != http.StatusOK {
		t.Errorf("plain item read = %d, want 200", code)
	}
}

// Unknown query parameters stay refused now that the operation declares one.
func TestItemStillRefusesAnUnknownQueryParameter(t *testing.T) {
	db := newFakeDB(t, reply{cols: docCols(), rows: nil})
	api := mountDocs(t, db.db, []string{"org"})

	if resp := api.Get("/documents/d1?sort=title"); resp.Code == http.StatusOK {
		t.Errorf("an unknown query parameter was accepted on the item endpoint: %s", resp.Body)
	}
}

// The item operation documents ?expand with the same enum the list operation
// carries. A client generates against that enum, so two spellings of it is two
// places for a relation to go missing — and huma builds its parameter set from
// the input struct, which would give a bare string array without this.
func TestItemOperationDocumentsTheExpandableRelations(t *testing.T) {
	db := newFakeDB(t, reply{cols: docCols(), rows: nil})
	api := mountDocs(t, db.db, []string{"org"})

	op := api.OpenAPI().Paths["/documents/{id}"].Get
	if op == nil {
		t.Fatal("no GET operation on the item path")
	}
	var found *huma.Param
	for _, p := range op.Parameters {
		if p.Name == "expand" {
			found = p
		}
	}
	if found == nil {
		t.Fatalf("the item operation does not document ?expand: %+v", op.Parameters)
	}
	if found.Schema == nil || found.Schema.Items == nil || len(found.Schema.Items.Enum) != 1 ||
		found.Schema.Items.Enum[0] != "org" {
		t.Errorf("the parameter does not enumerate the relations: %+v", found.Schema)
	}
}

// And a resource with no relation documents no such parameter, so the generated
// client cannot offer one that would be refused.
func TestItemOperationOmitsExpandWhenNothingIsExpandable(t *testing.T) {
	db := newFakeDB(t, reply{cols: docCols(), rows: nil})
	api := mountDocs(t, db.db, nil)

	for _, p := range api.OpenAPI().Paths["/documents/{id}"].Get.Parameters {
		if p.Name == "expand" {
			t.Errorf("a resource with no expandable relation documents ?expand: %+v", p)
		}
	}
}

// cursorOf reads next_cursor from a list response, failing if it is absent.
func cursorOf(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, ok := body["next_cursor"]
	if !ok {
		t.Fatalf("response has no next_cursor: %v", body)
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		t.Fatalf("next_cursor = %v, want a non-empty string", raw)
	}
	return s
}

// A client should be able to page by cursor without ever having asked for it:
// the first response carries the position to resume from, so there is no flag
// to set and no first cursor to obtain some other way.
func TestListHandsBackACursorWhenThereIsMore(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{
		postRow("p1", "One"), postRow("p2", "Two"), postRow("p3", "Three"),
	}})
	api := mount(t, db.db, postOptions())

	resp := api.Get("/posts")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	body := decode(t, resp.Body.Bytes())
	if body["has_more"] != true {
		t.Fatalf("has_more = %v, want true", body["has_more"])
	}
	cursor := cursorOf(t, body)

	// Nothing sorted this request, so the ordering is the tiebreaker alone and
	// the cursor names the last row of the page — p2, not the p3 that was read
	// only to answer has_more.
	resp = api.Get("/posts?cursor=" + cursor)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	stmt := db.lastStatement()
	if !strings.Contains(stmt, `WHERE "id" > $1`) {
		t.Errorf("second page did not seek:\n%s", stmt)
	}
	if args := db.lastArgs(); len(args) != 1 || args[0] != "p2" {
		t.Errorf("seek bound %v, want the last row of the first page", args)
	}
}

// The cursor names the end of *this* page, so a last page has none — which is
// how a client knows to stop without comparing counts.
func TestListOmitsTheCursorOnTheLastPage(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "One")}})
	api := mount(t, db.db, postOptions())

	body := decode(t, api.Get("/posts").Body.Bytes())
	if body["has_more"] != false {
		t.Fatalf("has_more = %v, want false", body["has_more"])
	}
	if _, present := body["next_cursor"]; present {
		t.Errorf("last page carries a next_cursor: %v", body)
	}
}

// A sorted request produces a cursor over that sort, and feeding it back seeks
// on both the sort column and the tiebreaker.
func TestCursorCarriesTheRequestedSort(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{
		postRow("p1", "One"), postRow("p2", "Two"), postRow("p3", "Three"),
	}})
	api := mount(t, db.db, postOptions())

	body := decode(t, api.Get("/posts?sort=-view_count").Body.Bytes())
	cursor := cursorOf(t, body)

	resp := api.Get("/posts?sort=-view_count&cursor=" + cursor)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	stmt := db.lastStatement()
	// Both terms descend and neither column is nullable, so this is the row
	// comparison Postgres can answer with one index seek.
	if !strings.Contains(stmt, `WHERE ("view_count", "id") < ($1, $2)`) {
		t.Errorf("statement did not seek on the sort and the tiebreaker:\n%s", stmt)
	}
	if args := db.lastArgs(); len(args) != 2 || args[0] != int64(3) || args[1] != "p2" {
		t.Errorf("seek bound %v, want the last row's view_count and id", args)
	}
}

// Changing ?sort= and keeping the cursor is the ordinary way to reach an
// unusable one, so it is a 400 that says what happened rather than a 500.
func TestCursorFromADifferentSortIsRejected(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{
		postRow("p1", "One"), postRow("p2", "Two"), postRow("p3", "Three"),
	}})
	api := mount(t, db.db, postOptions())

	cursor := cursorOf(t, decode(t, api.Get("/posts?sort=-view_count").Body.Bytes()))

	resp := api.Get("/posts?sort=title&cursor=" + cursor)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.Code, resp.Body)
	}
	for _, want := range []string{"view_count desc", "title asc", "query.cursor"} {
		if !strings.Contains(resp.Body.String(), want) {
			t.Errorf("body does not mention %q:\n%s", want, resp.Body)
		}
	}
}

func TestCursorAndPageTogetherAreRejected(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "One")}})
	api := mount(t, db.db, postOptions())

	resp := api.Get("/posts?cursor=abc&page=2")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.Code, resp.Body)
	}
	if !strings.Contains(resp.Body.String(), "send one or the other") {
		t.Errorf("body should say which to drop:\n%s", resp.Body)
	}
}

// A count is the size of the result set, so it must not shrink as a client
// pages through it — otherwise a progress bar built on it would run backwards.
func TestCountIgnoresTheCursor(t *testing.T) {
	db := newFakeDB(t,
		reply{match: "count(*)", cols: []string{"count"}, rows: [][]any{{int64(97)}}},
		reply{cols: postCols(), rows: [][]any{
			postRow("p1", "One"), postRow("p2", "Two"), postRow("p3", "Three"),
		}},
	)
	api := mount(t, db.db, postOptions())

	cursor := cursorOf(t, decode(t, api.Get("/posts?count=exact").Body.Bytes()))
	body := decode(t, api.Get("/posts?count=exact&cursor="+cursor).Body.Bytes())

	if body["total"] != float64(97) {
		t.Errorf("total = %v, want the whole result set", body["total"])
	}
	for _, stmt := range db.statements() {
		if strings.Contains(stmt, "count(*)") && strings.Contains(stmt, `"id" > `) {
			t.Errorf("the count query carried the cursor boundary:\n%s", stmt)
		}
	}
}

// A group parameter has to survive the whole path — huma's parameter binding,
// filter.Parse, and filter.Apply — not just filter.Parse, which is where the
// package's own tests stop. `not` is the newest of the three (#98), and the
// failure this closes is a parser that understands a parameter the mounted
// resource never passes it.
func TestNotGroupReachesTheStatement(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	api := mount(t, db.db, postOptions())

	resp := api.Get("/posts?not=" + url.QueryEscape("(status.eq.draft,view_count.lt.3)"))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}

	stmt := db.lastStatement()
	// The bare list is the conjunction, negated — NOT (a AND b) — so both
	// conditions must be inside one NOT rather than negated separately.
	for _, want := range []string{`NOT (("status" = $1) AND ("view_count" < $2))`} {
		if !strings.Contains(stmt, want) {
			t.Errorf("statement missing %q:\n%s", want, stmt)
		}
	}
}

// Reads is the read-only mount, and it exposes no write.
//
// rest.Op mirrors schema.Op by hand — rest may not import schema, which is what
// keeps the runtime usable without the DSL — so the two Reads constants are two
// declarations of one fact. The bit values are asserted literally here rather
// than against schema's copy, because importing schema to compare them is the
// edge this package exists not to have.
func TestReadsExposesNoWrite(t *testing.T) {
	if rest.Reads != rest.OpRead|rest.OpList {
		t.Fatalf("rest.Reads = %v, want read|list", rest.Reads)
	}
	for _, w := range []struct {
		op   rest.Op
		name string
	}{{rest.OpCreate, "create"}, {rest.OpUpdate, "update"}, {rest.OpDelete, "delete"}} {
		if rest.Reads.Has(w.op) {
			t.Errorf("rest.Reads exposes %s: %v", w.name, rest.Reads)
		}
	}
	// The mirror: schema.Reads is OpRead|OpList over the same bit layout, so
	// both are 2|16 = 18. A change to either bitmask that did not change the
	// other lands here.
	if uint8(rest.Reads) != 2|16 {
		t.Fatalf("rest.Reads = %d; schema.Op's layout puts read|list at %d", uint8(rest.Reads), 2|16)
	}
}

// The body's spelling and the request's spelling are two tags on one field, and
// a resource whose two disagree serves an API its own document is wrong about:
// the response says createdAt and the filter only answers to created_at. That
// is the two spellings ADR-0036 exists to prevent, so it refuses to mount.
//
// Codegen cannot produce this — both tags come from one WireCase — but a
// hand-written model can, and so can a generated one edited by hand.
func TestMountRefusesDisagreeingSpellings(t *testing.T) {
	type Mismatched struct {
		ID        string `db:"id" json:"id" sqlb:"pk"`
		CreatedAt string `db:"created_at" json:"createdAt" sqlb:"filter"`
	}

	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("t", "1"))
	err := rest.Resource[Mismatched, rest.None[Mismatched], rest.None[Mismatched]](
		api, db.db, rest.Options{Path: "/things", Ops: rest.Reads})
	if err == nil {
		t.Fatal("a resource whose body and wire spellings disagree must not mount")
	}
	for _, want := range []string{"createdAt", "created_at", "one spelling"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not explain itself (missing %q):\n%v", want, err)
		}
	}
}

// The agreeing case, which is what codegen emits: the json tag and the sqlb
// wire entry say the same thing, and the resource mounts.
func TestMountAcceptsAgreeingSpellings(t *testing.T) {
	type Consistent struct {
		ID        string `db:"id" json:"id" sqlb:"pk"`
		CreatedAt string `db:"created_at" json:"createdAt" sqlb:"filter,wire:createdAt"`
	}

	db := newFakeDB(t)
	_, api := humatest.New(t, huma.DefaultConfig("t", "1"))
	if err := rest.Resource[Consistent, rest.None[Consistent], rest.None[Consistent]](
		api, db.db, rest.Options{Path: "/things", Ops: rest.Reads}); err != nil {
		t.Fatalf("a consistently spelled resource must mount: %v", err)
	}
}
