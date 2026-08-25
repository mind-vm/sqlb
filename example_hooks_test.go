package sqlb_test

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/internal/pgfake"
)

// The examples below run real terminal methods, because a hook only fires when
// a statement executes — asserting on a builder would prove nothing. This is the
// smallest thing that can stand behind Executor: something that records every
// statement and replays one canned row. No Postgres, and no pretending.
var exampleLog []string

// exampleDB returns a handle over the recording executor and clears the log.
func exampleDB(reg *sqlb.Registry) *sqlb.DB {
	exampleLog = nil
	return sqlb.New(exampleExec{}).WithHooks(reg)
}

// whereClause returns just the WHERE clause of the last statement, so an example
// can show what a hook contributed without printing the whole projection.
func whereClause() string {
	if len(exampleLog) == 0 {
		return "(no statement ran)"
	}
	last := exampleLog[len(exampleLog)-1]
	_, where, ok := strings.Cut(last, " WHERE ")
	if !ok {
		return "(no WHERE clause)"
	}
	return where
}

// BeforeQuery is the load-bearing hook: it receives the query itself, so one
// registration constrains every read of the model — including the reads the
// generated REST handlers issue. Multi-tenancy and soft deletes stop being
// something each call site has to remember.
func ExampleHooks_BeforeQuery() {
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[Article](reg)

	hooks.BeforeQuery(func(_ context.Context, q *sqlb.Builder[Article]) error {
		// In a real application the tenant comes from the request context.
		q.Where(sqlb.F("org_id").Eq("acme"))
		return nil
	})

	db := exampleDB(reg)
	ctx := context.Background()

	// The caller filters on status and knows nothing about tenants.
	if _, err := sqlb.Query[Article]().Where(sqlb.F("status").Eq("published")).All(ctx, db); err != nil {
		panic(err)
	}
	fmt.Println("list: ", whereClause())

	// A different read, through a different entry point, is scoped too.
	if _, err := sqlb.Query[Article]().Count(ctx, db); err != nil {
		panic(err)
	}
	fmt.Println("count:", whereClause())

	// Output:
	// list:  ("status" = $1) AND ("org_id" = $2)
	// count: "org_id" = $1
}

// A hook returning an error aborts the operation, and the error reaches the
// caller unwrapped. This is how "no tenant in this context" becomes impossible
// to forget rather than merely documented.
func ExampleHooks_BeforeQuery_reject() {
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[Article](reg)

	errNoTenant := errors.New("no tenant in context")
	hooks.BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Article]) error {
		org, ok := ctx.Value(orgKey{}).(string)
		if !ok {
			return errNoTenant
		}
		q.Where(sqlb.F("org_id").Eq(org))
		return nil
	})

	db := exampleDB(reg)

	_, err := sqlb.Query[Article]().All(context.Background(), db)
	fmt.Println("unscoped:", err)
	fmt.Println("statements run:", len(exampleLog))

	ctx := context.WithValue(context.Background(), orgKey{}, "acme")
	if _, err := sqlb.Query[Article]().All(ctx, db); err != nil {
		panic(err)
	}
	fmt.Println("scoped:  ", whereClause())

	// Output:
	// unscoped: no tenant in context
	// statements run: 0
	// scoped:   "org_id" = $1
}

type orgKey struct{}

// AfterCommit runs work that must not happen if the write does not. AfterCreate
// and its siblings run inside the transaction, which is right for validation and
// wrong for anything the outside world can observe: the transaction may still
// abort after the hook has announced a write that then never happened.
func ExampleAfterCommit() {
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[Article](reg)

	hooks.AfterCreate(func(ctx context.Context, a *Article) error {
		// Runs inside the transaction. Returning an error here rolls the insert
		// back, so the event is registered rather than published.
		id := a.ID
		return sqlb.AfterCommit(ctx, func(context.Context) error {
			fmt.Println("published event for", id)
			return nil
		})
	})

	db := exampleDB(reg)
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		a := Article{Title: "Hello", Status: "draft", OrgID: "acme"}
		_, err := sqlb.InsertRows(&a).One(ctx, tx)
		fmt.Println("insert returned, still inside the transaction")
		return err
	})
	if err != nil {
		panic(err)
	}

	// Output:
	// insert returned, still inside the transaction
	// published event for a1
}

