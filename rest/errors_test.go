package rest_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jryannel/sqlb"
)

// pgErr stands in for a driver error carrying a SQLSTATE, which is how the
// engine recognises a constraint violation without importing a driver.
type pgErr struct {
	code    string
	message string
}

func (e *pgErr) Error() string    { return "ERROR: " + e.message + " (SQLSTATE " + e.code + ")" }
func (e *pgErr) SQLState() string { return e.code }

// A duplicate value is the client's own mistake, and it arrives at the handler
// as a driver error. Answering 500 sends the caller looking for an outage that
// is not there; 409 says what actually happened.
func TestUniqueViolationAnswers409(t *testing.T) {
	db := newFakeDB(t, reply{err: &pgErr{
		code:    "23505",
		message: `duplicate key value violates unique constraint "posts_org_id_title_key"`,
	}})
	api := mount(t, db.db, postOptions())

	resp := api.Post("/posts", map[string]any{
		"org_id": "acme", "title": "Hello", "body": "text",
	})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.Code, resp.Body)
	}
}

// A foreign key or check violation is not a conflict with an existing row: the
// entity is wrong, and no later state of the database makes it right.
func TestOtherConstraintViolationsAnswer422(t *testing.T) {
	for _, code := range []string{"23503", "23514", "23502"} {
		db := newFakeDB(t, reply{err: &pgErr{code: code, message: "refused"}})
		api := mount(t, db.db, postOptions())

		resp := api.Post("/posts", map[string]any{
			"org_id": "acme", "title": "Hello", "body": "text",
		})
		if resp.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", code, resp.Code, resp.Body)
		}
	}
}

// The engine annotates a driver error with the statement that failed, which is
// what a log needs. Returning it is how that becomes a way to read the schema
// off a public endpoint: post a duplicate and the response names the table,
// its columns and the constraint that refused.
//
// This asserts on the body of a 500 rather than on its status, because the
// status was never the problem.
func TestServerErrorsDoNotLeakTheStatement(t *testing.T) {
	db := newFakeDB(t, reply{err: errors.New(
		`pq: relation "posts_secret_idx" does not exist`)})
	api := mount(t, db.db, postOptions())

	resp := api.Post("/posts", map[string]any{
		"org_id": "acme", "title": "Hello", "body": "text",
	})
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", resp.Code, resp.Body)
	}
	body := resp.Body.String()
	for _, leaked := range []string{
		"INSERT INTO",      // the compiled statement
		"posts_secret_idx", // what the database named
		`"org_id"`,         // column names from the statement
		"sqlb:",            // the engine's own framing
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("the 500 body leaks %q:\n%s", leaked, body)
		}
	}
}

// The same leak is reachable through a read, where the statement carries
// whatever the filter grammar compiled from the query string.
func TestServerErrorsOnListDoNotLeakTheStatement(t *testing.T) {
	db := newFakeDB(t, reply{err: errors.New("connection reset")})
	api := mount(t, db.db, postOptions())

	resp := api.Get("/posts?title=eq.Hello")
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", resp.Code, resp.Body)
	}
	if strings.Contains(resp.Body.String(), "SELECT") {
		t.Errorf("the 500 body leaks the statement:\n%s", resp.Body)
	}
}

// The constraint name is available to Go callers on sqlb.ConstraintError,
// which is where branching on it belongs. It must not be in the response: a
// client that can provoke one constraint by name can enumerate the rest.
func TestConstraintResponseDoesNotNameTheConstraint(t *testing.T) {
	sqlb.SetErrorClassifier(func(err error) (sqlb.ConstraintError, bool) {
		var pg *pgErr
		if !errors.As(err, &pg) {
			return sqlb.ConstraintError{}, false
		}
		kind, ok := sqlb.ConstraintKindOf(pg.SQLState())
		if !ok {
			return sqlb.ConstraintError{}, false
		}
		return sqlb.ConstraintError{Kind: kind, Constraint: "posts_org_id_title_key"}, true
	})
	defer sqlb.SetErrorClassifier(nil)

	db := newFakeDB(t, reply{err: &pgErr{code: "23505", message: "duplicate key"}})
	api := mount(t, db.db, postOptions())

	resp := api.Post("/posts", map[string]any{
		"org_id": "acme", "title": "Hello", "body": "text",
	})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.Code, resp.Body)
	}
	if strings.Contains(resp.Body.String(), "posts_org_id_title_key") {
		t.Errorf("the response names the constraint:\n%s", resp.Body)
	}
}

// Sanitising the unclassified case must not swallow the classified ones an
// application makes for itself. A hook returning huma.Error403Forbidden because
// the caller lacks a role has already decided the answer; replacing it with a
// generic 500 turns every deliberate refusal into an apparent outage.
func TestAnErrorCarryingItsOwnStatusIsNotSanitised(t *testing.T) {
	reg := sqlb.NewRegistry()
	sqlb.On[Post](reg).BeforeCreate(func(context.Context, *Post) error {
		return huma.Error403Forbidden("posting needs the author role")
	})

	db := newFakeDB(t, reply{cols: postCols()})
	api := mount(t, sqlb.New(db.db).WithHooks(reg), postOptions())

	resp := api.Post("/posts", map[string]any{
		"org_id": "acme", "title": "Hello", "body": "text",
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.Code, resp.Body)
	}
	if !strings.Contains(resp.Body.String(), "author role") {
		t.Errorf("the hook's own message should reach the client: %s", resp.Body)
	}
}

// The other half of the same seam: a hook that returned a plain error meant to
// refuse, and got a 500 for it.
//
// Nothing can fix that automatically — an unclassified error is unclassified —
// so the check is on the one channel that reaches the person who wrote the
// hook. #293 reports shipping exactly this and finding it only by asserting on
// a status code; the sentence that would have explained it was in a comment in
// rest/errors.go, which is not where someone reading their server's log is
// looking.
func TestTheUnclassifiedLogNamesTheStatusAHookShouldHaveReturned(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	reg := sqlb.NewRegistry()
	sqlb.On[Post](reg).BeforeCreate(func(context.Context, *Post) error {
		return errors.New("only a parent may add a child")
	})

	db := newFakeDB(t, reply{cols: postCols()})
	api := mount(t, sqlb.New(db.db).WithHooks(reg), postOptions())

	resp := api.Post("/posts", map[string]any{
		"org_id": "acme", "title": "Hello", "body": "text",
	})
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", resp.Code, resp.Body)
	}
	logged := buf.String()
	if !strings.Contains(logged, "huma.Error403Forbidden") {
		t.Errorf("the log line should name the call that turns this into a refusal:\n%s", logged)
	}
	// The response must not gain the hint along with the log: it is advice for
	// whoever wrote the hook, not for whoever provoked it.
	if strings.Contains(resp.Body.String(), "huma.Error403Forbidden") {
		t.Errorf("the hint reached the client:\n%s", resp.Body)
	}
}

// A deferred driver error — one the driver reports while the rows are being
// read rather than when the statement is sent, which pgx does on the extended
// protocol — must classify identically. Otherwise whether a duplicate answers
// 409 or 500 depends on which driver is underneath.
func TestConstraintReportedAtScanTimeIsStillClassified(t *testing.T) {
	db := newFakeDB(t, reply{
		cols:    postCols(),
		rows:    [][]any{postRow("p1", "Hello")},
		rowsErr: &pgErr{code: "23505", message: "duplicate key"},
	})
	api := mount(t, db.db, postOptions())

	resp := api.Post("/posts", map[string]any{
		"org_id": "acme", "title": "Hello", "body": "text",
	})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.Code, resp.Body)
	}
}
