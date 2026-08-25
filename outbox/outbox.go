package outbox

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// DefaultTable is where events are recorded when [Options.Table] is empty.
const DefaultTable = "sqlb_outbox"

// DefaultChannel is the `NOTIFY` channel the table's trigger rings when
// [Options.Channel] is empty.
//
// It is a doorbell and carries no payload, so the 8000-byte limit on a
// notification is not a constraint on anything. A dispatcher that never hears it
// is late rather than wrong, which is what makes the poll a fallback rather than
// a second mechanism to keep correct.
const DefaultChannel = "sqlb_outbox"

// identifier is what a table or channel name may be. The name reaches SQL as an
// identifier rather than as a bind parameter — there is no other way to name a
// table — so it is checked here instead of trusted, and quoted at every use.
//
// Deliberately narrower than what Postgres accepts. A table called `my table` is
// legal and quoting would carry it correctly; refusing it costs a caller
// nothing and means the validation is one rule a reader can hold rather than an
// escaping argument they have to believe.
var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Options configures an [Outbox] and the DDL that backs it.
type Options struct {
	// Table is the outbox table, unqualified and lowercase. Defaults to
	// [DefaultTable].
	Table string

	// Channel is the NOTIFY channel the table's trigger rings. Defaults to
	// [DefaultChannel].
	Channel string

	// Retention is how long a delivered event is kept so that a reconnecting
	// client can still be replayed from it. Defaults to 24 hours; negative
	// disables pruning entirely.
	//
	// This is a delivery setting before it is a disk setting. A client whose
	// Last-Event-ID is older than the oldest retained row is told to refetch
	// everything it displays, so the retention window is the longest
	// disconnection a client can survive cheaply. A mobile app backgrounded
	// overnight wants a day; a dashboard on a wall wants whatever the deploy
	// interval is.
	Retention time.Duration

	// OnError reports a failure that could not be returned to a caller: a
	// best-effort record under autocommit, a poll that failed, a LISTEN that
	// dropped. Nil discards them, which is the wrong default for anything you
	// intend to rely on and is why every constructor's documentation says so.
	//
	// It may be called from several goroutines.
	OnError func(error)
}

func (o *Options) applyDefaults() error {
	if o.Table == "" {
		o.Table = DefaultTable
	}
	if o.Channel == "" {
		o.Channel = DefaultChannel
	}
	if !identifier.MatchString(o.Table) {
		return fmt.Errorf("outbox: Table %q must be a lowercase unqualified identifier", o.Table)
	}
	if !identifier.MatchString(o.Channel) {
		return fmt.Errorf("outbox: Channel %q must be a lowercase unqualified identifier", o.Channel)
	}
	if o.Retention == 0 {
		o.Retention = 24 * time.Hour
	}
	return nil
}

func (o *Options) report(err error) {
	if err != nil && o.OnError != nil {
		o.OnError(err)
	}
}

// Outbox records changes into the table, inside the transaction that made them.
//
// It implements [rest.TxPublisher], which is what makes it a drop-in for
// [rest.Broker] in a [rest.PublishChanges] call: the assertion in that function
// finds Record and uses it, so the events land in the writing transaction
// rather than in a callback after it.
//
// The zero value is not usable; call [New].
type Outbox struct {
	exec sqlb.Executor
	opts Options

	insertSQL string
	lockSQL   string
	lockKey   int64
}

// New returns an Outbox recording into the table Options names.
//
// exec is used only for the fallback path — a write that ran outside a
// transaction, where there is no transaction to record into and the change is
// already durable. Every ordinary write records on the transaction it is part
// of, taken from the context, and never touches this handle.
//
// Set [Options.OnError]. The fallback path cannot return a failure to anyone —
// the write it belongs to has already committed — so without it a change that
// was never recorded is indistinguishable from one that was.
func New(exec sqlb.Executor, opts Options) (*Outbox, error) {
	if exec == nil {
		return nil, errors.New("outbox: New needs an Executor")
	}
	if err := opts.applyDefaults(); err != nil {
		return nil, err
	}
	return &Outbox{
		exec: exec,
		opts: opts,
		// unnest rather than a VALUES list built per call, so a bulk update
		// publishing four hundred rows is one statement with four parameters
		// instead of one with sixteen hundred — and so the statement text is
		// the same every time, which is what lets pgx's prepared-statement
		// cache do anything for this path.
		insertSQL: fmt.Sprintf(
			`INSERT INTO %s (table_name, row_key, op, scope)
			 SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[])`,
			quoteIdent(opts.Table)),
		lockSQL: `SELECT pg_advisory_xact_lock($1)`,
		lockKey: lockKey(opts.Table),
	}, nil
}

