package computed_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/computed"
)

// TestDerivedProjection is the payoff: one statement carries the row and the
// three values the row does not store, and the per-viewer one arrives as a bind
// rather than as text spliced into SQL.
func TestDerivedProjection(t *testing.T) {
	q, args, err := computed.View(42).SQL()
	if err != nil {
		t.Fatalf("compiling the view: %v", err)
	}

	for _, want := range []string{
		`"id", "org_id", "name", "due_date", "total_tasks", "completed_tasks", "open_tasks"`,
		`AS "is_overdue"`,
		`(completed_tasks * 100 / NULLIF(total_tasks, 0)) AS "progress"`,
		`s.member_id = $1) AS "is_starred"`,
		`FROM "projects"`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query is missing %q:\n%s", want, q)
		}
	}

	if len(args) != 1 || args[0] != int64(42) {
		t.Errorf("viewer should be the only bind, got %v", args)
	}
}

// TestDerivedIsNotSplicedText is the property that makes technique (3) safe to
// build a per-viewer field on: the value reaches Postgres as a parameter, so it
// is not SQL and cannot become SQL.
func TestDerivedIsNotSplicedText(t *testing.T) {
	q, args, err := computed.View(7).SQL()
	if err != nil {
		t.Fatalf("compiling the view: %v", err)
	}
	if strings.Contains(q, "= 7") {
		t.Errorf("viewer was spliced into the SQL text:\n%s", q)
	}
	if len(args) != 1 {
		t.Fatalf("want one bind, got %d", len(args))
	}
}

// TestBindsRenumberAcrossSources is the reason RawSel takes `?` rather than
// $N. The projection's bind, the predicate's bind and the search term are
// written at three call sites that cannot see each other, and only the compiler
// knows what position each ends up in.
func TestBindsRenumberAcrossSources(t *testing.T) {
	q, args, err := computed.StarredBy(computed.View(42), 42).
		Where(sqlb.F("name").Contains("road")).
		SQL()
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}
	if len(args) != 3 {
		t.Fatalf("want three binds (projection, predicate, search), got %d: %v", len(args), args)
	}
	for i, want := range []string{"$1", "$2", "$3"} {
		if !strings.Contains(q, want) {
			t.Errorf("bind %d (%s) is missing from:\n%s", i+1, want, q)
		}
	}
	// The projection is compiled first, so its bind is $1 — which is exactly
	// the fact a hand-written $N would have to guess.
	if !strings.Contains(q, "s.member_id = $1)") {
		t.Errorf("expected the projection's bind to be $1:\n%s", q)
	}
}

// TestGeneratedColumnNeedsNoSpecialCase is the point of techniques (1) and (2):
// a column Postgres maintains is filterable and sortable through the ordinary
// path, with no raw SQL at the call site and no gap in the generated clients.
func TestGeneratedColumnNeedsNoSpecialCase(t *testing.T) {
	q, args, err := sqlb.Query[computed.Project]().
		Where(sqlb.F("open_tasks").Gt(0)).
		OrderBy(sqlb.F("open_tasks").Desc()).
		SQL()
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}
	if !strings.Contains(q, `WHERE "open_tasks" > $1`) {
		t.Errorf("expected a plain predicate on the generated column:\n%s", q)
	}
	if !strings.Contains(q, `ORDER BY "open_tasks" DESC`) {
		t.Errorf("expected a plain sort on the generated column:\n%s", q)
	}
	if len(args) != 1 {
		t.Errorf("want one bind, got %v", args)
	}
}

// TestDerivedColumnsAreNotWritten guards the ReadOnly claim in the package doc
// from the sqlb side: an INSERT names the columns a caller may set, and the
// three maintained ones hold their defaults rather than being handed a zero.
func TestDerivedColumnsAreNotWritten(t *testing.T) {
	q, _, err := sqlb.InsertRows(&computed.Project{OrgID: 1, Name: "roadmap"}).SQL()
	if err != nil {
		t.Fatalf("compiling the insert: %v", err)
	}
	written, _, _ := strings.Cut(q, " VALUES ")
	for _, col := range []string{"id", "total_tasks", "completed_tasks", "open_tasks"} {
		if strings.Contains(written, `"`+col+`"`) {
			t.Errorf("insert writes %s, which the database fills:\n%s", col, q)
		}
	}
	// RETURNING still reads them back, so the caller sees what Postgres
	// computed without a second query.
	if !strings.Contains(q, `RETURNING`) || !strings.Contains(q, `"open_tasks"`) {
		t.Errorf("expected open_tasks in RETURNING:\n%s", q)
	}
}

// TestViewScansTheProjection checks the two types line up: every field of
// ProjectView is named by the projection, so none of them scans as a zero.
//
// Collect enforces this at runtime — it scans exactly, and names the fields no
// result column filled. Asserting it here means a column added to Project and
// forgotten in ProjectView fails in a unit test instead of on the first
// request, which is the whole reason the check is cheap enough to write.
func TestViewScansTheProjection(t *testing.T) {
	q, _, err := computed.View(1).SQL()
	if err != nil {
		t.Fatalf("compiling the view: %v", err)
	}
	// Cut on the table, not on " FROM ": the is_starred subquery has a FROM of
	// its own, and splitting on the first one truncates the projection.
	projection, _, ok := strings.Cut(strings.TrimPrefix(q, "SELECT "), ` FROM "projects"`)
	if !ok {
		t.Fatalf("cannot find the projection in:\n%s", q)
	}

	for _, col := range sqlb.ModelOf[computed.ProjectView]().Columns {
		if !strings.Contains(projection, `"`+col.Name+`"`) {
			t.Errorf("ProjectView.%s (db:%q) is not in the projection, so it would scan as a zero:\n%s",
				col.Field, col.Name, projection)
		}
	}
}
