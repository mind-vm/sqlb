package computed_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/computed"
	"github.com/mind-vm/sqlb/filter"
	"github.com/mind-vm/sqlb/migrate"
)

// The declared form produces the same statement the hand-written one does. That
// is the claim to check first: this is a way of saying the same SQL, not a
// different and slower thing that happens to be more convenient.
func TestDeclaredViewMatchesTheHandWrittenProjection(t *testing.T) {
	q, args, err := computed.DeclaredView(42).SQL()
	if err != nil {
		t.Fatalf("compiling the declared view: %v", err)
	}
	for _, want := range []string{
		`(due_date IS NOT NULL AND due_date < current_date AND open_tasks > 0) AS "is_overdue"`,
		`(completed_tasks * 100 / NULLIF(total_tasks, 0)) AS "progress"`,
		`s.member_id = $1)) AS "is_starred"`,
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

// And this is what the hand-written form cannot do at all: the derived values
// are in the filter grammar, so a REST client can ask for them by name.
func TestDeclaredValuesReachTheFilterGrammar(t *testing.T) {
	values, err := url.ParseQuery("is_overdue=true&sort=-progress&select=id,is_starred")
	if err != nil {
		t.Fatal(err)
	}
	q, err := filter.Parse(values, filter.Options{
		Model:    sqlb.ModelOf[computed.DeclaredProject](),
		Computed: computed.DeclaredValues,
	})
	if err != nil {
		t.Fatalf("parsing a request naming the derived columns: %v", err)
	}

	sql, _, err := filter.Apply(computed.DeclaredView(42), q).SQL()
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}
	for _, want := range []string{
		"WHERE (due_date IS NOT NULL AND due_date < current_date AND open_tasks > 0) = $",
		`ORDER BY (completed_tasks * 100 / NULLIF(total_tasks, 0)) DESC`,
		`s.member_id = $1)) AS "is_starred"`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL is missing %q:\n%s", want, sql)
		}
	}
	// ?select narrowed the response, so the columns nobody asked for are not
	// paid for — including the arithmetic.
	if strings.Contains(sql, `"name"`) {
		t.Errorf("?select should have narrowed the projection:\n%s", sql)
	}
}

// A volatile expression cannot be sorted on, and the refusal is at declaration
// time. is_overdue reads current_date, so a keyset paging on it would compare
// this page's boundary against next page's value.
func TestDeclaredVolatileColumnIsNotSortable(t *testing.T) {
	values, err := url.ParseQuery("sort=is_overdue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[computed.DeclaredProject]()}); err == nil {
		t.Error("sorting on the volatile derived column should be refused")
	}
}

// The schema is valid, and the DDL it produces is the stored half only: three
// declarations, no columns, no migration.
func TestDeclaredSchemaEmitsNoDDLForTheExpressions(t *testing.T) {
	if err := computed.Declared.Validate(); err != nil {
		t.Fatalf("the declared schema is invalid: %v", err)
	}

	changes, err := migrate.Diff(nil, computed.Declared)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	var ddl strings.Builder
	for _, c := range changes {
		ddl.WriteString(c.Up + "\n")
	}
	for _, absent := range []string{"is_overdue", "progress", "is_starred"} {
		if strings.Contains(ddl.String(), absent) {
			t.Errorf("%s reached the DDL:\n%s", absent, ddl.String())
		}
	}
	// The stored columns are still created, including the ones techniques (1)
	// and (2) maintain — a computed column is an addition to those, not a
	// replacement for them.
	for _, want := range []string{"total_tasks", "completed_tasks", "open_tasks"} {
		if !strings.Contains(ddl.String(), want) {
			t.Errorf("%s is missing from the DDL:\n%s", want, ddl.String())
		}
	}
}

// The query that made the opt-in necessary, and the measurement of what it now
// costs: nothing.
//
// Before #92 this could not be written against a model declaring a Needs
// column. The projection carried is_starred, is_starred wanted the "viewer"
// bind, and an existence check by id failed before it reached the database —
// for a value it had not asked for and had no way to decline.
func TestAnExistenceCheckPaysForNoDerivedValue(t *testing.T) {
	sql, args, err := computed.Exists(42).SQL()
	if err != nil {
		t.Fatalf("an existence check on a model with computed columns failed: %v", err)
	}
	for _, absent := range []string{"is_starred", "is_overdue", "progress", "EXISTS", "NULLIF"} {
		if strings.Contains(sql, absent) {
			t.Errorf("the existence check carries %q, which it never asked for:\n%s", absent, sql)
		}
	}
	if len(args) != 1 || args[0] != int64(42) {
		t.Errorf("args = %v, want only the id", args)
	}
}

// And the resource-level half: a request to an endpoint that does not select a
// computed column cannot reach it by name either. Unreachable rather than
// merely unprojected, because a filter on a correlated subquery costs what the
// projection would have.
func TestAResourceThatDoesNotSelectAColumnCannotBeAskedForIt(t *testing.T) {
	values, err := url.ParseQuery("is_overdue=true")
	if err != nil {
		t.Fatal(err)
	}
	// The same model, mounted without the derived values.
	_, err = filter.Parse(values, filter.Options{
		Model: sqlb.ModelOf[computed.DeclaredProject](),
	})
	if err == nil {
		t.Fatal("a resource that does not select is_overdue accepted a filter on it")
	}
	if !strings.Contains(err.Error(), "is_overdue") {
		t.Errorf("the rejection does not name the parameter: %v", err)
	}
	// And it is not advertised as one of the columns that would have worked —
	// naming it there would point a caller at a column every request for it is
	// about to be refused for.
	if _, allowed, found := strings.Cut(err.Error(), "allowed"); found && strings.Contains(allowed, "is_overdue") {
		t.Errorf("the unreachable column is listed among the allowed ones: %v", err)
	}
}
