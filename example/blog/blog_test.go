package blog_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/blog"
	_ "github.com/mind-vm/sqlb/example/blog/blogschema"
	"github.com/mind-vm/sqlb/filter"
	"github.com/mind-vm/sqlb/schema"
)

func TestSchemaIsValid(t *testing.T) {
	if err := schema.Validate(); err != nil {
		t.Fatalf("the example schema does not validate: %v", err)
	}
}

// TestListHandler is the payoff the whole design is aimed at: the entire
// HTTP-to-SQL layer for a dynamic, filterable, sortable, searchable, paginated
// list endpoint.
//
// It does not cover hooks, and it used to look as though it did — it registered
// a BeforeQuery scoping posts by org and hiding soft-deleted rows, and asserted
// nothing about either, because it could not: see the note beside the handler
// call below. server_test.go exercises the hooks, through a handle that carries
// them.
func TestListHandler(t *testing.T) {
	// The handler. This is the whole thing.
	//
	// Expandable is left empty on purpose. filter.Apply would perform the join
	// — blog.Post declares author expandable — but this test is about the
	// filter grammar reaching a hand-written handler, and an expansion here
	// would only add a column to every assertion below.
	opts := filter.Options{
		Model: sqlb.ModelOf[blog.Post](),
	}
	var lastSQL string
	handler := func(w http.ResponseWriter, r *http.Request) {
		q, err := filter.Parse(r.URL.Query(), opts)
		if err != nil {
			if !filter.WriteError(w, err) {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		sql, _, err := filter.Apply(sqlb.Query[blog.Post](), q).SQL()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		lastSQL = sql
		w.WriteHeader(http.StatusOK)
	}

	req := httptest.NewRequest("GET",
		"/posts?status=in.published,review&view_count=gte.100&search=postgres&sort=-published_at&page=2&per_page=10",
		nil)
	req = req.WithContext(context.WithValue(context.Background(), orgKey{}, "acme"))
	rec := httptest.NewRecorder()

	// SQL() compiles what the handler assembled. Query hooks are applied inside
	// All/Count rather than at compile time, so the tenant scoping is verified
	// against a live executor in the engine's own tests, not here.
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	for _, want := range []string{
		`"status" IN ($1, $2)`,
		`"view_count" >= $3`,
		`("title" ILIKE $4) OR ("body" ILIKE $5)`,
		`ORDER BY "published_at" DESC`,
		`LIMIT 10 OFFSET 10`,
	} {
		if !strings.Contains(lastSQL, want) {
			t.Errorf("compiled SQL is missing %s\ngot: %s", want, lastSQL)
		}
	}
}

// TestRejectionsAreActionable checks the error contract: a rejected parameter
// says what was wrong and what would have been accepted.
func TestRejectionsAreActionable(t *testing.T) {
	opts := filter.Options{Model: sqlb.ModelOf[blog.Post]()}
	req := httptest.NewRequest("GET", "/posts?body=eq.x&sort=body", nil)

	_, err := filter.Parse(req.URL.Query(), opts)
	if err == nil {
		t.Fatal("expected the request to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not sortable") {
		t.Errorf("error should explain the sort rejection: %s", msg)
	}
	if !strings.Contains(msg, "title") {
		t.Errorf("error should list the sortable columns: %s", msg)
	}
	// body is searchable, and searchable implies filterable, so filtering it
	// is legitimate; only the sort should have been rejected.
	if strings.Count(msg, ";") > 0 {
		t.Errorf("only one problem was expected, got: %s", msg)
	}
}

// TestHiddenColumnStaysHidden is the property that makes exposing a table safe.
func TestHiddenColumnStaysHidden(t *testing.T) {
	opts := filter.Options{Model: sqlb.ModelOf[blog.Author]()}

	// It is not selectable.
	if _, err := filter.Parse(values("select=password_hash"), opts); err == nil {
		t.Error("password_hash should not be selectable")
	}
	// It is not filterable, and the rejection does not confirm it exists.
	_, err := filter.Parse(values("password_hash=eq.abc"), opts)
	if err == nil {
		t.Fatal("password_hash should not be filterable")
	}
	if strings.Contains(err.Error(), "not filterable") {
		t.Errorf("the rejection confirms the column exists: %s", err)
	}
	// It is absent from the response projection.
	for _, col := range sqlb.ModelOf[blog.Author]().Selectable() {
		if col.Name == "password_hash" {
			t.Error("password_hash is in the REST projection")
		}
	}
}

// TestDashboardAggregate covers the grouping case that static query generators
// force into hand-written SQL.
func TestDashboardAggregate(t *testing.T) {
	type StatusCount struct {
		Status blog.PostStatus `db:"status"`
		N      int64           `db:"n"`
		Views  int64           `db:"views"`
	}

	q := sqlb.Query[blog.Post]().
		Select(
			sqlb.F("status"),
			sqlb.Count().As("n"),
			sqlb.Sum(sqlb.F("view_count")).As("views"),
		).
		Where(sqlb.F("deleted_at").IsNull()).
		GroupBy(sqlb.F("status")).
		OrderBy(sqlb.OrderByDesc(sqlb.Raw{SQL: "count(*)"}))

	sql, _, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	want := `SELECT "status", count(*) AS "n", sum("view_count") AS "views" FROM "posts"` +
		` WHERE "deleted_at" IS NULL GROUP BY "status" ORDER BY count(*) DESC`
	if sql != want {
		t.Errorf("SQL\n got: %s\nwant: %s", sql, want)
	}

	// The result shape is declared where it is used, and Collect scans into it.
	_ = StatusCount{}
}

// orgKey is how a real request carries its tenant. Nothing in this file reads
// it back: the hook that would is registered in server_test.go, against a
// handle that can run it.
type orgKey struct{}

func values(query string) map[string][]string {
	req := httptest.NewRequest("GET", "/?"+query, nil)
	return req.URL.Query()
}
