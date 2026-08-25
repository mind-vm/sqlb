package sqlbtest

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb/internal/pgfake"
)

// Reply is one scripted answer.
//
// Match is a substring of the statement it answers, so a test can tell the page
// query from the count query without parsing SQL. An empty Match answers
// anything, which is what a script with a single reply wants. Replies are tried
// in order and the first match wins, so put the specific ones first.
//
// A statement no reply matches fails, with an error naming the statement. That
// is deliberate and it is the one place this package is stricter than it has to
// be: a double that answered an unscripted read with an empty result set would
// hand back zero columns, and the scan would fail several frames later with a
// message about the model's db tags rather than about the missing reply. Add a
// Reply with an empty Match for a catch-all.
type Reply struct {
	Match string
	Cols  []string
	Rows  [][]any

	// Err fails the statement when it is sent, which is what a syntax error or
	// a connection failure looks like.
	Err error

	// RowsErr fails the statement while its result is being read, which is what
	// a constraint violation looks like on pgx's extended protocol. The
	// distinction is not academic: code that only checks what Query returned
	// misses a constraint violation entirely, and a wrapped
	// *pgconn.PgError here is how a test reaches sqlb's constraint
	// classification.
	RowsErr error

	// Tag overrides the command tag an Exec reports. The default is derived
	// from len(Rows), which is what the row-count paths read.
	Tag string
}

// DB is a scripted, database-free [sqlb.Executor]. It also satisfies the
// transaction-capable interface sqlb.New looks for, so generated writes — which
// wrap themselves in a transaction by default — run against it unchanged, and
// BEGIN, COMMIT and ROLLBACK land in the statement log where a test can assert
// on them.
//
// The zero DB is usable and refuses every statement, since it has no script.
type DB struct {
	mu      sync.Mutex
	replies []Reply
	log     []string
	args    [][]any
}

// New returns a DB answering from the given script.
func New(replies ...Reply) *DB { return &DB{replies: replies} }

// Script replaces the reply set, for a test that changes what the database says
// partway through. It does not clear the statement log.
func (d *DB) Script(replies ...Reply) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.replies = replies
}

// Statements returns every statement issued, in order, including the
// transaction markers.
func (d *DB) Statements() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.log...)
}

// LastStatement is the most recent statement, for asserting on compiled SQL.
//
// Transaction markers are skipped. A generated write is wrapped by default, so
// the raw last entry is COMMIT and no assertion about SQL has ever been about
// that; a test asking whether a write was wrapped reads [DB.Statements].
func (d *DB) LastStatement() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i := d.lastRealLocked(); i >= 0 {
		return d.log[i]
	}
	return ""
}

// LastArgs is the bind parameters of the most recent statement, skipping the
// transaction markers for the same reason [DB.LastStatement] does.
//
// This is where a scoping test belongs. The statement text says a predicate was
// added; the args say what value it was given, which is the half that catches a
// hook reading the tenant from the request body.
func (d *DB) LastArgs() []any {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i := d.lastRealLocked(); i >= 0 {
		return append([]any(nil), d.args[i]...)
	}
	return nil
}

// Args is the bind parameters of every statement, aligned with
// [DB.Statements]. A transaction marker has none.
func (d *DB) Args() [][]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]any, len(d.args))
	for i, a := range d.args {
		out[i] = append([]any(nil), a...)
	}
	return out
}

// Reset clears the statement log, leaving the script in place.
func (d *DB) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log, d.args = nil, nil
}

// Query answers a read.
func (d *DB) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	r, ok := d.answer(query, args)
	if !ok {
		return nil, unscripted(query)
	}
	if r.Err != nil {
		return nil, r.Err
	}
	return &pgfake.Rows{Cols: r.Cols, Data: r.Rows, Fail: r.RowsErr}, nil
}

// QueryRow answers a single-row read. It is here because pgx.Tx requires it;
// sqlb itself reads through Query.
func (d *DB) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	rows, err := d.Query(ctx, query, args...)
	if err != nil {
		return errRow{err}
	}
	row, ok := rows.(*pgfake.Rows)
	if !ok {
		return errRow{fmt.Errorf("sqlbtest: unexpected rows type %T", rows)}
	}
	return row
}

// Exec answers a write, reporting a command tag whose row count is the number of
// rows the matching reply carries.
func (d *DB) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	r, ok := d.answer(query, args)
	if !ok {
		return pgconn.CommandTag{}, unscripted(query)
	}
	if r.Err != nil {
		return pgconn.CommandTag{}, r.Err
	}
	if r.Tag != "" {
		return pgconn.NewCommandTag(r.Tag), nil
	}
	return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", len(r.Rows))), nil
}

// BeginTx opens a scripted transaction. Its statements go through this same DB,
// and its boundary is recorded in the statement log — which is what lets a test
// assert that a unit of work was wrapped, and that a failing one rolled back
// rather than committing.
func (d *DB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	d.record("BEGIN")
	return &pgfake.Tx{
		Statements: d,
		OnCommit:   func() error { d.record("COMMIT"); return nil },
		OnRollback: func() error { d.record("ROLLBACK"); return nil },
	}, nil
}

// answer picks the reply for a statement, recording the statement either way.
func (d *DB) answer(query string, args []any) (Reply, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, query)
	d.args = append(d.args, args)
	for _, r := range d.replies {
		if r.Match == "" || strings.Contains(query, r.Match) {
			return r, true
		}
	}
	return Reply{}, false
}

// record logs a statement that carries no bind parameters, keeping args aligned
// with the log so LastArgs stays meaningful.
func (d *DB) record(stmt string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, stmt)
	d.args = append(d.args, nil)
}

// lastRealLocked is the index of the most recent statement that is not a
// transaction marker, or -1. Callers hold d.mu.
func (d *DB) lastRealLocked() int {
	for i := len(d.log) - 1; i >= 0; i-- {
		switch d.log[i] {
		case "BEGIN", "COMMIT", "ROLLBACK":
		default:
			return i
		}
	}
	return -1
}

// unscripted is the error a statement no reply matches produces. It quotes the
// statement, because the fix is always to add a Reply whose Match distinguishes
// it — and a truncated one, because a generated statement can be long and the
// distinguishing part is at the front.
func unscripted(query string) error {
	return fmt.Errorf("sqlbtest: no Reply matches this statement, so the test has not said "+
		"what the database should answer; add one with a Match that selects it, or a "+
		"Reply with an empty Match as a catch-all:\n  %s", truncate(query, 300))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// errRow is a pgx.Row that only reports its error, for the QueryRow path.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }
