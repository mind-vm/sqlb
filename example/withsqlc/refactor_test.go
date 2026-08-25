package withsqlc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb/example/withsqlc"
	"github.com/mind-vm/sqlb/internal/pgfake"
)

// The four stages of docs/refactoring-from-sqlc.md, each asserted for the claim
// the document makes about it.
//
// What is *not* here: that the four return the same rows. These tests run
// against a stub that answers every statement with the same canned result, so
// an equivalence asserted here would hold no matter what SQL the stages sent —
// a check that reports success while verifying nothing, which is the failure
// ADR-0016 exists to name. The real equivalence needs a real planner and lives
// in pgtest/refactor_test.go, where four stages run against one Postgres and
// have to agree row for row.
//
// What these can answer, and what the document claims, is what each stage
// *sends* and what each stage *refuses*.

const org = "acme"

// Stage 1 sends every optional predicate on every request. This is the shape
// the document calls the kitchen sink, and it is asserted rather than described
// so nobody has to take the criticism on faith.
func TestStage1SendsEveryFilterEvenWhenNoneWereAsked(t *testing.T) {
	db := newStubDB(postColumns(), [][]any{postValues("p1", "Hello")})

	if _, err := withsqlc.ListPostsStage1(t.Context(), db, org, url.Values{}); err != nil {
		t.Fatalf("ListPostsStage1: %v", err)
	}

	stmt := db.last()
	for _, want := range []string{
		"$2::text IS NULL OR status = $2::text",
		"$3::bigint IS NULL OR view_count >= $3::bigint",
		"$4::text IS NULL OR title ILIKE",
	} {
		if !strings.Contains(stmt, want) {
			t.Errorf("stage 1 did not send the arm %q:\n%s", want, stmt)
		}
	}
}

// Stage 2 sends only what was asked, which is the first thing the move buys.
func TestStage2SendsOnlyTheFiltersAskedFor(t *testing.T) {
	db := newStubDB(postColumns(), [][]any{postValues("p1", "Hello")})

	_, err := withsqlc.ListPostsStage2(t.Context(), db, org, url.Values{
		"status": {"published"},
	})
	if err != nil {
		t.Fatalf("ListPostsStage2: %v", err)
	}

	stmt := db.last()
	if !strings.Contains(stmt, `"status" = $`) {
		t.Errorf("the filter that was asked for is missing:\n%s", stmt)
	}
	// The two that were not asked for are absent entirely, rather than present
	// and guarded by a NULL check.
	for _, absent := range []string{"view_count >=", "IS NULL OR"} {
		if strings.Contains(stmt, absent) {
			t.Errorf("stage 2 sent %q for a filter nobody asked for:\n%s", absent, stmt)
		}
	}
}

// The 2n-queries claim, proven in both directions: the sort stage 1 was
// generated for works, any other one is a query it does not have, and stages 2
// and 3 take it as a value.
func TestOnlyStage1NeedsASecondQueryToSortDifferently(t *testing.T) {
	byViews := url.Values{"sort": {"-view_count"}}

	t.Run("stage 1 refuses", func(t *testing.T) {
		db := newStubDB(postColumns(), nil)
		_, err := withsqlc.ListPostsStage1(t.Context(), db, org, byViews)
		if !errors.Is(err, withsqlc.ErrSortUnavailable) {
			t.Fatalf("err = %v, want ErrSortUnavailable", err)
		}
	})

	// The other direction, so the test above is not passing because stage 1
	// refuses everything.
	t.Run("stage 1 serves the sort it was generated for", func(t *testing.T) {
		db := newStubDB(postColumns(), nil)
		_, err := withsqlc.ListPostsStage1(t.Context(), db, org, url.Values{"sort": {"-published_at"}})
		if err != nil {
			t.Fatalf("ListPostsStage1: %v", err)
		}
		if !strings.Contains(db.last(), "ORDER BY published_at DESC") {
			t.Errorf("the baked-in ordering is missing:\n%s", db.last())
		}
	})

	t.Run("stage 2 takes it as a value", func(t *testing.T) {
		db := newStubDB(postColumns(), nil)
		if _, err := withsqlc.ListPostsStage2(t.Context(), db, org, byViews); err != nil {
			t.Fatalf("ListPostsStage2: %v", err)
		}
		if !strings.Contains(db.last(), `ORDER BY "view_count" DESC`) {
			t.Errorf("stage 2 did not sort by view_count:\n%s", db.last())
		}
	})

	t.Run("stage 3 takes it as a value", func(t *testing.T) {
		db := newStubDB(postColumns(), nil)
		if _, err := withsqlc.ListPostsStage3(t.Context(), db, org, byViews); err != nil {
			t.Fatalf("ListPostsStage3: %v", err)
		}
		if !strings.Contains(db.last(), `ORDER BY "view_count" DESC`) {
			t.Errorf("stage 3 did not sort by view_count:\n%s", db.last())
		}
	})
}

