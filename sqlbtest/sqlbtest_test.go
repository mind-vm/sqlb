package sqlbtest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/sqlbtest"
)

// Note is the model these tests read and write. It is deliberately ordinary:
// what is under test is the double, not the engine.
type Note struct {
	ID      string `db:"id" sqlb:"pk,default"`
	SpaceID string `db:"space_id" sqlb:"filter"`
	Title   string `db:"title" sqlb:"filter,sort"`
	Secret  string `db:"internal" sqlb:"hidden"`
}

func (Note) TableName() string { return "notes" }

// The package's reason for existing, written as the test a consumer would write
// on day three: a hook that scopes every read to the caller's space, asserted
// without a database (issue #77).
func TestAScopingHookIsTestableWithoutADatabase(t *testing.T) {
	hooks := sqlb.NewRegistry()
	sqlb.On[Note](hooks).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Note]) error {
		q.Where(sqlb.F("space_id").Eq(ctx.Value(spaceKey{})))
		return nil
	})

	db := sqlbtest.New(sqlbtest.Reply{
		Cols: []string{"id", "space_id", "title"},
		Rows: [][]any{{"n1", "acme", "Hello"}},
	})
	handle := sqlb.New(db).WithHooks(hooks)

	ctx := context.WithValue(context.Background(), spaceKey{}, "acme")
	notes, err := sqlb.Query[Note]().All(ctx, handle)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(notes) != 1 || notes[0].Title != "Hello" {
		t.Fatalf("scanned %+v", notes)
	}

	// The statement says a predicate was added...
	if !strings.Contains(db.LastStatement(), `"space_id" = $1`) {
		t.Errorf("the hook's predicate did not reach the statement:\n%s", db.LastStatement())
	}
	// ...and the args say what it was given, which is the half that catches a
	// hook reading the space from the wrong place.
	if args := db.LastArgs(); len(args) != 1 || args[0] != "acme" {
		t.Errorf("bound %#v, want the caller's space", args)
	}
}

type spaceKey struct{}

// A write is wrapped in a transaction, and the markers are in the log — which is
// what lets a consumer assert that its unit of work committed, or that a failure
// rolled back.
func TestTransactionBoundariesAreRecorded(t *testing.T) {
	db := sqlbtest.New(sqlbtest.Reply{
		Cols: []string{"id", "space_id", "title", "internal"},
		Rows: [][]any{{"n1", "acme", "Hello", ""}},
	})
	handle := sqlb.New(db)

	err := handle.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		_, err := sqlb.InsertRows(&Note{SpaceID: "acme", Title: "Hello"}).One(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	got := strings.Join(db.Statements(), " | ")
	if !strings.HasPrefix(got, "BEGIN") || !strings.HasSuffix(got, "COMMIT") {
		t.Errorf("the write was not wrapped:\n%s", got)
	}
	// LastStatement skips the markers, so an assertion about SQL is not about
	// COMMIT.
	if !strings.HasPrefix(db.LastStatement(), `INSERT INTO "notes"`) {
		t.Errorf("LastStatement = %q, want the insert", db.LastStatement())
	}
}

func TestAFailingUnitOfWorkRollsBack(t *testing.T) {
	db := sqlbtest.New(sqlbtest.Reply{Match: "INSERT", Err: errors.New("nope")})
	handle := sqlb.New(db)

	err := handle.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		_, err := sqlb.InsertRows(&Note{Title: "Hello"}).One(ctx, tx)
		return err
	})
	if err == nil {
		t.Fatal("a failing statement reported success")
	}
	got := strings.Join(db.Statements(), " | ")
	if !strings.HasSuffix(got, "ROLLBACK") {
		t.Errorf("the failed write did not roll back:\n%s", got)
	}
}

// Replies are matched in order by substring, which is what lets a test tell the
// page query from the count query. A statement nothing matches answers empty
// rather than failing, so a test only has to script what it asserts on.
func TestRepliesMatchBySubstringInOrder(t *testing.T) {
	db := sqlbtest.New(
		sqlbtest.Reply{Match: "count(", Cols: []string{"count"}, Rows: [][]any{{int64(42)}}},
		sqlbtest.Reply{Cols: []string{"id", "space_id", "title"}, Rows: [][]any{{"n1", "acme", "Hi"}}},
	)
	handle := sqlb.New(db)
	ctx := context.Background()

	n, err := sqlb.Query[Note]().Count(ctx, handle)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 42 {
		t.Errorf("Count = %d, want 42 — the specific reply should win", n)
	}

	notes, err := sqlb.Query[Note]().All(ctx, handle)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(notes) != 1 {
		t.Errorf("the catch-all reply answered %d rows, want 1", len(notes))
	}

	// And an unscripted statement fails, naming itself. Answering it with an
	// empty result set would hand back zero columns, and the scan would fail
	// several frames later with a message about the model's db tags rather
	// than about the missing reply.
	_, err = sqlb.Query[Note]().All(ctx, sqlb.New(sqlbtest.New()))
	if err == nil {
		t.Fatal("an unscripted statement was answered")
	}
	for _, want := range []string{"no Reply matches", "SELECT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should say what is missing and quote the statement, got: %v", err)
		}
	}
}

// RowsErr is the failure pgx reports while the result is being read, which is
// what a constraint violation looks like on the extended protocol. Code that
// only checked what Query returned would miss it, so the double has to be able
// to produce one.
func TestRowsErrFailsDuringIteration(t *testing.T) {
	boom := errors.New("duplicate key value violates unique constraint")
	db := sqlbtest.New(sqlbtest.Reply{
		Cols:    []string{"id", "space_id", "title"},
		Rows:    [][]any{{"n1", "acme", "Hi"}},
		RowsErr: boom,
	})

	_, err := sqlb.Query[Note]().All(context.Background(), sqlb.New(db))
	if !errors.Is(err, boom) {
		t.Errorf("All() = %v, want the iteration error", err)
	}
}

func TestResetClearsTheLogAndKeepsTheScript(t *testing.T) {
	db := sqlbtest.New(sqlbtest.Reply{Cols: []string{"id", "space_id", "title"}})
	handle := sqlb.New(db)
	ctx := context.Background()

	if _, err := sqlb.Query[Note]().All(ctx, handle); err != nil {
		t.Fatal(err)
	}
	db.Reset()
	if got := db.Statements(); len(got) != 0 {
		t.Errorf("Reset left %v behind", got)
	}
	if _, err := sqlb.Query[Note]().All(ctx, handle); err != nil {
		t.Fatalf("the script was cleared along with the log: %v", err)
	}
	if len(db.Statements()) != 1 {
		t.Errorf("after Reset the log holds %d statements, want 1", len(db.Statements()))
	}
}
