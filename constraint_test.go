package sqlb_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

// A constraint violation is the caller's own mistake far more often than it is
// an outage, so it has to be tellable from one without matching on a message.
func TestConstraintViolationIsClassified(t *testing.T) {
	cases := []struct {
		sqlstate string
		want     sqlb.ConstraintKind
	}{
		{"23505", sqlb.ConstraintUnique},
		{"23503", sqlb.ConstraintForeignKey},
		{"23514", sqlb.ConstraintCheck},
		{"23502", sqlb.ConstraintNotNull},
		{"23P01", sqlb.ConstraintExclusion},
	}
	for _, tc := range cases {
		t.Run(tc.sqlstate, func(t *testing.T) {
			h := newHarness(t, nil, nil)
			defer h.close()
			h.failWithErr(&pgErr{code: tc.sqlstate, message: "refused"})

			u := &User{Email: "ada@example.com", Name: "Ada"}
			_, err := sqlb.InsertRows(u).Exec(context.Background(), h.db)
			if err == nil {
				t.Fatal("expected the insert to fail")
			}
			if !errors.Is(err, sqlb.ErrConstraint) {
				t.Errorf("errors.Is(err, ErrConstraint) = false for %s", tc.sqlstate)
			}
			var ce *sqlb.ConstraintError
			if !errors.As(err, &ce) {
				t.Fatalf("errors.As did not find a ConstraintError in %v", err)
			}
			if ce.Kind != tc.want {
				t.Errorf("kind = %q, want %q", ce.Kind, tc.want)
			}
		})
	}
}

// A code outside class 23 is not the caller's fault and must not be dressed up
// as it. Without this the check would report every driver error as a
// constraint violation, and its silence about real ones would mean nothing.
func TestNonConstraintErrorsAreNotClassified(t *testing.T) {
	for _, code := range []string{"42601", "42703", "08006", "40P01"} {
		h := newHarness(t, nil, nil)
		h.failWithErr(&pgErr{code: code, message: "not a constraint"})

		u := &User{Email: "ada@example.com"}
		_, err := sqlb.InsertRows(u).Exec(context.Background(), h.db)
		if err == nil {
			h.close()
			t.Fatalf("%s: expected the insert to fail", code)
		}
		if errors.Is(err, sqlb.ErrConstraint) {
			t.Errorf("%s was classified as a constraint violation", code)
		}
		h.close()
	}
}

// The wrapped error still carries the statement, because that is what a log
// needs. What must not happen is the reverse: rest returning it to a client,
// which rest_test asserts from the other side.
func TestConstraintErrorKeepsTheStatementForLogs(t *testing.T) {
	h := newHarness(t, nil, nil)
	defer h.close()
	h.failWithErr(&pgErr{code: "23505", message: "duplicate key"})

	u := &User{Email: "ada@example.com"}
	_, err := sqlb.InsertRows(u).Exec(context.Background(), h.db)
	if err == nil {
		t.Fatal("expected the insert to fail")
	}
	if !strings.Contains(err.Error(), "INSERT INTO") {
		t.Errorf("the error should carry the statement for a log, got: %v", err)
	}
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("the error should carry what the database said, got: %v", err)
	}
}

// The constraint *name* is the field an application branches on, and every
// driver exposes it as a struct field rather than as a method — so it is
// reachable only by naming the driver, which this library will not do. The
// classifier is that seam, and this is the shape the doc comment promises.
func TestRegisteredClassifierSuppliesTheConstraintName(t *testing.T) {
	sqlb.SetErrorClassifier(func(err error) (sqlb.ConstraintError, bool) {
		var pg *pgErr
		if !errors.As(err, &pg) {
			return sqlb.ConstraintError{}, false
		}
		kind, ok := sqlb.ConstraintKindOf(pg.SQLState())
		if !ok {
			return sqlb.ConstraintError{}, false
		}
		return sqlb.ConstraintError{Kind: kind, Constraint: pg.constraint}, true
	})
	defer sqlb.SetErrorClassifier(nil)

	h := newHarness(t, nil, nil)
	defer h.close()
	h.failWithErr(&pgErr{
		code:       "23505",
		constraint: "loans_one_open_per_book_per_borrower",
		message:    "duplicate key value violates unique constraint",
	})

	u := &User{Email: "ada@example.com"}
	_, err := sqlb.InsertRows(u).Exec(context.Background(), h.db)

	var ce *sqlb.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("errors.As did not find a ConstraintError in %v", err)
	}
	// The point of the whole exercise: a value to compare, not a substring to
	// search for. A rename of the index is now a change to this comparison
	// rather than a branch that silently stops firing.
	if ce.Constraint != "loans_one_open_per_book_per_borrower" {
		t.Errorf("constraint = %q, want the declared index name", ce.Constraint)
	}
	if ce.Kind != sqlb.ConstraintUnique {
		t.Errorf("kind = %q, want unique", ce.Kind)
	}
}

// A classifier that does not recognise an error must not veto the built-in
// check: it may know one driver and be handed an error from another.
func TestClassifierDecliningFallsBackToSQLState(t *testing.T) {
	sqlb.SetErrorClassifier(func(error) (sqlb.ConstraintError, bool) {
		return sqlb.ConstraintError{}, false
	})
	defer sqlb.SetErrorClassifier(nil)

	h := newHarness(t, nil, nil)
	defer h.close()
	h.failWithErr(&pgErr{code: "23503", message: "foreign key"})

	u := &User{Email: "ada@example.com"}
	_, err := sqlb.InsertRows(u).Exec(context.Background(), h.db)
	if !errors.Is(err, sqlb.ErrConstraint) {
		t.Errorf("a declining classifier suppressed the built-in check: %v", err)
	}
}