// Stage 3's rejection is the declared capability talking, and it names what
// would have worked. Stage 2's hand-written allow-list can only say no, and the
// contrast is the point rather than an incidental difference.
func TestStage3RefusesAnUndeclaredSortAndSaysWhatWouldWork(t *testing.T) {
	db := newStubDB(postColumns(), nil)

	// body is Searchable in the schema but not Sortable, so it exists, is not
	// hidden, and still may not be sorted on.
	_, err := withsqlc.ListPostsStage3(t.Context(), db, org, url.Values{"sort": {"body"}})
	if err == nil {
		t.Fatal("sorting on a column that did not declare Sortable should be refused")
	}
	if !strings.Contains(err.Error(), "published_at") {
		t.Errorf("the rejection does not name a column that would have worked: %v", err)
	}

	// And the guard fires for a column the client should not know exists.
	if _, err := withsqlc.ListPostsStage3(t.Context(), db, org, url.Values{
		"password_hash": {"eq.secret"},
	}); err == nil {
		t.Error("filtering an undeclared column should be refused")
	}
}

// Stage 4 has no list handler, so this is the assertion that the generated one
// does the same work: one HTTP request, and the statement carries the filter,
// the sort, and the two predicates that are now a hook rather than an argument.
func TestStage4ServesTheListWithNoHandlerWritten(t *testing.T) {
	db := newStubDB(postColumns(), [][]any{postValues("p1", "Hello")})
	server, err := withsqlc.ServerStage4(db)
	if err != nil {
		t.Fatalf("ServerStage4: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/posts?status=eq.draft&sort=-view_count&per_page=5", nil)
	req = req.WithContext(withsqlc.WithOrg(req.Context(), org))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	stmt := db.last()
	for _, want := range []string{
		`"status" = $`,               // the filter, from the grammar
		`ORDER BY "view_count" DESC`, // the sort, from the grammar
		`"org_id" = $`,               // the hook, which no request mentioned
		`"deleted_at" IS NULL`,       // the hook
		"LIMIT 6",                    // per_page plus the has-more probe
	} {
		if !strings.Contains(stmt, want) {
			t.Errorf("statement missing %q:\n%s", want, stmt)
		}
	}

	var body struct {
		Items   []map[string]any `json:"items"`
		PerPage int              `json:"per_page"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body, err)
	}
	if len(body.Items) != 1 || body.Items[0]["title"] != "Hello" {
		t.Errorf("items = %v", body.Items)
	}
	if body.PerPage != 5 {
		t.Errorf("per_page = %d, want 5", body.PerPage)
	}
}

// The direction that matters more than the one above: a request no middleware
// scoped must fail rather than return every tenant's rows. Stage 4 moved the
// tenant predicate off the call site, so this is the assertion that moving it
// did not make it optional.
func TestStage4RefusesAnUnscopedRequest(t *testing.T) {
	db := newStubDB(postColumns(), [][]any{postValues("p1", "Hello")})
	server, err := withsqlc.ServerStage4(db)
	if err != nil {
		t.Fatalf("ServerStage4: %v", err)
	}

	// No WithOrg on this context.
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/posts", nil))

	if rec.Code == http.StatusOK {
		t.Fatalf("an unscoped request was served: %s", rec.Body)
	}
	if stmt := db.last(); strings.Contains(stmt, "SELECT") {
		t.Errorf("an unscoped request reached the database:\n%s", stmt)
	}
}

// stubDB answers every statement with the same canned result and records what
// it was asked. Modelled on example/blog/server_test.go's, and kept separate
// rather than shared: what a stub answers is a policy of the package testing
// with it, and these assertions are about the SQL rather than the scanning.
type stubDB struct {
	mu   sync.Mutex
	log  []string
	cols []string
	rows [][]any
}

func newStubDB(cols []string, rows [][]any) *stubDB {
	return &stubDB{cols: cols, rows: rows}
}

func (s *stubDB) record(q string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, q)
}

// last is the most recent statement, skipping the transaction markers a write
// is wrapped in.
func (s *stubDB) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.log) - 1; i >= 0; i-- {
		switch s.log[i] {
		case "BEGIN", "COMMIT", "ROLLBACK":
		default:
			return s.log[i]
		}
	}
	return ""
}

func (s *stubDB) Query(_ context.Context, query string, _ ...any) (pgx.Rows, error) {
	s.record(query)
	return &pgfake.Rows{Cols: s.cols, Data: s.rows}, nil
}

func (s *stubDB) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	s.record(query)
	return pgconn.NewCommandTag(fmt.Sprintf("SELECT %d", len(s.rows))), nil
}

// The generated handlers wrap a write in a transaction so a hook can register
// AfterCommit work, so the stub has to be able to open one.
func (s *stubDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	s.record("BEGIN")
	return &pgfake.Tx{
		Statements: s,
		OnCommit:   func() error { s.record("COMMIT"); return nil },
		OnRollback: func() error { s.record("ROLLBACK"); return nil },
	}, nil
}

// QueryRow is what sqlcgen's :one queries use. Stage 1's ListPosts is :many, so
// nothing here reaches it, but DBTX requires it.
func (s *stubDB) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	s.record(query)
	return &pgfake.Rows{Cols: s.cols, Data: s.rows}
}

func postColumns() []string {
	return []string{
		"id", "org_id", "author_id", "title", "body", "status",
		"view_count", "published_at", "created_at", "updated_at", "deleted_at",
	}
}

func postValues(id, title string) []any {
	now := time.Unix(0, 0).UTC()
	return []any{
		id, org, "a1", title, "body text", "draft",
		int64(3), now, now, now, nil,
	}
}
