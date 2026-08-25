package sqlbtest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jryannel/sqlb"
	"github.com/jryannel/sqlb/sqlbtest"
)

// Task is the model with the two decisions in it: a nullable column, which is a
// pointer field, and a computed one, which the default projection does not
// select.
type Task struct {
	ID         string  `db:"id" sqlb:"pk,default"`
	Title      string  `db:"title"`
	AssigneeID *string `db:"assignee_id"`
	IsOverdue  bool    `db:"is_overdue"`
}

func (Task) TableName() string { return "tasks" }

func (Task) ComputedColumns() []sqlb.Computed {
	return []sqlb.Computed{{Name: "is_overdue", Expr: "due_date < current_date"}}
}

// The reason the helper exists. Note's four columns are all strings, so a Cols
// and Rows pair written by hand that disagrees on order scans without
// complaining and hands back a Note with the space id in its title — which is
// the failure this removes, since both halves now come from the same model.
func TestRowsAnswersAModelWithoutSpellingItsColumns(t *testing.T) {
	want := Note{ID: "n1", SpaceID: "acme", Title: "Hello", Secret: "shh"}
	db := sqlbtest.New(sqlbtest.Rows(want))

	notes, err := sqlb.Query[Note]().All(context.Background(), sqlb.New(db))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("scanned %d rows, want 1", len(notes))
	}
	if notes[0] != want {
		t.Errorf("scanned %+v, want %+v", notes[0], want)
	}
}

// Hidden is a REST response rule rather than a projection one, so the double
// answers the column for the same reason the engine selects it: a test that
// scripted it away would be asserting against a statement sqlb does not compile.
func TestRowsAnswersAHiddenColumnBecauseTheProjectionSelectsIt(t *testing.T) {
	db := sqlbtest.New(sqlbtest.Rows(Note{ID: "n1", Secret: "shh"}))

	notes, err := sqlb.Query[Note]().All(context.Background(), sqlb.New(db))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if notes[0].Secret != "shh" {
		t.Errorf("Secret = %q, want the hidden column to have been answered", notes[0].Secret)
	}
	if !strings.Contains(db.LastStatement(), `"internal"`) {
		t.Errorf("the projection should carry the hidden column:\n%s", db.LastStatement())
	}
}

// A computed column is a per-query decision the double cannot see, so Rows
// leaves it where the default projection leaves it — out — and a test reading
// one scripts a Reply.
func TestRowsLeavesComputedColumnsOut(t *testing.T) {
	reply := sqlbtest.Rows(Task{ID: "t1", Title: "Ship it", IsOverdue: true})

	for _, name := range reply.Cols {
		if name == "is_overdue" {
			t.Fatalf("Cols = %v, want the computed column left out", reply.Cols)
		}
	}

	tasks, err := sqlb.Query[Task]().All(context.Background(), sqlb.New(sqlbtest.New(reply)))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if tasks[0].IsOverdue {
		t.Error("IsOverdue was filled by a column the statement never selected")
	}
	if tasks[0].Title != "Ship it" {
		t.Errorf("Title = %q, want the stored columns to still scan", tasks[0].Title)
	}
}

// A pointer field is a nullable column, and the two states have to survive the
// round trip separately: nil is NULL, and a set pointer is a value.
func TestRowsSendsAnAbsentPointerAsNull(t *testing.T) {
	assignee := "u1"
	db := sqlbtest.New(sqlbtest.Rows(
		Task{ID: "t1", AssigneeID: &assignee},
		Task{ID: "t2"},
	))

	tasks, err := sqlb.Query[Task]().All(context.Background(), sqlb.New(db))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if tasks[0].AssigneeID == nil || *tasks[0].AssigneeID != "u1" {
		t.Errorf("AssigneeID = %v, want the pointer's value", tasks[0].AssigneeID)
	}
	if tasks[1].AssigneeID != nil {
		t.Errorf("AssigneeID = %q, want NULL", *tasks[1].AssigneeID)
	}
}

// A Reply is a value a test may read back, so a NULL in one built here is
// spelled the way a hand-written script spells it: nil, not a typed nil pointer
// that only prints the same.
func TestRowsSpellsANullTheWayAScriptWould(t *testing.T) {
	reply := sqlbtest.Rows(Task{ID: "t1"})

	var at int
	for i, name := range reply.Cols {
		if name == "assignee_id" {
			at = i
		}
	}
	if got := reply.Rows[0][at]; got != nil {
		t.Errorf("Rows[0][%d] = %#v, want an untyped nil", at, got)
	}
}

// Rows with no values is an empty result set rather than an unscripted
// statement, which is what a test asserting on "found nothing" wants.
func TestRowsWithNoValuesIsAnEmptyResultSet(t *testing.T) {
	db := sqlbtest.New(sqlbtest.Rows[Note]())

	notes, err := sqlb.Query[Note]().All(context.Background(), sqlb.New(db))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("scanned %d rows, want none", len(notes))
	}
}

// The paging pair, which is the script most tests write: the count statement
// and the page statement, told apart by Match.
func TestCountAnswersTheCountStatementAndRowsThePage(t *testing.T) {
	db := sqlbtest.New(
		sqlbtest.Count(42),
		sqlbtest.Rows(Note{ID: "n1"}, Note{ID: "n2"}),
	)
	handle := sqlb.New(db)
	ctx := context.Background()

	n, err := sqlb.Query[Note]().Count(ctx, handle)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 42 {
		t.Errorf("Count = %d, want 42", n)
	}

	notes, err := sqlb.Query[Note]().All(ctx, handle)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(notes) != 2 {
		t.Errorf("scanned %d rows, want the page", len(notes))
	}
}

// Matching narrows a constructed reply in place, so a script holding two of
// them does not need a variable to reach the field.
func TestMatchingNarrowsAConstructedReply(t *testing.T) {
	db := sqlbtest.New(
		sqlbtest.Rows(Note{ID: "scoped"}).Matching(`"space_id" = $1`),
		sqlbtest.Rows(Note{ID: "everything"}),
	)
	handle := sqlb.New(db)
	ctx := context.Background()

	scoped, err := sqlb.Query[Note]().Where(sqlb.F("space_id").Eq("acme")).All(ctx, handle)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if scoped[0].ID != "scoped" {
		t.Errorf("ID = %q, want the narrowed reply to have won", scoped[0].ID)
	}

	all, err := sqlb.Query[Note]().All(ctx, handle)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if all[0].ID != "everything" {
		t.Errorf("ID = %q, want the catch-all", all[0].ID)
	}
}