// A rollback discards the callbacks by never reaching them, which is the whole
// point: no event is published for a write that did not land.
func ExampleAfterCommit_rollback() {
	reg := sqlb.NewRegistry()
	hooks := sqlb.On[Article](reg)

	hooks.AfterCreate(func(ctx context.Context, a *Article) error {
		return sqlb.AfterCommit(ctx, func(context.Context) error {
			fmt.Println("this must not print")
			return nil
		})
	})

	db := exampleDB(reg)
	errPaymentDeclined := errors.New("payment declined")
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		a := Article{Title: "Hello", Status: "draft", OrgID: "acme"}
		if _, err := sqlb.InsertRows(&a).One(ctx, tx); err != nil {
			return err
		}
		return errPaymentDeclined // something later in the unit of work fails
	})

	fmt.Println("WithTx:", err)
	fmt.Println("last statement:", exampleLog[len(exampleLog)-1])

	// Output:
	// WithTx: payment declined
	// last statement: ROLLBACK
}

// --- the recording executor -------------------------------------------------

type exampleExec struct{}

func (e exampleExec) Query(_ context.Context, query string, _ ...any) (pgx.Rows, error) {
	exampleLog = append(exampleLog, query)
	// The result has to match the projection, or the scan fails before the
	// example gets to print anything. A count is one column; everything else
	// here selects the whole row.
	if strings.HasPrefix(query, "SELECT count(") {
		return &pgfake.Rows{Cols: []string{"count"}, Data: [][]any{{int64(1)}}}, nil
	}
	return &pgfake.Rows{
		Cols: []string{"id", "title", "status", "view_count", "org_id"},
		Data: [][]any{{"a1", "Hello", "draft", int64(0), "acme"}},
	}, nil
}

func (e exampleExec) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	exampleLog = append(exampleLog, query)
	return pgconn.NewCommandTag("DELETE 1"), nil
}

func (e exampleExec) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	exampleLog = append(exampleLog, "BEGIN")
	return &pgfake.Tx{
		Statements: e,
		OnCommit:   func() error { exampleLog = append(exampleLog, "COMMIT"); return nil },
		OnRollback: func() error { exampleLog = append(exampleLog, "ROLLBACK"); return nil },
	}, nil
}

// A background worker acts as a synthetic principal — "system", with a tenant
// but no user row — so its reads and writes run the same rules a request's do.
// One rule cannot answer for it: the one that asks who the caller is. Naming
// that rule at registration is what lets a worker handle release exactly it
// while every other rule stays live, instead of reaching for a second,
// unhooked handle and dropping the tenant boundary along with it.
func ExampleDB_WithoutScope() {
	type claims struct{ Subject, Org string }

	reg := sqlb.NewRegistry()
	hooks := sqlb.On[Article](reg)

	// Tenant-shaped, and deliberately unnamed: nothing may release it.
	hooks.BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Article]) error {
		c, ok := sqlb.PrincipalFrom[claims](ctx)
		if !ok {
			return errors.New("no principal on this context")
		}
		q.Where(sqlb.F("org_id").Eq(c.Org))
		return nil
	})

	// Identity-shaped, and named: "system" is not a user id, so this question
	// has no answer for a worker.
	hooks.Scope("membership").BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Article]) error {
		c, _ := sqlb.PrincipalFrom[claims](ctx)
		q.Where(sqlb.F("author_id").Eq(c.Subject))
		return nil
	})

	db := exampleDB(reg)
	ctx := sqlb.WithPrincipal(context.Background(), claims{Subject: "system", Org: "acme"})

	if _, err := sqlb.Query[Article]().All(ctx, db); err != nil {
		panic(err)
	}
	fmt.Println("request:", whereClause())

	// The worker's handle is built next to the registry, so which rule anyone
	// is allowed to escape is readable from the wiring.
	worker := db.WithoutScope("membership")
	if _, err := sqlb.Query[Article]().All(ctx, worker); err != nil {
		panic(err)
	}
	fmt.Println("worker: ", whereClause())

	// Output:
	// request: ("org_id" = $1) AND ("author_id" = $2)
	// worker:  "org_id" = $1
}
