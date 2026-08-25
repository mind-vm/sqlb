package rest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mind-vm/sqlb"
)

// errNoRowsAffected reports a delete that matched nothing. It travels as an
// error so that the unit of work rolls back rather than committing, and is
// translated to a 404 by the handler.
var errNoRowsAffected = errors.New("rest: no rows affected")

// writer is what a generated create, update or delete runs through.
//
// Reads do not go through it. A single SELECT is already atomic, and wrapping
// one would hold a connection across a `BEGIN`/`COMMIT` round trip for no
// guarantee it did not already have.
type writer struct {
	// db is the plain executor, used when the resource opted out.
	db sqlb.Executor
	// handle is non-nil when writes are wrapped, and is what opens the
	// transaction.
	handle *sqlb.DB
	// name is the resource name, for the log message when an after-commit
	// callback fails.
	name string
}

// newWriter decides how a resource's writes will run, and refuses at startup if
// it was asked for something it cannot deliver.
//
// Refusing is the point. Falling back to autocommit when the executor cannot
// begin a transaction would put things back exactly as they were — hooks
// calling sqlb.AfterCommit would fail at request time, on the write that was
// supposed to be durable, which is the failure this whole change exists to
// remove.
func newWriter(db sqlb.Executor, opts Options) (writer, error) {
	w := writer{db: db, name: opts.name()}
	if opts.DisableTransactions {
		return w, nil
	}

	handle, ok := db.(*sqlb.DB)
	if !ok {
		handle = sqlb.New(db)
	}
	if !handle.CanBeginTx() {
		return writer{}, fmt.Errorf(
			"rest: %s wraps its writes in a transaction, but %T cannot begin one; "+
				"pass the *pgxpool.Pool (or a *sqlb.DB over it), implement "+
				"BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) on the wrapper, "+
				"or set Options.DisableTransactions to run writes under autocommit",
			opts.Path, db)
	}
	w.handle = handle
	return w, nil
}

// write runs fn as one unit of work and returns its result.
//
// Inside a transaction, fn receives a context carrying it, so a hook reaching
// for sqlb.TxFrom or sqlb.AfterCommit finds one.
func write[R any](ctx context.Context, w writer, fn func(context.Context, sqlb.Executor) (R, error)) (R, error) {
	if w.handle == nil {
		return fn(ctx, w.db)
	}

	var out R
	err := w.handle.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
		var err error
		out, err = fn(ctx, tx)
		return err
	})
	if err != nil {
		var zero R
		// The row is written and the response is correct; only a side effect
		// failed. Reporting that as a failed request would invite a retry of a
		// write that already happened, which is the whole reason the sentinel
		// exists — see sqlb.ErrAfterCommit.
		if errors.Is(err, sqlb.ErrAfterCommit) {
			slog.ErrorContext(ctx, "rest: after-commit callback failed; the write is durable",
				"resource", w.name, "err", err)
			return out, nil
		}
		return zero, err
	}
	return out, nil
}
