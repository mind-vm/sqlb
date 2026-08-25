package recipes_test

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// The sentinel errors, and what each one means:
//
//	ErrNotFound     One or First matched nothing
//	ErrConstraint   the database refused the write
//	ErrUnscoped     an update or delete with no Where
//	ErrBadCursor    ?cursor= did not decode against this ordering
//	ErrAfterCommit  the transaction committed, but a callback failed
//
// Every one is testable with errors.Is, so a handler branches on the class
// rather than on the text of a message.
func Example_errorsSentinels() {
	_, err := sqlb.Query[recipes.Post]().
		Where(sqlb.F("id").Eq("nope")).
		One(context.Background(), recordingDBWith(postColumns))

	fmt.Println("is ErrNotFound:", errors.Is(err, sqlb.ErrNotFound))
	fmt.Println("message:       ", err)
	// Output:
	// is ErrNotFound: true
	// message:        sqlb: no rows matched
}

// A constraint violation is the caller's mistake far more often than it is an
// outage: a second signup on a taken email, an order naming a product that was
// deleted, a balance a CHECK will not let go negative. Without this they arrive
// as an opaque driver error, and the only way to tell them apart is to match on
// the text of a message — which no rename survives.
//
// errors.Is is the cheap test for the class; errors.As gets the detail. The
// constraint *name* is the field that carries the value, because it is what
// lets a handler say "that email is taken" rather than "something was already
// there" — and it is filled in with no registration, because ADR-0040 settled
// which driver sqlb reads.
func Example_errorsConstraintViolation() {
	db := failingDB(&pgconn.PgError{
		Code:           "23505",
		ConstraintName: "authors_email_key",
		TableName:      "authors",
		Message:        `duplicate key value violates unique constraint "authors_email_key"`,
	})

	author := recipes.Author{Email: "ada@example.com"}
	_, err := sqlb.InsertRows(&author).One(context.Background(), db)

	fmt.Println("is ErrConstraint:", errors.Is(err, sqlb.ErrConstraint))

	var ce *sqlb.ConstraintError
	if errors.As(err, &ce) {
		fmt.Printf("%s on %s.%s\n", ce.Kind, ce.Table, ce.Constraint)

		// Which is what a handler branches on — the name the schema declares,
		// not prose from the database.
		if ce.Constraint == "authors_email_key" {
			fmt.Println("response: that email address is already registered")
		}
	}
	// Output:
	// is ErrConstraint: true
	// unique on authors.authors_email_key
	// response: that email address is already registered
}

// The five kinds are SQLSTATE class 23, named as a schema names them rather
// than as Postgres numbers them, so a switch reads the way the declaration
// does.
func Example_errorsConstraintKinds() {
	for _, code := range []string{"23505", "23503", "23514", "23502", "23P01", "42P01"} {
		kind, ok := sqlb.ConstraintKindOf(code)
		fmt.Printf("%-6s %-12s %v\n", code, kind, ok)
	}
	// Output:
	// 23505  unique       true
	// 23503  foreign_key  true
	// 23514  check        true
	// 23502  not_null     true
	// 23P01  exclusion    true
	// 42P01               false
}

// An error that is not a constraint violation stays what it was. A syntax error
// or a dead connection must never arrive dressed as the caller's fault, which
// is why the classification is a whitelist of class 23 rather than "a write
// failed, so blame the input".
func Example_errorsOtherFailuresAreNotConstraints() {
	db := failingDB(&pgconn.PgError{Code: "08006", Message: "connection failure"})

	author := recipes.Author{Email: "ada@example.com"}
	_, err := sqlb.InsertRows(&author).One(context.Background(), db)

	fmt.Println("is ErrConstraint:", errors.Is(err, sqlb.ErrConstraint))

	// The driver's own error is wrapped rather than replaced, so a caller that
	// does depend on pgx loses nothing.
	var pg *pgconn.PgError
	fmt.Println("PgError reachable:", errors.As(err, &pg), pg.Code)
	// Output:
	// is ErrConstraint: false
	// PgError reachable: true 08006
}

// opaqueError is a failure that has lost its type on the way up — a pool or a
// middleware that rendered the driver's error as text. errors.As cannot see a
// *pgconn.PgError inside it, because there is not one in there any more.
type opaqueError struct{ text string }

func (e opaqueError) Error() string { return e.text }

// SetErrorClassifier is what remains for that case, and it is rarely needed
// now. It used to be the only way to reach the constraint name at all — sqlb
// depended on the standard library alone and would not name a driver — and
// anyone who registered one for that reason can delete it.
//
// A registered classifier that declines is not a veto: it may know one driver
// and be handed an error from another, so the built-in check still runs after
// it. Call it once at startup, before serving.
func Example_errorsCustomClassifier() {
	sqlb.SetErrorClassifier(func(err error) (sqlb.ConstraintError, bool) {
		var opaque opaqueError
		if !errors.As(err, &opaque) {
			return sqlb.ConstraintError{}, false
		}
		_, rest, ok := strings.Cut(opaque.text, "SQLSTATE ")
		if !ok {
			return sqlb.ConstraintError{}, false
		}
		kind, ok := sqlb.ConstraintKindOf(strings.TrimRight(rest, ")"))
		if !ok {
			return sqlb.ConstraintError{}, false
		}
		return sqlb.ConstraintError{Kind: kind}, true
	})
	defer sqlb.SetErrorClassifier(nil) // an application never does this

	db := failingDB(opaqueError{text: "ERROR: duplicate key (SQLSTATE 23505)"})

	author := recipes.Author{Email: "ada@example.com"}
	_, err := sqlb.InsertRows(&author).One(context.Background(), db)

	var ce *sqlb.ConstraintError
	fmt.Println("classified:", errors.As(err, &ce), ce.Kind)
	// Output:
	// classified: true unique
}

// After-commit callbacks run once the transaction has committed, so a failure
// in one cannot roll anything back. A failing callback does not stop the others
// either — these are independent side effects, and abandoning the rest leaves
// more inconsistency rather than less. The failures come back joined under
// ErrAfterCommit.
func Example_errorsAfterCommitFailure() {
	db := recordingDB()

	err := db.WithTx(context.Background(), func(ctx context.Context, _ *sqlb.DB) error {
		if err := sqlb.AfterCommit(ctx, func(context.Context) error {
			return errors.New("the event bus was down")
		}); err != nil {
			return err
		}
		return sqlb.AfterCommit(ctx, func(context.Context) error {
			fmt.Println("the second callback still ran")
			return nil
		})
	})

	fmt.Println("committed:", count(statements(), "COMMIT") == 1)
	fmt.Println("is ErrAfterCommit:", errors.Is(err, sqlb.ErrAfterCommit))
	// Output:
	// the second callback still ran
	// committed: true
	// is ErrAfterCommit: true
}
