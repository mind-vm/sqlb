package pgtest

import (
	"context"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
	"github.com/mind-vm/sqlb/shadow"
)

// Issue #24: a declared CHECK never round-tripped.
//
// Postgres stores a check as a parse tree and pg_get_expr renders it back in a
// canonical spelling, so `status <> 'done'` came back as
// `(status <> 'done'::text)` and migrate.Diff — which compares constraint
// definitions as strings — saw two different constraints with the same name.
// Every run proposed dropping and re-adding it, with an ACCESS EXCLUSIVE lock
// attached.

// checked is a schema with one hand-written check, which is the case the whole
// issue is about. The expression is written the way a person writes it: no
// redundant parentheses, no casts.
func checked(expr string) *schema.Registry {
	r := schema.NewRegistry()
	r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Enum("status", "todo", "done").Filterable(),
		schema.Timestamp("completed_at").Nullable(),
	).Check("done_tasks_have_a_completion_time", expr)
	return r
}

const declaredCheck = "status <> 'done' OR completed_at IS NOT NULL"

// The property that was broken: a schema declaring a check, applied and read
// back, diffs to nothing.
func TestADeclaredCheckRoundTripsToNoChange(t *testing.T) {
	t.Parallel()
	db := freshDB(t)
	reg := checked(declaredCheck)

	applySchema(t, db, reg)
	current := importRegistry(t, db)

	// Without normalisation this is where it went wrong, and the assertion
	// below would report a drop and an add.
	unprobed, err := shadow.Normalize(context.Background(), db, reg, shadow.Options{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(unprobed) != 0 {
		t.Fatalf("a check against an existing table could not be probed: %v", unprobed)
	}

	changes, err := migrate.Diff(current, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("a schema diffed against itself produced %d change(s):\n%s",
			len(changes), describe(changes))
	}
}

// The other direction, and the one the design turns on.
//
// The rejected fix was to canonicalise both spellings textually — strip
// parentheses, drop casts. That can make two genuinely different expressions
// compare equal, and a diff that reports "unchanged" about a constraint that
// changed produces no migration at all: a silent wrong answer, where the churn
// it replaces was merely loud. So this asserts that a real edit still shows up.
func TestAChangedCheckIsStillReportedAsChanged(t *testing.T) {
	t.Parallel()
	db := freshDB(t)

	applySchema(t, db, checked(declaredCheck))
	current := importRegistry(t, db)

	// Same constraint name, different meaning: the OR became an AND. A
	// paren-stripping heuristic would still see two different strings here, so
	// to be a real test of the risk the edit has to be one that *normalises*
	// close to the original — hence a change of operator inside the same shape.
	edited := checked("status <> 'done' AND completed_at IS NOT NULL")
	if _, err := shadow.Normalize(context.Background(), db, edited, shadow.Options{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	changes, err := migrate.Diff(current, edited)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatal("changing OR to AND in a check produced no migration, so normalisation " +
			"has made different constraints compare equal — which is the failure the " +
			"textual approach was rejected for")
	}
	if !strings.Contains(describe(changes), "done_tasks_have_a_completion_time") {
		t.Errorf("the change does not name the constraint that moved:\n%s", describe(changes))
	}
}

// Normalising a registry introspect produced must be a no-op, because its
// expressions are already in Postgres's spelling. Without this the function
// could not safely be applied to both sides of a comparison.
func TestNormalizeIsIdempotent(t *testing.T) {
	t.Parallel()
	db := freshDB(t)
	applySchema(t, db, checked(declaredCheck))
	reg := importRegistry(t, db)
	before := checkExprs(reg)
	if len(before) == 0 {
		t.Fatal("introspection found no checks, so this test compares nothing")
	}

	if _, err := shadow.Normalize(context.Background(), db, reg, shadow.Options{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	after := checkExprs(reg)

	for name, want := range before {
		if after[name] != want {
			t.Errorf("normalising an already-normalised check changed it:\n  before: %s\n  after:  %s",
				want, after[name])
		}
	}
}

// A check that cannot be probed — the ordinary case being one that names a
// column the migration is about to add — must be reported and left alone, not
// fail the run. Everything after it must still be normalised, which is what the
// per-probe savepoint is for: Postgres aborts a transaction on any error.
func TestAnUnprobeableCheckIsReportedAndDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	db := freshDB(t)
	applySchema(t, db, checked(declaredCheck))

	reg := checked(declaredCheck)
	// Declared against a column that does not exist in the database yet, which
	// is what adding a column with a check on it looks like at this moment.
	reg.Tables()[0].Check("mentions_a_column_that_is_not_there_yet", "not_a_column > 0")

	unprobed, err := shadow.Normalize(context.Background(), db, reg, shadow.Options{})
	if err != nil {
		t.Fatalf("an unprobeable check failed the whole run: %v", err)
	}
	if len(unprobed) != 1 {
		t.Fatalf("want exactly one unprobeable check, got %v", unprobed)
	}
	if !strings.Contains(unprobed[0], "mentions_a_column_that_is_not_there_yet") {
		t.Errorf("the report does not name the check that could not be probed: %v", unprobed)
	}

	// And the probeable one either side of it still got normalised — the
	// savepoint working. Without it the failed probe poisons the transaction
	// and every later check comes back unprobeable too.
	got := checkExprs(reg)["done_tasks_have_a_completion_time"]
	if got == declaredCheck {
		t.Errorf("the check declared before the failing one was not normalised: %q", got)
	}
	if !strings.Contains(got, "::text") {
		t.Errorf("normalised check does not look like Postgres's spelling: %q", got)
	}
}

func checkExprs(reg *schema.Registry) map[string]string {
	out := map[string]string{}
	for _, t := range reg.Tables() {
		for _, c := range t.Checks() {
			out[c.Name] = c.Expr
		}
	}
	return out
}

// Issue #63: a partial index's predicate is stored the way a CHECK is, and
// arrived as the same complaint from the same direction.
//
// `Where: "latitude IS NOT NULL"` never matched the live index; adding the
// parentheses Postgres itself had added made it match. The diff a consumer saw
// proposed CREATE INDEX for an index that was already there, with DDL that read
// identically to what the database held — and a drift gate that reports a
// difference the author cannot see teaches people to add waivers.

// partial is a schema with one partial index, its predicate written the way a
// person writes it: no redundant parentheses.
func partial(expr string) *schema.Registry {
	r := schema.NewRegistry()
	r.Table("work_packages",
		schema.UUIDv7("id").PrimaryKey(),
		schema.UUIDv7("project_id"),
		schema.Float("latitude").Nullable(),
	).AddIndex(schema.Index{
		Name:    "idx_work_packages_location_by_project",
		Columns: []string{"project_id"},
		Where:   expr,
	})
	return r
}

const declaredPredicate = "latitude IS NOT NULL"

func TestADeclaredPartialIndexRoundTripsToNoChange(t *testing.T) {
	t.Parallel()
	db := freshDB(t)
	reg := partial(declaredPredicate)

	applySchema(t, db, reg)
	current := importRegistry(t, db)

	unprobed, err := shadow.Normalize(context.Background(), db, reg, shadow.Options{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(unprobed) != 0 {
		t.Fatalf("a predicate against an existing table could not be probed: %v", unprobed)
	}

	// The claim the probe rests on: pg_get_expr renders a partial-index
	// predicate and a CHECK expression through the same code, so the CHECK
	// probe is a valid normaliser for a predicate. Asserted rather than
	// assumed — if the two ever diverge, this is where it shows.
	got := reg.Tables()[0].Indexes()
	var normalised string
	for _, idx := range got {
		if idx.Name == "idx_work_packages_location_by_project" {
			normalised = idx.Where
		}
	}
	if normalised == declaredPredicate {
		t.Errorf("the predicate was not normalised at all: %q", normalised)
	}
	for _, idx := range current.Tables()[0].Indexes() {
		if idx.Name == "idx_work_packages_location_by_project" && idx.Where != normalised {
			t.Errorf("the probe and the catalog disagree about the same predicate:\n"+
				"  probed:  %q\n  catalog: %q", normalised, idx.Where)
		}
	}

	changes, err := migrate.Diff(current, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("a schema diffed against itself produced %d change(s):\n%s",
			len(changes), describe(changes))
	}
}

// The direction that matters more, for the same reason as the CHECK case: a
// normalisation that made two different predicates compare equal would produce
// no migration at all, which is a silent wrong answer where the churn it
// replaces was merely loud.
func TestAChangedPartialIndexPredicateIsStillReportedAsChanged(t *testing.T) {
	t.Parallel()
	db := freshDB(t)

	applySchema(t, db, partial(declaredPredicate))
	current := importRegistry(t, db)

	edited := partial("latitude IS NULL")
	if _, err := shadow.Normalize(context.Background(), db, edited, shadow.Options{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	changes, err := migrate.Diff(current, edited)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatal("inverting a partial index's predicate produced no migration, so " +
			"normalisation has made different indexes compare equal")
	}
}
