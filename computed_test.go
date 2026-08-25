package sqlb_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

// A project with three derived fields, one of each tier ADR-0041 names: a
// row-local expression, a correlated subquery, and one whose answer depends on
// who is asking.
type CompProject struct {
	ID        string `db:"id" sqlb:"pk"`
	Name      string `db:"name" sqlb:"search"`
	DueDate   string `db:"due_date" sqlb:"sort"`
	OpenTasks int32  `db:"open_tasks"`

	IsOverdue  bool  `db:"is_overdue" sqlb:"filter,sort"`
	TotalTasks int32 `db:"total_tasks"`
	IsStarred  bool  `db:"is_starred" sqlb:"filter"`
}

func (CompProject) TableName() string { return "projects" }

func (CompProject) ComputedColumns() []sqlb.Computed {
	return []sqlb.Computed{
		{Name: "is_overdue", Expr: "due_date < current_date AND open_tasks > 0"},
		{Name: "total_tasks", Expr: "(SELECT count(*) FROM tasks t WHERE t.project_id = projects.id)"},
		{
			Name:  "is_starred",
			Expr:  "EXISTS (SELECT 1 FROM stars s WHERE s.project_id = projects.id AND s.member_id = ?)",
			Needs: []string{"viewer"},
		},
	}
}

// One declaration reaches the projection, the WHERE and the ORDER BY, because
// all three render through the same function.
func TestComputedRendersInProjectionFilterAndOrder(t *testing.T) {
	sql, _, err := sqlb.Query[CompProject]().
		WithComputed("is_overdue", "total_tasks", "is_starred").
		Bind("viewer", "member-1").
		Where(sqlb.F("is_overdue").Eq(true)).
		OrderBy(sqlb.OrderBy(sqlb.F("is_overdue"))).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, want := range []string{
		// Projected as an expression, aliased back to the column name so the
		// scan can match it to the field.
		`(due_date < current_date AND open_tasks > 0) AS "is_overdue"`,
		`(SELECT count(*) FROM tasks t WHERE t.project_id = projects.id) AS "total_tasks"`,
		`WHERE (due_date < current_date AND open_tasks > 0) = $2`,
		`ORDER BY (due_date < current_date AND open_tasks > 0) ASC`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL is missing %q:\n%s", want, sql)
		}
	}
	// The stored columns still render as columns.
	if !strings.Contains(sql, `"projects"."name"`) && !strings.Contains(sql, `"name"`) {
		t.Errorf("stored columns are missing:\n%s", sql)
	}
}

// A parameterised expression binds once however many times it is rendered —
// the property Near proved worth having, generalised.
func TestComputedBindsOnce(t *testing.T) {
	sql, args, err := sqlb.Query[CompProject]().
		WithComputed("is_starred").
		Bind("viewer", "member-1").
		Where(sqlb.F("is_starred").Eq(true)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if n := strings.Count(sql, "$1"); n != 2 {
		t.Errorf("want the viewer bound once and referenced twice, got %d references:\n%s", n, sql)
	}
	if len(args) != 2 || args[0] != "member-1" {
		t.Errorf("args = %v, want the viewer first and the predicate's value second", args)
	}
}

// An unbound expression would render `member_id = NULL` and be false for every
// row forever, which looks exactly like a working feature. It fails instead.
//
// The query has to ask for the column first: since #92 a computed column is not
// in the default projection, so a query that never mentions is_starred never
// needs the viewer — which is the whole point of that change, and is asserted
// just below.
func TestComputedWithoutItsBindFails(t *testing.T) {
	_, _, err := sqlb.Query[CompProject]().WithComputed("is_starred").SQL()
	if err == nil {
		t.Fatal("want an error when the viewer bind is missing")
	}
	for _, want := range []string{"is_starred", "viewer", "Bind"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Nothing writes an expression: no insert names it, no update assigns it, and
// since #164 no write evaluates one it was not asked for.
func TestComputedIsNotWritten(t *testing.T) {
	sql, _, err := sqlb.InsertRows(&CompProject{ID: "p1", Name: "Apollo"}).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, `INSERT INTO "projects" ("id", "name", "due_date", "open_tasks", "is_overdue"`) {
		t.Errorf("insert writes a computed column:\n%s", sql)
	}
	// A write takes its derived columns the way a read does: only the ones it
	// asked for. Before #164 the same aggregate a read had to name was evaluated
	// by every insert of the table, including the ones that discard it.
	if strings.Contains(sql, "is_overdue") {
		t.Errorf("RETURNING should omit a computed column nothing asked for:\n%s", sql)
	}
	if strings.Contains(sql, "is_starred") {
		t.Errorf("RETURNING should omit a parameterised computed column:\n%s", sql)
	}

	_, _, err = sqlb.UpdateRows[CompProject]().Set("is_overdue", true).Where(sqlb.F("id").Eq("p1")).SQL()
	if err == nil || !strings.Contains(err.Error(), "computed") {
		t.Errorf("assigning a computed column should be refused, got %v", err)
	}
}

// The opt-in half of the same rule: a write that names the column gets it back
// without a second read, which is what ADR-0041 wanted RETURNING for.
func TestWriteComputedIsOptIn(t *testing.T) {
	sql, _, err := sqlb.InsertRows(&CompProject{ID: "p1", Name: "Apollo"}).
		WithComputed("is_overdue").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `(due_date < current_date AND open_tasks > 0) AS "is_overdue"`) {
		t.Errorf("RETURNING should carry the column the insert asked for:\n%s", sql)
	}

	sql, _, err = sqlb.UpdateRows[CompProject]().
		WithComputed("is_overdue").
		Set("name", "Artemis").
		Where(sqlb.F("id").Eq("p1")).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `AS "is_overdue"`) {
		t.Errorf("an update should carry the column it asked for:\n%s", sql)
	}
}

