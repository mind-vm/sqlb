package outbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/internal/pgfake"
	"github.com/mind-vm/sqlb/outbox"
	"github.com/mind-vm/sqlb/rest"
)

// The assertion the whole swap rests on. If Outbox stops satisfying
// TxPublisher, rest.PublishChanges silently falls back to announcing after the
// commit — a working-looking feed with none of the durability this package
// exists for — so it is checked at compile time rather than waited for.
var (
	_ rest.TxPublisher = (*outbox.Outbox)(nil)
	_ rest.Publisher   = (*outbox.Outbox)(nil)
	_ rest.Source      = (*outbox.Dispatcher)(nil)
)

// recExec is an Executor that remembers, so the statements this package builds
// can be asserted without a database.
type recExec struct {
	stmts []string
	args  [][]any
	fail  error
}

func (e *recExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.stmts = append(e.stmts, sql)
	e.args = append(e.args, args)
	if e.fail != nil {
		return pgconn.CommandTag{}, e.fail
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (e *recExec) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &pgfake.Rows{}, nil
}

// BeginTx makes the fake a sqlb.Beginner, so these tests can use the real
// WithTx rather than assembling a transaction context by hand.
func (e *recExec) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return &pgfake.Tx{Statements: e}, nil
}

func TestOptionsRefuseANameThatIsNotAnIdentifier(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts outbox.Options
		want string
	}{
		{"a quoted table", outbox.Options{Table: `sqlb"; DROP TABLE users; --`}, "Table"},
		{"a qualified table", outbox.Options{Table: "public.sqlb_outbox"}, "Table"},
		{"an uppercase table", outbox.Options{Table: "Outbox"}, "Table"},
		{"a channel with a space", outbox.Options{Channel: "sqlb outbox"}, "Channel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := outbox.DDL(tc.opts); err == nil {
				t.Fatalf("DDL(%+v) was accepted; the name reaches SQL as an identifier", tc.opts)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

// The other direction (ADR-0016): the ordinary names are accepted, so the check
// above is a rule about identifiers and not a wall.
func TestOptionsAcceptOrdinaryNames(t *testing.T) {
	if _, err := outbox.DDL(outbox.Options{}); err != nil {
		t.Fatalf("the defaults were refused: %v", err)
	}
	if _, err := outbox.DDL(outbox.Options{Table: "events_outbox", Channel: "events"}); err != nil {
		t.Fatalf("a custom name was refused: %v", err)
	}
}

func TestDDLCarriesTheTableIndexAndDoorbell(t *testing.T) {
	ddl, err := outbox.DDL(outbox.Options{})
	if err != nil {
		t.Fatalf("DDL: %v", err)
	}
	for _, want := range []string{
		`CREATE TABLE IF NOT EXISTS "sqlb_outbox"`,
		`GENERATED ALWAYS AS IDENTITY PRIMARY KEY`,
		`CHECK (op IN ('create', 'update', 'delete'))`,
		`CREATE INDEX IF NOT EXISTS "sqlb_outbox_created_at_idx"`,
		`pg_notify('sqlb_outbox', '')`,
		`FOR EACH STATEMENT`,
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("the DDL is missing %q:\n%s", want, ddl)
		}
	}
	// Per row would ring once per event where the notification carries no
	// payload and one ring says as much as four hundred.
	if strings.Contains(ddl, "FOR EACH ROW") {
		t.Errorf("the doorbell is per row:\n%s", ddl)
	}
}

func TestDDLFollowsTheConfiguredNames(t *testing.T) {
	ddl, err := outbox.DDL(outbox.Options{Table: "changes", Channel: "changed"})
	if err != nil {
		t.Fatalf("DDL: %v", err)
	}
	for _, want := range []string{`"changes"`, `"changes_notify"`, `pg_notify('changed', '')`} {
		if !strings.Contains(ddl, want) {
			t.Errorf("the DDL is missing %q:\n%s", want, ddl)
		}
	}
	if strings.Contains(ddl, "sqlb_outbox") {
		t.Errorf("the default name leaked into a configured outbox:\n%s", ddl)
	}
}

func newOutbox(t *testing.T, exec sqlb.Executor, opts outbox.Options) *outbox.Outbox {
	t.Helper()
	ob, err := outbox.New(exec, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ob
}

// The lock is what makes id order commit order, so it has to be taken before
// the append and not alongside it.
func TestRecordLocksBeforeItAppends(t *testing.T) {
	exec := &recExec{}
	ob := newOutbox(t, exec, outbox.Options{})

	err := inTx(t, exec, func(ctx context.Context) error {
		return ob.Record(ctx, rest.Event{Table: "posts", Key: "p1", Op: rest.Created})
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	if len(exec.stmts) != 2 {
		t.Fatalf("statements = %v, want the lock and the append", exec.stmts)
	}
	if !strings.Contains(exec.stmts[0], "pg_advisory_xact_lock") {
		t.Errorf("the first statement is %q, want the advisory lock", exec.stmts[0])
	}
	// The transaction form, not the session form: ADR-0019 forbids anything
	// session-scoped on a path that may run through a pooler in transaction
	// mode, where a session lock would never be released.
	if strings.Contains(exec.stmts[0], "pg_advisory_lock(") {
		t.Errorf("the lock is session-scoped: %q", exec.stmts[0])
	}
	if !strings.Contains(exec.stmts[1], `INSERT INTO "sqlb_outbox"`) {
		t.Errorf("the second statement is %q, want the append", exec.stmts[1])
	}
}

// One statement whatever the batch size, so a bulk update publishing four
// hundred rows is four arrays rather than sixteen hundred parameters.
func TestRecordAppendsABatchInOneStatement(t *testing.T) {
	exec := &recExec{}
	ob := newOutbox(t, exec, outbox.Options{})

	events := make([]rest.Event, 400)
	for i := range events {
		events[i] = rest.Event{Table: "posts", Key: "k", Op: rest.Updated, Scope: "acme"}
	}
	err := inTx(t, exec, func(ctx context.Context) error { return ob.Record(ctx, events...) })
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	if len(exec.stmts) != 2 {
		t.Fatalf("statements = %d, want 2 regardless of the batch", len(exec.stmts))
	}
	args := exec.args[1]
	if len(args) != 4 {
		t.Fatalf("the append took %d parameters, want 4 arrays", len(args))
	}
	tables, ok := args[0].([]string)
	if !ok || len(tables) != 400 {
		t.Errorf("the first parameter is %T of the wrong length, want 400 table names", args[0])
	}
}

// Outside a transaction there is nothing to record into, and saying so beats
// dereferencing a nil handle.
func TestRecordOutsideATransactionSaysWhichCallToUse(t *testing.T) {
	ob := newOutbox(t, &recExec{}, outbox.Options{})
	err := ob.Record(context.Background(), rest.Event{Table: "posts", Op: rest.Created})
	if err == nil {
		t.Fatal("Record outside a transaction was accepted")
	}
	for _, want := range []string{"WithTx", "Publish"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not offer %q", err, want)
		}
	}
}

// The failure has nowhere to go — the write that caused it is already durable —
// so OnError is the whole of the reporting, which is why its documentation says
// to set it.
func TestPublishReportsAFailureItCannotReturn(t *testing.T) {
	exec := &recExec{fail: errors.New("connection refused")}
	var got []error
	ob := newOutbox(t, exec, outbox.Options{OnError: func(err error) { got = append(got, err) }})

	ob.Publish(rest.Event{Table: "posts", Key: "p1", Op: rest.Created})

	if len(got) != 1 {
		t.Fatalf("OnError saw %d failures, want 1", len(got))
	}
	if !strings.Contains(got[0].Error(), "connection refused") {
		t.Errorf("the report %q does not carry the cause", got[0])
	}
}

func TestRecordRejectsAnEventNamingNothing(t *testing.T) {
	exec := &recExec{}
	ob := newOutbox(t, exec, outbox.Options{})

	if err := inTx(t, exec, func(ctx context.Context) error {
		return ob.Record(ctx, rest.Event{Op: rest.Created})
	}); err == nil {
		t.Error("an event with no table was recorded")
	}
	if err := inTx(t, exec, func(ctx context.Context) error {
		return ob.Record(ctx, rest.Event{Table: "posts"})
	}); err == nil {
		t.Error("an event with no operation was recorded")
	}
}

func TestRecordingNothingIsNotAStatement(t *testing.T) {
	exec := &recExec{}
	ob := newOutbox(t, exec, outbox.Options{})
	if err := inTx(t, exec, func(ctx context.Context) error { return ob.Record(ctx) }); err != nil {
		t.Fatalf("Record with no events: %v", err)
	}
	ob.Publish()
	if len(exec.stmts) != 0 {
		t.Errorf("statements = %v, want none: an empty batch is not a lock to take", exec.stmts)
	}
}

func TestRetentionDefaultsToADay(t *testing.T) {
	exec := &recExec{}
	ob := newOutbox(t, exec, outbox.Options{})
	if _, err := ob.Prune(context.Background()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(exec.args) != 1 || len(exec.args[0]) != 1 {
		t.Fatalf("Prune issued %v", exec.stmts)
	}
	// Seconds, not the Duration's own text: "24h0m0s" is not an interval literal
	// and Postgres rejects it. pgtest/outbox_test.go is what found that.
	if got := exec.args[0][0]; got != (24 * time.Hour).Seconds() {
		t.Errorf("retention = %v, want %v seconds", got, (24 * time.Hour).Seconds())
	}
	if !strings.Contains(exec.stmts[0], "make_interval") {
		t.Errorf("prune statement is %q, want make_interval", exec.stmts[0])
	}
}

func TestNegativeRetentionPrunesNothing(t *testing.T) {
	exec := &recExec{}
	ob := newOutbox(t, exec, outbox.Options{Retention: -1})
	if n, err := ob.Prune(context.Background()); err != nil || n != 0 {
		t.Fatalf("Prune = %d, %v; want 0, nil", n, err)
	}
	if len(exec.stmts) != 0 {
		t.Errorf("statements = %v, want none", exec.stmts)
	}
}

// inTx runs fn inside a real sqlb transaction over the fake, so Record finds
// the transaction the way it would in a hook — through sqlb.TxFrom, put there by
// WithTx and not by a test reaching into an unexported context key.
func inTx(t *testing.T, exec *recExec, fn func(ctx context.Context) error) error {
	t.Helper()
	return sqlb.New(exec).WithTx(context.Background(), func(ctx context.Context, _ *sqlb.DB) error {
		return fn(ctx)
	})
}