// Must is New for a startup path that has nowhere to put an error.
func Must(o *Outbox, err error) *Outbox {
	if err != nil {
		panic(err)
	}
	return o
}

// Table reports the table this Outbox records into.
func (o *Outbox) Table() string { return o.opts.Table }

// Record writes the events into the transaction carried by ctx.
//
// This is [rest.TxPublisher], and the error it returns is load-bearing: it
// travels back through the hook that called it and rolls the mutation back. A
// change that could not be recorded is a change no subscriber would ever hear
// about, and a row that exists while every client believes it does not is worse
// than a write that failed and said so.
//
// It takes a transaction-scoped advisory lock first. See the package
// documentation for why the ordering that buys is worth what it costs.
func (o *Outbox) Record(ctx context.Context, events ...rest.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, inTx := sqlb.TxFrom(ctx)
	if !inTx {
		// Reachable only from a hand-written call, because rest.announce sends
		// a non-transactional write to Publish instead. Saying which of the two
		// it is beats a nil dereference on tx.
		return errors.New("outbox: Record needs the writing transaction on the context; " +
			"call it from a hook inside WithTx, or use Publish for an autocommit write")
	}
	return o.write(ctx, tx, events)
}

// Publish is the fallback for a write that ran outside a transaction, which is
// what [rest.Options.DisableTransactions] produces.
//
// There is nothing to be atomic with: the statement committed before the hook
// ran, so the event is recorded in a transaction of its own. That is at-most-once
// for this one event — the process can die in between — and it is the guarantee
// [rest.Broker] gives for every event, so nothing is worse off than it would be
// without an outbox. It is simply not what the outbox is for.
//
// The failure has nowhere to go. The write it belongs to is durable and cannot
// be undone by returning an error, and this signature has no error to return
// anyway, so it goes to [Options.OnError] — which is why that field's
// documentation says to set it.
func (o *Outbox) Publish(events ...rest.Event) {
	if len(events) == 0 {
		return
	}
	// Background rather than a caller's context: Publish is called after the
	// change is durable, and cancelling the request that caused it must not be
	// what decides whether anyone hears about it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := o.write(ctx, o.exec, events); err != nil {
		o.opts.report(fmt.Errorf("outbox: recording %d event(s) outside a transaction: %w", len(events), err))
	}
}

// write takes the lock and appends. Two statements rather than one: the lock has
// to be held before any row is inserted, and folding it into the INSERT means
// relying on how the planner orders a join against a one-row CTE. That is
// probably fine and is not worth being probably fine about — if it ever
// evaluated late, ids would stop matching commit order and events would go
// missing with nothing failing.
func (o *Outbox) write(ctx context.Context, exec sqlb.Executor, events []rest.Event) error {
	if _, err := exec.Exec(ctx, o.lockSQL, o.lockKey); err != nil {
		return fmt.Errorf("taking the outbox lock: %w", err)
	}

	tables := make([]string, len(events))
	keys := make([]string, len(events))
	ops := make([]string, len(events))
	scopes := make([]string, len(events))
	for i, e := range events {
		if e.Table == "" {
			return fmt.Errorf("outbox: event %d names no table", i)
		}
		if e.Op == "" {
			return fmt.Errorf("outbox: event %d for table %q names no operation", i, e.Table)
		}
		tables[i], keys[i], ops[i], scopes[i] = e.Table, e.Key, string(e.Op), e.Scope
	}

	if _, err := exec.Exec(ctx, o.insertSQL, tables, keys, ops, scopes); err != nil {
		return fmt.Errorf("appending to %s: %w", o.opts.Table, err)
	}
	return nil
}

// Prune removes events older than [Options.Retention] and reports how many went.
//
// [Dispatcher.Run] calls this on a timer, so an application that runs a
// dispatcher does not need to. It is exported for the one that does not — a
// worker fleet that publishes but serves no stream still fills the table.
//
// Pruning is what makes the retention window a guarantee rather than an
// intention, and it is also the one operation here that can make a connected
// client refetch: an event deleted while a client was disconnected past it turns
// that client's reconnection into a [rest.Reset].
func (o *Outbox) Prune(ctx context.Context) (int64, error) {
	return prune(ctx, o.exec, o.opts.Table, o.opts.Retention)
}