// The three names a write cannot take, each named rather than skipped. The last
// is the one that matters: a parameterised expression has no bind on a write, so
// asking for it is asking for a value no statement here can produce — and the
// alternative to refusing is a field that arrives holding a definite zero.
func TestWriteComputedRefusesWhatItCannotReturn(t *testing.T) {
	tests := []struct {
		name, column, want string
	}{
		{"unknown", "is_overdu", "not a column of"},
		{"stored", "name", "stores that column rather than computing it"},
		{"needs a bind", "is_starred", "nowhere to take a bind from"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := sqlb.InsertRows(&CompProject{ID: "p1"}).WithComputed(tc.column).SQL()
			if err == nil {
				t.Fatalf("WithComputed(%q) was accepted on an insert", tc.column)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("insert error %q does not say %q", err, tc.want)
			}
			_, _, err = sqlb.UpdateRows[CompProject]().
				WithComputed(tc.column).Set("name", "x").Where(sqlb.F("id").Eq("p1")).SQL()
			if err == nil {
				t.Fatalf("WithComputed(%q) was accepted on an update", tc.column)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("update error %q does not say %q", err, tc.want)
			}
		})
	}
}

// A count wraps the query in a subselect, and the inner statement's derived
// columns must not leak into the outer one.
func TestComputedSurvivesCount(t *testing.T) {
	sql, _, err := sqlb.Query[CompProject]().
		Bind("viewer", "member-1").
		Where(sqlb.F("is_starred").Eq(true)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, "EXISTS (SELECT 1 FROM stars") {
		t.Errorf("expected the expression inline:\n%s", sql)
	}
}

// The default projection leaves computed columns out, which is what makes an
// unrelated query of a shared model cheap and possible.
//
// Both halves of #92 are here: the existence check by id must not fail for want
// of a bind it has no business supplying, and it must not attach a correlated
// subquery per derived column to a query that asked for none.
func TestTheDefaultProjectionLeavesComputedColumnsOut(t *testing.T) {
	sql, args, err := sqlb.Query[CompProject]().
		Where(sqlb.F("id").Eq(int64(7))).
		SQL()
	if err != nil {
		t.Fatalf("a query that names no computed column failed: %v", err)
	}
	for _, absent := range []string{"is_starred", "is_overdue", "total_tasks", "SELECT count(*)", "EXISTS"} {
		if strings.Contains(sql, absent) {
			t.Errorf("the default projection carries %q, which nothing asked for:\n%s", absent, sql)
		}
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want only the predicate's value", args)
	}
}

// Opting in is per column, not all-or-nothing: a screen wanting one aggregate
// should not pay for the per-viewer subquery beside it.
func TestWithComputedTakesOnlyWhatIsNamed(t *testing.T) {
	sql, _, err := sqlb.Query[CompProject]().WithComputed("is_overdue").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `AS "is_overdue"`) {
		t.Errorf("the named column is missing:\n%s", sql)
	}
	for _, absent := range []string{"is_starred", "total_tasks"} {
		if strings.Contains(sql, absent) {
			t.Errorf("an unnamed computed column came along: %q\n%s", absent, sql)
		}
	}
}

// A name that is not a computed column is a mistake worth refusing. A silent
// no-op would leave the caller believing a value is on its way when the field
// is about to arrive as the zero value — which is the failure mode this whole
// issue is about, one level up.
func TestWithComputedRefusesNamesThatAreNotComputed(t *testing.T) {
	tests := []struct {
		name, column, want string
	}{
		{"unknown", "is_overdu", "not a column of"},
		{"stored", "name", "stores that column rather than computing it"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := sqlb.Query[CompProject]().WithComputed(tc.column).SQL()
			if err == nil {
				t.Fatalf("WithComputed(%q) was accepted", tc.column)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not say what is wrong (%q missing): %v", tc.want, err)
			}
		})
	}
}

// Filtering on a computed column still works without projecting it: the
// expression is rendered where it is used, and the opt-in governs the
// projection rather than the vocabulary. The bind is still required, because
// the WHERE renders the same expression.
func TestAComputedColumnCanBeFilteredWithoutBeingProjected(t *testing.T) {
	sql, _, err := sqlb.Query[CompProject]().
		Where(sqlb.F("is_overdue").Eq(true)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, "WHERE (due_date < current_date") {
		t.Errorf("the predicate did not render:\n%s", sql)
	}
	if strings.Contains(sql, `AS "is_overdue"`) {
		t.Errorf("filtering on a computed column projected it:\n%s", sql)
	}
}

// Clone has to copy the opt-in, or a base query shared between screens loses
// the columns one of them asked for.
func TestCloneCarriesTheComputedOptIn(t *testing.T) {
	base := sqlb.Query[CompProject]().WithComputed("is_overdue")
	sql, _, err := base.Clone().SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `AS "is_overdue"`) {
		t.Errorf("the clone lost the opt-in:\n%s", sql)
	}
}
