package rest_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// The gap this closes: nothing in rest opened a transaction, so every generated
// write ran under autocommit and sqlb.AfterCommit — documented as the place for
// work that must not happen if the write aborts — was unreachable from any
// generated handler (ADR-0021).

// rowsOf is the one-row result a create, update or delete RETURNING produces.
func rowsOf(row []any) [][]any { return [][]any{row} }

func TestGeneratedCreateIsWrapped(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	api := mount(t, db.db, postOptions())

	resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}
	assertWrapped(t, db.statements(), "INSERT")
}

func TestGeneratedUpdateIsWrapped(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "New"))})
	api := mount(t, db.db, postOptions())

	resp := api.Patch("/posts/p1", map[string]any{"title": "New"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	assertWrapped(t, db.statements(), "UPDATE")
}

func TestGeneratedDeleteIsWrapped(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	api := mount(t, db.db, postOptions())

	resp := api.Delete("/posts/p1")
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", resp.Code, resp.Body)
	}
	assertWrapped(t, db.statements(), "DELETE")
}

// A read is already atomic. Wrapping it would hold a connection across a
// round trip for a guarantee it already had.
func TestReadsAreNotWrapped(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	api := mount(t, db.db, postOptions())

	if resp := api.Get("/posts/p1"); resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	for _, stmt := range db.statements() {
		if stmt == "BEGIN" {
			t.Errorf("a read opened a transaction: %v", db.statements())
		}
	}
}

// The point of the change: a hook can now register post-commit work from a
// generated write, and it runs.
func TestAfterCommitIsReachableFromAGeneratedWrite(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})

	scoped := sqlb.NewRegistry()
	published := 0
	sqlb.On[Post](scoped).AfterCreate(func(ctx context.Context, _ *Post) error {
		return sqlb.AfterCommit(ctx, func(context.Context) error {
			published++
			return nil
		})
	})

	api := mount(t, sqlb.New(db.db).WithHooks(scoped), postOptions())
	resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}
	if published != 1 {
		t.Errorf("the after-commit callback ran %d times, want 1 — it is unreachable from generated CRUD", published)
	}
}

// A callback failing is not the request failing. The row is durable, so a 5xx
// would invite a retry that writes it twice.
func TestAfterCommitFailureStillReports201(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})

	scoped := sqlb.NewRegistry()
	sqlb.On[Post](scoped).AfterCreate(func(ctx context.Context, _ *Post) error {
		return sqlb.AfterCommit(ctx, func(context.Context) error {
			return errors.New("broker unreachable")
		})
	})

	api := mount(t, sqlb.New(db.db).WithHooks(scoped), postOptions())
	resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — the write committed: %s", resp.Code, resp.Body)
	}
	// And it committed rather than rolling back.
	assertWrapped(t, db.statements(), "INSERT")
}

// A hook refusing the write rolls it back, and the callbacks registered before
// the refusal do not fire.
func TestHookRefusalRollsBackTheGeneratedWrite(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})

	scoped := sqlb.NewRegistry()
	fired := false
	sqlb.On[Post](scoped).AfterCreate(func(ctx context.Context, _ *Post) error {
		if err := sqlb.AfterCommit(ctx, func(context.Context) error {
			fired = true
			return nil
		}); err != nil {
			return err
		}
		return errors.New("domain rule said no")
	})

	api := mount(t, sqlb.New(db.db).WithHooks(scoped), postOptions())
	resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"})
	if resp.Code == http.StatusCreated {
		t.Fatalf("a refused create reported success: %s", resp.Body)
	}
	if fired {
		t.Error("an after-commit callback fired for a write that rolled back")
	}
	stmts := db.statements()
	if stmts[len(stmts)-1] != "ROLLBACK" {
		t.Errorf("statements = %v, want them to end in ROLLBACK", stmts)
	}
}

// A delete matching nothing is a 404, and must not commit a transaction whose
// callbacks would then announce a deletion that did not happen.
func TestDeleteMatchingNothingRollsBack(t *testing.T) {
	db := newFakeDB(t, reply{match: "DELETE", cols: postCols()})
	api := mount(t, db.db, postOptions())

	resp := api.Delete("/posts/p1")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body)
	}
	stmts := db.statements()
	if stmts[len(stmts)-1] != "ROLLBACK" {
		t.Errorf("statements = %v, want them to end in ROLLBACK", stmts)
	}
}

func TestDisableTransactionsOptsOut(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	opts := postOptions()
	opts.DisableTransactions = true
	api := mount(t, db.db, opts)

	resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}
	for _, stmt := range db.statements() {
		if stmt == "BEGIN" {
			t.Errorf("DisableTransactions still wrapped the write: %v", db.statements())
		}
	}
}

// Refusing at startup rather than on the first POST. Falling back to autocommit
// would restore the exact gap this change removes, and would do it silently.
func TestMountRefusesAnExecutorThatCannotBegin(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols()})
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	err := rest.Resource[Post, PostCreate, PostUpdate](api, execOnly{inner: db.db}, postOptions())
	if err == nil {
		t.Fatal("mounting over an executor that cannot begin a transaction should fail")
	}
	for _, want := range []string{"BeginTx", "DisableTransactions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not offer %q as a way out", err, want)
		}
	}
}

// The same executor mounts fine once the resource says it does not need a
// transaction, which is what makes the refusal above a choice rather than a
// wall (ADR-0016: a guard is proven by both directions).
func TestExecutorThatCannotBeginMountsWithTransactionsDisabled(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols()})
	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))

	opts := postOptions()
	opts.DisableTransactions = true
	if err := rest.Resource[Post, PostCreate, PostUpdate](api, execOnly{inner: db.db}, opts); err != nil {
		t.Fatalf("mounting with transactions disabled: %v", err)
	}
}

// execOnly implements Executor and nothing else — the tracer wrapper the README
// documents, and the case the startup check has to name helpfully.
type execOnly struct{ inner sqlb.Executor }

func (e execOnly) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
	return e.inner.Query(ctx, q, args...)
}

func (e execOnly) Exec(ctx context.Context, q string, args ...any) (pgconn.CommandTag, error) {
	return e.inner.Exec(ctx, q, args...)
}

// assertWrapped checks the statement log is BEGIN, the write, then COMMIT.
func assertWrapped(t *testing.T, stmts []string, verb string) {
	t.Helper()
	if len(stmts) < 3 {
		t.Fatalf("statements = %v, want the write wrapped in a transaction", stmts)
	}
	if stmts[0] != "BEGIN" {
		t.Errorf("statements = %v, want them to open with BEGIN", stmts)
	}
	if stmts[len(stmts)-1] != "COMMIT" {
		t.Errorf("statements = %v, want them to end with COMMIT", stmts)
	}
	found := false
	for _, s := range stmts {
		if strings.Contains(s, verb) {
			found = true
		}
	}
	if !found {
		t.Errorf("statements = %v, want one to be a %s", stmts, verb)
	}
}