func prune(ctx context.Context, exec sqlb.Executor, table string, retention time.Duration) (int64, error) {
	if retention < 0 {
		return 0, nil
	}
	// make_interval rather than a cast of the duration's own text. Go renders
	// "24h0m0s" and "1ns", neither of which is an interval literal — the first
	// is rejected outright and the second is the kind of thing that would only
	// be discovered by a test that pruned against a real server. Seconds are
	// unambiguous in both languages.
	tag, err := exec.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE created_at < now() - make_interval(secs => $1)`, quoteIdent(table)),
		retention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("pruning %s: %w", table, err)
	}
	return tag.RowsAffected(), nil
}

// DDL renders the table, its index and the trigger that rings the doorbell.
//
// It is `IF NOT EXISTS` throughout and safe to apply repeatedly, which is what
// lets [Install] be called at startup. For a project that owns its migrations —
// which is every project that has got as far as needing this — the right home
// for this text is a migration file, so that the outbox appears in the same
// history as everything else it is transactional with.
//
// The trigger is `FOR EACH STATEMENT`, not per row: the notification carries no
// payload, so one per statement says exactly as much as one per row and a bulk
// update publishing four hundred events rings once.
//
// The doorbell is a trigger on the outbox table rather than a `pg_notify` in
// Go, which is [ADR-0012]'s decision and worth restating where someone might
// undo it. A notify issued from the mutation path is one fewer database object
// and is forgettable: a new write path that appends to the table and omits the
// notify passes every test and lags in production, because the fallback poll
// covers for it.
//
// [ADR-0012]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#change-feed-outbox
func DDL(opts Options) (string, error) {
	if err := opts.applyDefaults(); err != nil {
		return "", err
	}
	table := quoteIdent(opts.Table)
	fn := quoteIdent(opts.Table + "_notify")

	var b strings.Builder
	fmt.Fprintf(&b, `CREATE TABLE IF NOT EXISTS %s (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    table_name text        NOT NULL,
    row_key    text        NOT NULL DEFAULT '',
    op         text        NOT NULL CHECK (op IN ('create', 'update', 'delete')),
    scope      text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
`, table)

	// Pruning is the only query that does not go through the primary key, and it
	// is a range delete over a monotonic column, so it is the one thing here
	// that would otherwise seq-scan a table sized by write volume.
	fmt.Fprintf(&b, `
CREATE INDEX IF NOT EXISTS %s ON %s (created_at);
`, quoteIdent(opts.Table+"_created_at_idx"), table)

	fmt.Fprintf(&b, `
CREATE OR REPLACE FUNCTION %s() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify(%s, '');
    RETURN NULL;
END;
$$;
`, fn, quoteLiteral(opts.Channel))

	// DROP then CREATE rather than CREATE OR REPLACE, which triggers do not
	// support before Postgres 14 and which this file has no reason to require.
	fmt.Fprintf(&b, `
DROP TRIGGER IF EXISTS %s ON %s;
CREATE TRIGGER %s AFTER INSERT ON %s
FOR EACH STATEMENT EXECUTE FUNCTION %s();
`, fn, table, fn, table, fn)

	return b.String(), nil
}

// Install applies [DDL]. It is for tests, examples and a single-binary
// deployment that has no migration runner; anything with one should put the DDL
// in a migration instead.
func Install(ctx context.Context, exec sqlb.Executor, opts Options) error {
	if exec == nil {
		return errors.New("outbox: Install needs an Executor")
	}
	ddl, err := DDL(opts)
	if err != nil {
		return err
	}
	if _, err := exec.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("outbox: installing the outbox: %w", err)
	}
	return nil
}

// lockKey derives the advisory-lock key from the table name, so that two
// outboxes in one database — a test suite's, an application's — serialise
// against themselves and not against each other.
//
// Advisory locks share one 64-bit namespace with every other user of them in
// the database, so a collision with an unrelated application's lock is possible
// and would show up as unexplained contention rather than as incorrectness.
// Hashing a name is what everything else in that namespace does.
func lockKey(table string) int64 {
	h := fnv.New64a()
	// A prefix rather than the bare table name, so that an application hashing
	// its own table names the same way does not collide with this by agreeing
	// about the table.
	_, _ = h.Write([]byte("sqlb/outbox:" + table))
	return int64(h.Sum64())
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteLiteral(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }
